// Package admin is Parley's operator app: user management and server settings,
// served on a loopback-only address and reached through a tunnel
// (kubectl port-forward). See README.md in the Helm chart.
package admin

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"parley/pkg/db"
	"parley/services/api/internal/identity"
	"parley/services/api/internal/sealbox"
	"parley/services/api/internal/totp"
)

const (
	SessionIdle     = 30 * time.Minute
	SessionAbsolute = 12 * time.Hour
	// Sign-in failures per admin username before a lock, and the lock's length.
	maxLoginFailures = 5
	loginWindow      = 15 * time.Minute
	loginLock        = 15 * time.Minute
	totpIssuer       = "Parley Admin"
)

var (
	ErrBadLogin      = errors.New("wrong username, password or code")
	ErrLoginLocked   = errors.New("too many failed sign-ins; try again in 15 minutes")
	ErrNoSession     = errors.New("signed out")
	ErrAdminExists   = errors.New("an admin with that username already exists")
	ErrAdminNotFound = errors.New("no admin with that username")
	ErrInvalidAdmin  = errors.New("admin usernames are 2-32 characters: a-z, 0-9, '_' and '.'")
	ErrWrongPassword = errors.New("your current password or code is wrong")
	ErrSamePassword  = errors.New("choose a password different from the current one")
)

// Admin is an operator account.
type Admin struct {
	ID        uuid.UUID `json:"id"`
	Username  string    `json:"username"`
	CreatedAt time.Time `json:"created_at"`
	Disabled  bool      `json:"disabled"`
}

// Credentials are shown once, when an admin is created or reset.
type Credentials struct {
	Username   string
	Password   string
	Secret     string // base32, for typing into an authenticator app
	OTPAuthURI string
}

// Accounts manages admin accounts, sessions and the audit log.
type Accounts struct {
	pool      *pgxpool.Pool
	key       []byte // seals admin TOTP secrets; derived from PARLEY_MFA_KEY
	hash      identity.HashParams
	dummyHash string
	now       func() time.Time
}

func NewAccounts(pool *pgxpool.Pool, mfaKey []byte, hash identity.HashParams) (*Accounts, error) {
	k, err := sealbox.Derive(mfaKey, "admin totp")
	if err != nil {
		return nil, err
	}
	dummy, err := identity.HashPassword("parley-admin-dummy", hash)
	if err != nil {
		return nil, err
	}
	return &Accounts{pool: pool, key: k, hash: hash, dummyHash: dummy, now: time.Now}, nil
}

