package identity

import (
	"context"
	"crypto/rand"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"parley/pkg/auth"
	"parley/services/api/internal/sealbox"
	"parley/services/api/internal/totp"
)

const (
	recoveryCodes   = 10
	mfaChallengeTTL = 5 * time.Minute
)

var (
	ErrInvalidMFACode    = errors.New("that code isn't right; check your authenticator app and try again")
	ErrMFAChallenge      = errors.New("your login took too long; enter your password again")
	ErrMFAAlreadyEnabled = errors.New("two-factor authentication is already set up")
	ErrMFANotStarted     = errors.New("start two-factor setup first")
)

// MFAEnrollment is what an authenticator app needs.
type MFAEnrollment struct {
	Secret     string `json:"secret"`
	OTPAuthURI string `json:"otpauth_uri"`
}

// Enrolled is a full session plus recovery codes, which are shown only once.
type Enrolled struct {
	Session
	RecoveryCodes []string `json:"recovery_codes"`
}

// StartEnrollment creates a new TOTP secret, unconfirmed until ConfirmEnrollment.
func (s *Service) StartEnrollment(ctx context.Context, userID uuid.UUID) (MFAEnrollment, error) {
	secret, err := totp.NewSecret()
	if err != nil {
		return MFAEnrollment{}, err
	}
	sealed, err := s.seal(userID, secret)
	if err != nil {
		return MFAEnrollment{}, err
	}
	var username string
	err = s.pool.QueryRow(ctx, `UPDATE users SET mfa_pending_secret = $2 WHERE id = $1 AND mfa_enabled_at IS NULL RETURNING username`,
		userID, sealed).Scan(&username)
	if errors.Is(err, pgx.ErrNoRows) {
		return MFAEnrollment{}, ErrMFAAlreadyEnabled
	}
	if err != nil {
		return MFAEnrollment{}, err
	}
	return MFAEnrollment{
		Secret:     totp.Encoding.EncodeToString(secret),
		OTPAuthURI: totp.URI(s.cfg().Recovery.AppName, username, secret),
	}, nil
}

// ConfirmEnrollment turns MFA on once a code proves the app is set up. Every
// other session is revoked, because those were only ever password-authenticated;
// the caller gets a new full session and its recovery codes.
func (s *Service) ConfirmEnrollment(ctx context.Context, userID uuid.UUID, code, clientIP string) (Enrolled, error) {
	u, err := s.GetUser(ctx, userID)
	if err != nil {
		return Enrolled{}, err
	}
	keys := s.mfaKeys(u.Username, clientIP)
	if err := s.checkLocked(ctx, keys); err != nil {
		return Enrolled{}, err
	}
	var (
		out Enrolled
		bad bool
	)
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		bad = false
		var sealed []byte
		var enabled *time.Time
		if err := tx.QueryRow(ctx, `SELECT mfa_pending_secret, mfa_enabled_at FROM users WHERE id = $1 FOR UPDATE`, userID).
			Scan(&sealed, &enabled); err != nil {
			return err
		}
		if enabled != nil {
			return ErrMFAAlreadyEnabled
		}
		if sealed == nil {
			return ErrMFANotStarted
		}
		secret, err := s.open(userID, sealed)
		if err != nil {
			return err
		}
		step, ok := totp.Match(secret, code, s.now(), 0)
		if !ok {
			bad = true
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE users SET mfa_secret = mfa_pending_secret, mfa_pending_secret = NULL,
			mfa_enabled_at = $2, mfa_last_step = $3 WHERE id = $1`, userID, s.now(), step); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE refresh_tokens SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL`, userID); err != nil {
			return err
		}
		codes, err := replaceRecoveryCodes(ctx, tx, userID)
		if err != nil {
			return err
		}
		if u, err = getUser(ctx, tx, userID); err != nil {
			return err
		}
		sess, err := s.newSession(ctx, tx, u, uuid.New())
		out = Enrolled{Session: sess, RecoveryCodes: codes}
		return err
	})
	if err != nil {
		return Enrolled{}, err
	}
	if bad {
		return Enrolled{}, s.mfaFailed(ctx, keys)
	}
	if err := s.clearFailures(ctx, keys); err != nil {
		return Enrolled{}, err
	}
	// Enrollment is the moment a stolen password could be turned into a
	// takeover, so the owner hears about it.
	s.notify(ctx, u, "Two-factor authentication was set up",
		"An authenticator app was just added to your account. If this wasn't you, contact your server administrator.")
	return out, nil
}

