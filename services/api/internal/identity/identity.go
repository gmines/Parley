// Package identity owns accounts, passwords, sessions (access + refresh
// tokens), login lockout and account recovery.
package identity

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"parley/pkg/auth"
	"parley/pkg/db"
)

var (
	ErrInvalidUsername    = errors.New("username must be 2-32 characters: a-z, 0-9, '_' or '.'")
	ErrInvalidPassword    = errors.New("password must be 8-128 characters")
	ErrInvalidDisplayName = errors.New("display name must be 1-32 printable characters")
	ErrUsernameTaken      = errors.New("username is taken")
	ErrBadCredentials     = errors.New("invalid username or password")
	ErrInvalidRefresh     = errors.New("invalid or expired refresh token")
	ErrUserNotFound       = errors.New("user not found")
)

// User is an account as its owner sees it. Contact fields are only ever
// returned to the account itself.
type User struct {
	ID            uuid.UUID `json:"id"`
	Username      string    `json:"username"`
	DisplayName   string    `json:"display_name"`
	CreatedAt     time.Time `json:"created_at"`
	Email         string    `json:"email,omitempty"`
	EmailVerified bool      `json:"email_verified,omitempty"`
	Phone         string    `json:"phone,omitempty"`
	PhoneVerified bool      `json:"phone_verified,omitempty"`
}

// userCols selects the User fields from a users table aliased as p.
func userCols(p string) string {
	return fmt.Sprintf(`%[1]s.id, %[1]s.username, %[1]s.display_name, %[1]s.created_at,
		coalesce(%[1]s.email, ''), %[1]s.email_verified_at IS NOT NULL,
		coalesce(%[1]s.phone, ''), %[1]s.phone_verified_at IS NOT NULL`, p)
}

// userDest returns scan destinations matching userCols, followed by extra.
func userDest(u *User, extra ...any) []any {
	return append([]any{&u.ID, &u.Username, &u.DisplayName, &u.CreatedAt,
		&u.Email, &u.EmailVerified, &u.Phone, &u.PhoneVerified}, extra...)
}

// Session is returned by Register, Login, Refresh and ChangePassword.
type Session struct {
	User             User      `json:"user"`
	AccessToken      string    `json:"access_token"`
	AccessExpiresAt  time.Time `json:"access_expires_at"`
	RefreshToken     string    `json:"refresh_token"`
	RefreshExpiresAt time.Time `json:"refresh_expires_at"`
}

type Config struct {
	Signer     *auth.Signer
	RefreshTTL time.Duration
	Hash       HashParams
	Lockout    LockoutConfig
	Recovery   RecoveryConfig
}

type Service struct {
	pool      *pgxpool.Pool
	cfg       Config
	dummyHash string // compared against when a username doesn't exist, to equalize timing
	now       func() time.Time
}

func NewService(pool *pgxpool.Pool, cfg Config) (*Service, error) {
	dummy, err := hashPassword("parley-dummy-password", cfg.Hash)
	if err != nil {
		return nil, err
	}
	cfg.Recovery.setDefaults()
	return &Service{pool: pool, cfg: cfg, dummyHash: dummy, now: time.Now}, nil
}

func NormalizeUsername(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

func validUsername(u string) bool {
	if len(u) < 2 || len(u) > 32 {
		return false
	}
	for _, r := range u {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '.') {
			return false
		}
	}
	return true
}