func validAdminName(u string) bool {
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

// randomPassword is 120 bits, grouped for reading aloud or typing.
func randomPassword() (string, error) {
	raw := make([]byte, 15)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	s := strings.ToLower(totp.Encoding.EncodeToString(raw))
	return s[:6] + "-" + s[6:12] + "-" + s[12:18] + "-" + s[18:], nil
}

// newCredentials makes a password and TOTP secret and returns them with
// what to store.
func (a *Accounts) newCredentials(id uuid.UUID, username string) (Credentials, string, []byte, error) {
	pw, err := randomPassword()
	if err != nil {
		return Credentials{}, "", nil, err
	}
	hash, err := identity.HashPassword(pw, a.hash)
	if err != nil {
		return Credentials{}, "", nil, err
	}
	secret, err := totp.NewSecret()
	if err != nil {
		return Credentials{}, "", nil, err
	}
	sealed, err := sealbox.Seal(a.key, id[:], secret)
	if err != nil {
		return Credentials{}, "", nil, err
	}
	return Credentials{
		Username:   username,
		Password:   pw,
		Secret:     totp.Encoding.EncodeToString(secret),
		OTPAuthURI: totp.URI(totpIssuer, username, secret),
	}, hash, sealed, nil
}

// Create adds an admin with a generated password and authenticator secret.
func (a *Accounts) Create(ctx context.Context, username string) (Credentials, error) {
	username = strings.ToLower(strings.TrimSpace(username))
	if !validAdminName(username) {
		return Credentials{}, ErrInvalidAdmin
	}
	id := uuid.New()
	creds, hash, sealed, err := a.newCredentials(id, username)
	if err != nil {
		return Credentials{}, err
	}
	_, err = a.pool.Exec(ctx, `INSERT INTO admin_users (id, username, password_hash, totp_secret) VALUES ($1, $2, $3, $4)`,
		id, username, hash, sealed)
	if db.IsUniqueViolation(err, "admin_users_username_key") {
		return Credentials{}, ErrAdminExists
	}
	if err != nil {
		return Credentials{}, err
	}
	a.Audit(ctx, "cli", "admin.create", username, nil, "")
	return creds, nil
}

// ResetCredentials replaces an admin's password and authenticator and signs
// them out, for a lost password or phone.
func (a *Accounts) ResetCredentials(ctx context.Context, username string) (Credentials, error) {
	username = strings.ToLower(strings.TrimSpace(username))
	var creds Credentials
	err := pgx.BeginFunc(ctx, a.pool, func(tx pgx.Tx) error {
		var id uuid.UUID
		err := tx.QueryRow(ctx, `SELECT id FROM admin_users WHERE username = $1 FOR UPDATE`, username).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrAdminNotFound
		}
		if err != nil {
			return err
		}
		c, hash, sealed, err := a.newCredentials(id, username)
		if err != nil {
			return err
		}
		creds = c
		if _, err := tx.Exec(ctx, `UPDATE admin_users SET password_hash = $2, totp_secret = $3, totp_last_step = 0 WHERE id = $1`,
			id, hash, sealed); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM login_throttle WHERE key = $1`, throttleKey(username)); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `DELETE FROM admin_sessions WHERE admin_id = $1`, id)
		return err
	})
	if err == nil {
		a.Audit(ctx, "cli", "admin.reset_credentials", username, nil, "")
	}
	return creds, err
}

// SetDisabled turns an admin account off (signing it out) or back on.
func (a *Accounts) SetDisabled(ctx context.Context, username string, disabled bool) error {
	username = strings.ToLower(strings.TrimSpace(username))
	err := pgx.BeginFunc(ctx, a.pool, func(tx pgx.Tx) error {
		var id uuid.UUID
		err := tx.QueryRow(ctx, `UPDATE admin_users SET disabled_at = CASE WHEN $2 THEN coalesce(disabled_at, now()) END
			WHERE username = $1 RETURNING id`, username, disabled).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrAdminNotFound
		}
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `DELETE FROM admin_sessions WHERE admin_id = $1`, id)
		return err
	})
	if err == nil {
		action := "admin.enable"
		if disabled {
			action = "admin.disable"
		}
		a.Audit(ctx, "cli", action, username, nil, "")
	}
	return err
}

// List returns every admin account.
func (a *Accounts) List(ctx context.Context) ([]Admin, error) {
	rows, err := a.pool.Query(ctx, `SELECT id, username, created_at, disabled_at IS NOT NULL FROM admin_users ORDER BY username`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Admin, error) {
		var x Admin
		err := r.Scan(&x.ID, &x.Username, &x.CreatedAt, &x.Disabled)
		return x, err
	})
}

// Sign-in ---------------------------------------------------------------------

// throttleKey shares the users' login_throttle table; "adm:" can't collide
// with its other keys, which start "a:", "aip:" or "mfa:".
func throttleKey(username string) string { return "adm:" + username }

func (a *Accounts) locked(ctx context.Context, username string) (bool, error) {
	var locked bool
	err := a.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM login_throttle WHERE key = $1 AND locked_until > $2)`,
		throttleKey(username), a.now()).Scan(&locked)
	return locked, err
}

func (a *Accounts) recordFailure(ctx context.Context, username string) error {
	now := a.now()
	_, err := a.pool.Exec(ctx, `
		INSERT INTO login_throttle AS t (key, failures, window_start) VALUES ($1, 1, $2)
		ON CONFLICT (key) DO UPDATE SET
			failures     = CASE WHEN t.window_start < $3 THEN 1 ELSE t.failures + 1 END,
			window_start = CASE WHEN t.window_start < $3 THEN $2 ELSE t.window_start END,
			locked_until = CASE WHEN t.window_start >= $3 AND t.failures + 1 >= $4 THEN $5::timestamptz ELSE t.locked_until END`,
		throttleKey(username), now, now.Add(-loginWindow), maxLoginFailures, now.Add(loginLock))
	return err
}

// Login checks a password and authenticator code together and starts a
// session, returning its cookie token. Every failure looks the same.
func (a *Accounts) Login(ctx context.Context, username, password, code, ip string) (string, Admin, error) {
	username = strings.ToLower(strings.TrimSpace(username))
	if len(username) > 64 {
		username = username[:64]
	}
	if locked, err := a.locked(ctx, username); err != nil {
		return "", Admin{}, err
	} else if locked {
		a.Audit(ctx, username, "login.locked", "", nil, ip)
		return "", Admin{}, ErrLoginLocked
	}
	fail := func(reason string) (string, Admin, error) {
		if err := a.recordFailure(ctx, username); err != nil {
			return "", Admin{}, err
		}
		a.Audit(ctx, username, "login.failed", "", map[string]any{"reason": reason}, ip)
		return "", Admin{}, ErrBadLogin
	}

	var (
		adm      Admin
		hash     string
		sealed   []byte
		lastStep int64
	)
	err := a.pool.QueryRow(ctx, `SELECT id, username, created_at, disabled_at IS NOT NULL, password_hash, totp_secret, totp_last_step
		FROM admin_users WHERE username = $1`, username).
		Scan(&adm.ID, &adm.Username, &adm.CreatedAt, &adm.Disabled, &hash, &sealed, &lastStep)
	if errors.Is(err, pgx.ErrNoRows) {
		identity.VerifyPassword(password, a.dummyHash) // same timing as a real account
		return fail("unknown username")
	}
	if err != nil {
		return "", Admin{}, err
	}
	ok, err := identity.VerifyPassword(password, hash)
	if err != nil {
		return "", Admin{}, err
	}
	if !ok {
		return fail("password")
	}
	secret, err := sealbox.Open(a.key, adm.ID[:], sealed)
	if err != nil {
		return "", Admin{}, err
	}
	step, ok := totp.Match(secret, code, a.now(), lastStep)
	if !ok {
		return fail("code")
	}
	if adm.Disabled {
		return fail("disabled")
	}

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", Admin{}, err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	now := a.now()
	err = pgx.BeginFunc(ctx, a.pool, func(tx pgx.Tx) error {
		// The step check and update are one statement, so a code can't be
		// used by two concurrent logins.
		tag, err := tx.Exec(ctx, `UPDATE admin_users SET totp_last_step = $2 WHERE id = $1 AND totp_last_step < $2`, adm.ID, step)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrBadLogin
		}
		if _, err := tx.Exec(ctx, `DELETE FROM login_throttle WHERE key = $1`, throttleKey(username)); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM admin_sessions WHERE expires_at < $1 OR last_seen_at < $2`, now, now.Add(-SessionIdle)); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO admin_sessions (token_hash, admin_id, created_at, last_seen_at, expires_at, ip)
			VALUES ($1, $2, $3, $3, $4, $5)`, hashToken(token), adm.ID, now, now.Add(SessionAbsolute), ip)
		return err
	})
	if errors.Is(err, ErrBadLogin) {
		return fail("code reused")
	}
	if err != nil {
		return "", Admin{}, err
	}
	a.Audit(ctx, adm.Username, "login", "", nil, ip)
	return token, adm, nil
}