// challenge returns the second step of a login for an account with MFA.
func (s *Service) challenge(u User) (Session, error) {
	token, exp, err := s.cfg().Signer.SignChallenge(auth.Claims{UserID: u.ID, Username: u.Username}, mfaChallengeTTL)
	if err != nil {
		return Session{}, err
	}
	return Session{MFARequired: true, MFAToken: token, MFAExpiresAt: exp}, nil
}

// CompleteMFALogin exchanges a login challenge and an authenticator or
// recovery code for a session. Failures share a lockout separate from the
// password's, so a password reset can't be used to reset the guess count.
func (s *Service) CompleteMFALogin(ctx context.Context, challenge, code, clientIP string) (Session, error) {
	claims, err := s.verifier().VerifyChallenge(challenge)
	if err != nil {
		return Session{}, ErrMFAChallenge
	}
	keys := s.mfaKeys(claims.Username, clientIP)
	if err := s.checkLocked(ctx, keys); err != nil {
		return Session{}, err
	}
	var (
		sess Session
		bad  bool
	)
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		ok, err := s.consumeSecondFactor(ctx, tx, claims.UserID, code)
		if err != nil {
			return err
		}
		if !ok {
			bad = true
			return nil
		}
		u, err := getUser(ctx, tx, claims.UserID)
		if err != nil {
			return err
		}
		sess, err = s.newSession(ctx, tx, u, uuid.New())
		return err
	})
	if err != nil {
		return Session{}, err
	}
	if bad {
		return Session{}, s.mfaFailed(ctx, keys)
	}
	if err := s.clearFailures(ctx, keys); err != nil {
		return Session{}, err
	}
	return sess, nil
}

// ResetMFA removes the authenticator, given the password and a current or
// recovery code, and signs out everywhere. The returned session is limited to
// enrolling a new one.
func (s *Service) ResetMFA(ctx context.Context, userID uuid.UUID, password, code, clientIP string) (Session, error) {
	u, hash, err := s.loadForPasswordCheck(ctx, userID)
	if err != nil {
		return Session{}, err
	}
	if err := s.checkPassword(ctx, u, hash, password, clientIP); err != nil {
		return Session{}, err
	}
	keys := s.mfaKeys(u.Username, clientIP)
	if err := s.checkLocked(ctx, keys); err != nil {
		return Session{}, err
	}
	var (
		sess Session
		bad  bool
	)
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		ok, err := s.consumeSecondFactor(ctx, tx, userID, code)
		if err != nil {
			return err
		}
		if !ok {
			bad = true
			return nil
		}
		if err := clearMFA(ctx, tx, userID); err != nil {
			return err
		}
		if u, err = getUser(ctx, tx, userID); err != nil {
			return err
		}
		sess, err = s.newSession(ctx, tx, u, uuid.New())
		return err
	})
	if err != nil {
		return Session{}, err
	}
	if bad {
		return Session{}, s.mfaFailed(ctx, keys)
	}
	s.notify(ctx, u, "Two-factor authentication was removed",
		"The authenticator on your account was removed and all devices were signed out. If this wasn't you, contact your server administrator.")
	return sess, nil
}

// AdminResetMFA clears a user's MFA for an operator (`api mfa-reset`), for
// someone who has lost both their authenticator and recovery codes. Their next
// login has to enroll again.
func (s *Service) AdminResetMFA(ctx context.Context, username string) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var id uuid.UUID
		err := tx.QueryRow(ctx, `SELECT id FROM users WHERE username = $1 FOR UPDATE`, NormalizeUsername(username)).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrUserNotFound
		}
		if err != nil {
			return err
		}
		return clearMFA(ctx, tx, id)
	})
}