func validDisplayName(s string) bool {
	n := utf8.RuneCountInString(s)
	if n < 1 || n > 32 {
		return false
	}
	for _, r := range s {
		if !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}

func validPasswordLength(p string) bool {
	n := utf8.RuneCountInString(p)
	return n >= 8 && n <= 128
}

// Registration is a new account. Email and Phone are optional recovery
// contacts, accepted only when that channel is enabled; each must then be
// verified with a code before it can be used.
type Registration struct {
	Username    string
	Password    string
	DisplayName string
	Email       string
	Phone       string
}

func (s *Service) Register(ctx context.Context, in Registration) (Session, error) {
	username := NormalizeUsername(in.Username)
	if !validUsername(username) {
		return Session{}, ErrInvalidUsername
	}
	if !validPasswordLength(in.Password) {
		return Session{}, ErrInvalidPassword
	}
	displayName := strings.TrimSpace(in.DisplayName)
	if displayName == "" {
		displayName = username
	}
	if !validDisplayName(displayName) {
		return Session{}, ErrInvalidDisplayName
	}
	email, phone, err := s.validateContacts(in.Email, in.Phone)
	if err != nil {
		return Session{}, err
	}
	if err := checkPasswordStrength(in.Password, username, displayName); err != nil {
		return Session{}, err
	}
	hash, err := hashPassword(in.Password, s.cfg.Hash)
	if err != nil {
		return Session{}, err
	}

	var (
		sess  Session
		codes []pendingCode
	)
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		codes = nil
		var u User
		err := tx.QueryRow(ctx, `
			INSERT INTO users (username, display_name, password_hash, email, phone) VALUES ($1, $2, $3, $4, $5)
			RETURNING `+userCols("users"),
			username, displayName, hash, nullable(email), nullable(phone),
		).Scan(userDest(&u)...)
		if db.IsUniqueViolation(err, "users_username_key") {
			return ErrUsernameTaken
		}
		if err != nil {
			return fmt.Errorf("insert user: %w", err)
		}
		if email != "" {
			c, err := s.issueCode(ctx, tx, u, purposeVerifyEmail, channelEmail, email)
			if err != nil {
				return err
			}
			codes = append(codes, c)
		}
		if phone != "" {
			c, err := s.issueCode(ctx, tx, u, purposeVerifyPhone, channelSMS, phone)
			if err != nil {
				return err
			}
			codes = append(codes, c)
		}
		sess, err = s.newSession(ctx, tx, u, uuid.New())
		return err
	})
	if err != nil {
		return Session{}, err
	}
	for _, c := range codes {
		s.deliver(ctx, c)
	}
	return sess, nil
}

// Login checks credentials, subject to the lockout policy. clientIP is the
// caller's address, used for per-IP lockout.
func (s *Service) Login(ctx context.Context, username, password, clientIP string) (Session, error) {
	username = NormalizeUsername(username)
	keys := s.throttleKeys(username, clientIP)
	// Check before verifying, so a locked-out guesser learns nothing.
	if err := s.checkLocked(ctx, keys); err != nil {
		return Session{}, err
	}

	var (
		u    User
		hash string
	)
	err := s.pool.QueryRow(ctx, `SELECT `+userCols("u")+`, u.password_hash FROM users u WHERE u.username = $1`,
		username).Scan(userDest(&u, &hash)...)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// Unknown usernames are verified and counted like real ones, so neither
		// timing nor lockout reveals whether an account exists.
		verifyPassword(password, s.dummyHash)
		return Session{}, s.loginFailed(ctx, keys)
	case err != nil:
		return Session{}, fmt.Errorf("load user: %w", err)
	}
	ok, err := verifyPassword(password, hash)
	if err != nil {
		return Session{}, err
	}
	if !ok {
		return Session{}, s.loginFailed(ctx, keys)
	}
	if err := s.clearFailures(ctx, keys); err != nil {
		return Session{}, err
	}
	return s.newSession(ctx, s.pool, u, uuid.New())
}

// loginFailed records the failure and returns ErrBadCredentials, or a
// *LockedError if this failure tripped the lockout.
func (s *Service) loginFailed(ctx context.Context, keys []throttleKey) error {
	if err := s.recordFailure(ctx, keys); err != nil {
		return err
	}
	return ErrBadCredentials
}

// Refresh exchanges a refresh token for a new session. Each refresh token is
// single-use; reusing one revokes every token in its family.
func (s *Service) Refresh(ctx context.Context, refreshToken string) (Session, error) {
	var (
		sess   Session
		reused bool
	)
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var (
			id, family    uuid.UUID
			u             User
			expiresAt     time.Time
			used, revoked *time.Time
		)
		err := tx.QueryRow(ctx, `
			SELECT t.id, t.family_id, t.expires_at, t.used_at, t.revoked_at, `+userCols("u")+`
			FROM refresh_tokens t JOIN users u ON u.id = t.user_id
			WHERE t.token_hash = $1
			FOR UPDATE OF t`, hashToken(refreshToken),
		).Scan(append([]any{&id, &family, &expiresAt, &used, &revoked}, userDest(&u)...)...)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrInvalidRefresh
		}
		if err != nil {
			return err
		}
		if revoked != nil || s.now().After(expiresAt) {
			return ErrInvalidRefresh
		}
		if used != nil {
			// Commit the revocation, then report failure.
			reused = true
			_, err := tx.Exec(ctx, `UPDATE refresh_tokens SET revoked_at = now()
				WHERE family_id = $1 AND revoked_at IS NULL`, family)
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE refresh_tokens SET used_at = now() WHERE id = $1`, id); err != nil {
			return err
		}
		sess, err = s.newSession(ctx, tx, u, family)
		return err
	})
	if err != nil {
		return Session{}, err
	}
	if reused {
		return Session{}, ErrInvalidRefresh
	}
	return sess, nil
}

// Logout revokes the refresh token's whole family. Unknown tokens are ignored.
func (s *Service) Logout(ctx context.Context, refreshToken string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE refresh_tokens SET revoked_at = now()
		WHERE revoked_at IS NULL
		  AND family_id = (SELECT family_id FROM refresh_tokens WHERE token_hash = $1)`,
		hashToken(refreshToken))
	return err
}