func hashToken(t string) []byte {
	h := sha256.Sum256([]byte(t))
	return h[:]
}

// Session returns the admin behind a cookie token and extends its idle timer.
func (a *Accounts) Session(ctx context.Context, token string) (Admin, error) {
	if token == "" {
		return Admin{}, ErrNoSession
	}
	now := a.now()
	var adm Admin
	err := a.pool.QueryRow(ctx, `UPDATE admin_sessions s SET last_seen_at = $2
		FROM admin_users u
		WHERE s.token_hash = $1 AND u.id = s.admin_id AND u.disabled_at IS NULL
		  AND s.expires_at > $2 AND s.last_seen_at > $3
		RETURNING u.id, u.username, u.created_at, false`, hashToken(token), now, now.Add(-SessionIdle)).
		Scan(&adm.ID, &adm.Username, &adm.CreatedAt, &adm.Disabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return Admin{}, ErrNoSession
	}
	return adm, err
}

// Logout ends a session.
func (a *Accounts) Logout(ctx context.Context, token string) error {
	_, err := a.pool.Exec(ctx, `DELETE FROM admin_sessions WHERE token_hash = $1`, hashToken(token))
	return err
}

// ChangePassword sets a new password, given the current one and a code, and
// ends the admin's other sessions.
func (a *Accounts) ChangePassword(ctx context.Context, adm Admin, keepToken, current, code, next, ip string) error {
	if locked, err := a.locked(ctx, adm.Username); err != nil {
		return err
	} else if locked {
		return ErrLoginLocked
	}
	var (
		hash     string
		sealed   []byte
		lastStep int64
	)
	if err := a.pool.QueryRow(ctx, `SELECT password_hash, totp_secret, totp_last_step FROM admin_users WHERE id = $1`, adm.ID).
		Scan(&hash, &sealed, &lastStep); err != nil {
		return err
	}
	ok, err := identity.VerifyPassword(current, hash)
	if err != nil {
		return err
	}
	secret, err := sealbox.Open(a.key, adm.ID[:], sealed)
	if err != nil {
		return err
	}
	step, codeOK := totp.Match(secret, code, a.now(), lastStep)
	if !ok || !codeOK {
		if err := a.recordFailure(ctx, adm.Username); err != nil {
			return err
		}
		a.Audit(ctx, adm.Username, "password.change_failed", "", nil, ip)
		return ErrWrongPassword
	}
	if current == next {
		return ErrSamePassword
	}
	if err := identity.CheckPasswordStrength(next, adm.Username, "admin"); err != nil {
		return err
	}
	newHash, err := identity.HashPassword(next, a.hash)
	if err != nil {
		return err
	}
	err = pgx.BeginFunc(ctx, a.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE admin_users SET password_hash = $2, totp_last_step = greatest(totp_last_step, $3) WHERE id = $1`,
			adm.ID, newHash, step); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `DELETE FROM admin_sessions WHERE admin_id = $1 AND token_hash <> $2`, adm.ID, hashToken(keepToken))
		return err
	})
	if err == nil {
		a.Audit(ctx, adm.Username, "password.change", "", nil, ip)
	}
	return err
}

// Audit log ---------------------------------------------------------------------

// Entry is one audit log record.
type Entry struct {
	ID     int64           `json:"id"`
	At     time.Time       `json:"at"`
	Admin  string          `json:"admin"`
	Action string          `json:"action"`
	Target string          `json:"target"`
	Detail json.RawMessage `json:"detail"`
	IP     string          `json:"ip"`
}

// Audit records an action that has already happened, so a failure to write
// it is logged rather than returned.
func (a *Accounts) Audit(ctx context.Context, admin, action, target string, detail map[string]any, ip string) {
	if detail == nil {
		detail = map[string]any{}
	}
	raw, _ := json.Marshal(detail)
	if _, err := a.pool.Exec(context.WithoutCancel(ctx), `INSERT INTO admin_audit_log (admin, action, target, detail, ip) VALUES ($1, $2, $3, $4, $5)`,
		admin, action, target, raw, ip); err != nil {
		slog.ErrorContext(ctx, "admin audit log write failed", "admin", admin, "action", action, "target", target, "err", err)
	}
}

// AuditLog returns entries newest first, older than before (0: from the newest).
func (a *Accounts) AuditLog(ctx context.Context, before int64, limit int) ([]Entry, error) {
	if before <= 0 {
		before = 1<<63 - 1
	}
	rows, err := a.pool.Query(ctx, `SELECT id, at, admin, action, target, detail, ip FROM admin_audit_log
		WHERE id < $1 ORDER BY id DESC LIMIT $2`, before, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Entry, error) {
		var e Entry
		err := r.Scan(&e.ID, &e.At, &e.Admin, &e.Action, &e.Target, &e.Detail, &e.IP)
		return e, err
	})
}