func clearMFA(ctx context.Context, tx pgx.Tx, userID uuid.UUID) error {
	if _, err := tx.Exec(ctx, `UPDATE users SET mfa_secret = NULL, mfa_pending_secret = NULL, mfa_enabled_at = NULL, mfa_last_step = 0
		WHERE id = $1`, userID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM mfa_recovery_codes WHERE user_id = $1`, userID); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE refresh_tokens SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL`, userID)
	return err
}

// consumeSecondFactor checks a TOTP code (never accepted twice) or spends a
// recovery code. The user row stays locked, so concurrent attempts can't both
// use one code.
func (s *Service) consumeSecondFactor(ctx context.Context, tx pgx.Tx, userID uuid.UUID, code string) (bool, error) {
	var (
		sealed   []byte
		lastStep int64
	)
	err := tx.QueryRow(ctx, `SELECT mfa_secret, mfa_last_step FROM users WHERE id = $1 AND mfa_enabled_at IS NOT NULL FOR UPDATE`, userID).
		Scan(&sealed, &lastStep)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	code = strings.TrimSpace(code)
	if len(code) == 6 {
		secret, err := s.open(userID, sealed)
		if err != nil {
			return false, err
		}
		step, ok := totp.Match(secret, code, s.now(), lastStep)
		if !ok {
			return false, nil
		}
		_, err = tx.Exec(ctx, `UPDATE users SET mfa_last_step = $2 WHERE id = $1`, userID, step)
		return err == nil, err
	}
	tag, err := tx.Exec(ctx, `UPDATE mfa_recovery_codes SET used_at = $3 WHERE user_id = $1 AND code_hash = $2 AND used_at IS NULL`,
		userID, hashToken(normalizeRecoveryCode(code)), s.now())
	return err == nil && tag.RowsAffected() == 1, err
}

func replaceRecoveryCodes(ctx context.Context, tx pgx.Tx, userID uuid.UUID) ([]string, error) {
	if _, err := tx.Exec(ctx, `DELETE FROM mfa_recovery_codes WHERE user_id = $1`, userID); err != nil {
		return nil, err
	}
	codes := make([]string, recoveryCodes)
	for i := range codes {
		raw := make([]byte, 10) // 80 bits; a plain hash is enough at this entropy
		if _, err := rand.Read(raw); err != nil {
			return nil, err
		}
		c := totp.Encoding.EncodeToString(raw)
		codes[i] = c[:4] + "-" + c[4:8] + "-" + c[8:12] + "-" + c[12:]
		if _, err := tx.Exec(ctx, `INSERT INTO mfa_recovery_codes (user_id, code_hash) VALUES ($1, $2)`,
			userID, hashToken(normalizeRecoveryCode(codes[i]))); err != nil {
			return nil, err
		}
	}
	return codes, nil
}

func normalizeRecoveryCode(c string) string {
	return strings.ToUpper(strings.NewReplacer("-", "", " ", "").Replace(c))
}

func (s *Service) mfaKeys(username, clientIP string) []throttleKey {
	// ':' can't appear in a username, so these can't collide with password keys.
	return s.throttleKeys("mfa:"+username, clientIP)
}

func (s *Service) mfaFailed(ctx context.Context, keys []throttleKey) error {
	if err := s.recordFailure(ctx, keys); err != nil {
		return err
	}
	return ErrInvalidMFACode
}

func (s *Service) verifier() *auth.Verifier { return s.cfg().Verifier }

// GenerateTOTP returns the code for a base32 secret at t, as an authenticator
// app would. Used by tests.
func GenerateTOTP(secret string, t time.Time) (string, error) { return totp.Generate(secret, t) }

// Secret sealing: a database dump alone doesn't reveal TOTP secrets. The user
// ID is authenticated, so a sealed secret can't be moved to another account.

func (s *Service) seal(userID uuid.UUID, secret []byte) ([]byte, error) {
	return sealbox.Seal(s.cfg().MFAKey, userID[:], secret)
}

func (s *Service) open(userID uuid.UUID, sealed []byte) ([]byte, error) {
	return sealbox.Open(s.cfg().MFAKey, userID[:], sealed)
}