func (s *Service) GetUser(ctx context.Context, id uuid.UUID) (User, error) {
	return getUser(ctx, s.pool, id)
}

func getUser(ctx context.Context, q db.Querier, id uuid.UUID) (User, error) {
	var u User
	err := q.QueryRow(ctx, `SELECT `+userCols("u")+` FROM users u WHERE u.id = $1`, id).Scan(userDest(&u)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrUserNotFound
	}
	return u, err
}

// ChangePassword sets a new password for a logged-in user. It requires the
// current password (counted by the lockout policy), signs the user out
// everywhere and returns a fresh session for the caller.
func (s *Service) ChangePassword(ctx context.Context, userID uuid.UUID, current, next, clientIP string) (Session, error) {
	u, hash, err := s.loadForPasswordCheck(ctx, userID)
	if err != nil {
		return Session{}, err
	}
	if err := s.checkPassword(ctx, u, hash, current, clientIP); err != nil {
		return Session{}, err
	}
	if !validPasswordLength(next) {
		return Session{}, ErrInvalidPassword
	}
	if err := checkPasswordStrength(next, u.Username, u.DisplayName); err != nil {
		return Session{}, err
	}
	newHash, err := hashPassword(next, s.cfg.Hash)
	if err != nil {
		return Session{}, err
	}
	var sess Session
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := setPassword(ctx, tx, u, newHash); err != nil {
			return err
		}
		var err error
		sess, err = s.newSession(ctx, tx, u, uuid.New())
		return err
	})
	return sess, err
}

func (s *Service) loadForPasswordCheck(ctx context.Context, userID uuid.UUID) (User, string, error) {
	var (
		u    User
		hash string
	)
	err := s.pool.QueryRow(ctx, `SELECT `+userCols("u")+`, u.password_hash FROM users u WHERE u.id = $1`,
		userID).Scan(userDest(&u, &hash)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, "", ErrUserNotFound
	}
	return u, hash, err
}

// checkPassword re-verifies a logged-in user's password (for sensitive
// changes). Failures count towards the same lockout as logins, so a stolen
// session can't be used to guess the password.
func (s *Service) checkPassword(ctx context.Context, u User, hash, password, clientIP string) error {
	keys := s.throttleKeys(u.Username, clientIP)
	if err := s.checkLocked(ctx, keys); err != nil {
		return err
	}
	ok, err := verifyPassword(password, hash)
	if err != nil {
		return err
	}
	if !ok {
		return s.loginFailed(ctx, keys)
	}
	return nil
}

// setPassword stores a new hash, signs the user out of every session and
// clears any lockout.
func setPassword(ctx context.Context, tx pgx.Tx, u User, hash string) error {
	if _, err := tx.Exec(ctx, `UPDATE users SET password_hash = $2 WHERE id = $1`, u.ID, hash); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE refresh_tokens SET revoked_at = now()
		WHERE user_id = $1 AND revoked_at IS NULL`, u.ID); err != nil {
		return err
	}
	return clearAllFailures(ctx, tx, u.Username)
}

func (s *Service) newSession(ctx context.Context, q db.Querier, u User, family uuid.UUID) (Session, error) {
	access, accessExp, err := s.cfg.Signer.Sign(auth.Claims{UserID: u.ID, Username: u.Username})
	if err != nil {
		return Session{}, err
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return Session{}, err
	}
	refresh := base64.RawURLEncoding.EncodeToString(raw)
	refreshExp := s.now().Add(s.cfg.RefreshTTL)
	if _, err := q.Exec(ctx, `
		INSERT INTO refresh_tokens (family_id, user_id, token_hash, expires_at) VALUES ($1, $2, $3, $4)`,
		family, u.ID, hashToken(refresh), refreshExp); err != nil {
		return Session{}, fmt.Errorf("insert refresh token: %w", err)
	}
	return Session{
		User:             u,
		AccessToken:      access,
		AccessExpiresAt:  accessExp,
		RefreshToken:     refresh,
		RefreshExpiresAt: refreshExp,
	}, nil
}

func hashToken(t string) []byte {
	h := sha256.Sum256([]byte(t))
	return h[:]
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
