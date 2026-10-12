package identity

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net/mail"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"parley/pkg/db"
	"parley/services/api/internal/delivery"
)

// RecoveryConfig controls email/SMS contact methods and password reset. Both
// channels are off by default; with a channel off, its endpoints answer
// ErrFeatureDisabled and registration rejects that contact type.
type RecoveryConfig struct {
	EmailEnabled bool
	SMSEnabled   bool
	Email        delivery.Sender // required if EmailEnabled
	SMS          delivery.Sender // required if SMSEnabled

	AppName         string        // used in message text; default "Parley"
	CodeTTL         time.Duration // default 15m
	MaxAttempts     int           // wrong guesses allowed per code; default 5
	ResendInterval  time.Duration // minimum gap between codes; default 60s
	MaxCodesPerHour int           // per user and purpose; default 5

	// SynchronousDelivery sends inline instead of in the background. For
	// tests only: inline sending makes response time depend on whether the
	// account exists.
	SynchronousDelivery bool
}

func (c *RecoveryConfig) setDefaults() {
	if c.AppName == "" {
		c.AppName = "Parley"
	}
	if c.CodeTTL == 0 {
		c.CodeTTL = 15 * time.Minute
	}
	if c.MaxAttempts == 0 {
		c.MaxAttempts = 5
	}
	if c.ResendInterval == 0 {
		c.ResendInterval = time.Minute
	}
	if c.MaxCodesPerHour == 0 {
		c.MaxCodesPerHour = 5
	}
}

var (
	ErrFeatureDisabled = errors.New("this feature is not enabled on this server")
	ErrInvalidEmail    = errors.New("enter a valid email address")
	ErrInvalidPhone    = errors.New("enter a phone number in international format, e.g. +15551234567")
	ErrInvalidCode     = errors.New("invalid or expired code")
	ErrContactInUse    = errors.New("that email address or phone number is already used by another account")
	ErrTooManyCodes    = errors.New("too many codes requested; wait a minute and try again")
	ErrInvalidChannel  = errors.New(`channel must be "email" or "sms"`)
)

const (
	channelEmail = "email"
	channelSMS   = "sms"

	purposeVerifyEmail   = "verify_email"
	purposeVerifyPhone   = "verify_phone"
	purposeResetPassword = "reset_password"
)

// Features tells clients which optional flows this server supports.
type Features struct {
	EmailRecovery bool `json:"email_recovery"`
	SMSRecovery   bool `json:"sms_recovery"`
}

func (s *Service) Features() Features {
	return Features{EmailRecovery: s.cfg.Recovery.EmailEnabled, SMSRecovery: s.cfg.Recovery.SMSEnabled}
}

func (s *Service) channelEnabled(channel string) (bool, error) {
	switch channel {
	case channelEmail:
		return s.cfg.Recovery.EmailEnabled, nil
	case channelSMS:
		return s.cfg.Recovery.SMSEnabled, nil
	}
	return false, ErrInvalidChannel
}

// Contact validation ------------------------------------------------------------

func normalizeEmail(s string) (string, error) {
	s = strings.TrimSpace(s)
	addr, err := mail.ParseAddress(s)
	if err != nil || addr.Address != s || len(s) > 254 {
		return "", ErrInvalidEmail
	}
	at := strings.LastIndex(s, "@")
	if at < 1 || !strings.Contains(s[at+1:], ".") {
		return "", ErrInvalidEmail
	}
	return strings.ToLower(s), nil
}

var e164 = regexp.MustCompile(`^\+[1-9][0-9]{7,14}$`)

func normalizePhone(s string) (string, error) {
	s = strings.NewReplacer(" ", "", "-", "", ".", "", "(", "", ")", "").Replace(strings.TrimSpace(s))
	if !e164.MatchString(s) {
		return "", ErrInvalidPhone
	}
	return s, nil
}

// validateContacts checks optional registration contacts against config.
func (s *Service) validateContacts(email, phone string) (string, string, error) {
	var err error
	if strings.TrimSpace(email) != "" {
		if !s.cfg.Recovery.EmailEnabled {
			return "", "", ErrFeatureDisabled
		}
		if email, err = normalizeEmail(email); err != nil {
			return "", "", err
		}
	}
	if strings.TrimSpace(phone) != "" {
		if !s.cfg.Recovery.SMSEnabled {
			return "", "", ErrFeatureDisabled
		}
		if phone, err = normalizePhone(phone); err != nil {
			return "", "", err
		}
	}
	return email, phone, nil
}

func normalizeContact(channel, value string) (string, error) {
	if channel == channelEmail {
		return normalizeEmail(value)
	}
	return normalizePhone(value)
}

// contactColumns returns the users columns holding a channel's value and
// verification time. Values are constants, never user input.
func contactColumns(channel string) (value, verifiedAt, purpose string) {
	if channel == channelEmail {
		return "email", "email_verified_at", purposeVerifyEmail
	}
	return "phone", "phone_verified_at", purposeVerifyPhone
}

// Contact management (logged in) --------------------------------------------------

// SetContact sets (or replaces) the account's email or phone, unverified, and
// sends a verification code to it. Requires the current password.
func (s *Service) SetContact(ctx context.Context, userID uuid.UUID, channel, value, password, clientIP string) error {
	enabled, err := s.channelEnabled(channel)
	if err != nil {
		return err
	}
	if !enabled {
		return ErrFeatureDisabled
	}
	value, err = normalizeContact(channel, value)
	if err != nil {
		return err
	}
	u, hash, err := s.loadForPasswordCheck(ctx, userID)
	if err != nil {
		return err
	}
	if err := s.checkPassword(ctx, u, hash, password, clientIP); err != nil {
		return err
	}
	col, verifiedCol, purpose := contactColumns(channel)

	var code pendingCode
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := s.checkCodeRate(ctx, tx, u.ID, purpose); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, fmt.Sprintf(`UPDATE users SET %s = $2, %s = NULL WHERE id = $1`, col, verifiedCol),
			u.ID, value); err != nil {
			return err
		}
		var err error
		code, err = s.issueCode(ctx, tx, u, purpose, channel, value)
		return err
	})
	if err != nil {
		return err
	}
	s.deliver(ctx, code)
	return nil
}

// VerifyContact confirms a contact method with the code sent to it.
func (s *Service) VerifyContact(ctx context.Context, userID uuid.UUID, channel, code string) (User, error) {
	enabled, err := s.channelEnabled(channel)
	if err != nil {
		return User{}, err
	}
	if !enabled {
		return User{}, ErrFeatureDisabled
	}
	col, verifiedCol, purpose := contactColumns(channel)

	var (
		u   User
		bad bool
	)
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		dest, ok, err := s.consumeCode(ctx, tx, userID, purpose, code)
		if err != nil {
			return err
		}
		if !ok {
			bad = true
			return nil // commit the attempt counter
		}
		// The code must be for the value currently on the account.
		tag, err := tx.Exec(ctx, fmt.Sprintf(`UPDATE users SET %s = now() WHERE id = $1 AND %s = $2`, verifiedCol, col),
			userID, dest)
		if db.IsUniqueViolation(err, "") {
			return ErrContactInUse
		}
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrInvalidCode
		}
		u, err = getUser(ctx, tx, userID)
		return err
	})
	if err != nil {
		return User{}, err
	}
	if bad {
		return User{}, ErrInvalidCode
	}
	return u, nil
}

// RemoveContact deletes the account's email or phone. Requires the password.
func (s *Service) RemoveContact(ctx context.Context, userID uuid.UUID, channel, password, clientIP string) (User, error) {
	if _, err := s.channelEnabled(channel); err != nil {
		return User{}, err
	}
	u, hash, err := s.loadForPasswordCheck(ctx, userID)
	if err != nil {
		return User{}, err
	}
	if err := s.checkPassword(ctx, u, hash, password, clientIP); err != nil {
		return User{}, err
	}
	col, verifiedCol, _ := contactColumns(channel)
	if _, err := s.pool.Exec(ctx, fmt.Sprintf(`UPDATE users SET %s = NULL, %s = NULL WHERE id = $1`, col, verifiedCol),
		userID); err != nil {
		return User{}, err
	}
	return s.GetUser(ctx, userID)
}

// Password reset (logged out) -----------------------------------------------------

// RequestPasswordReset sends a reset code to the account's verified email or
// phone. It reports success whether or not the account exists, has that
// contact verified, or is rate limited, so it can't be used to probe accounts.
func (s *Service) RequestPasswordReset(ctx context.Context, username, channel string) error {
	enabled, err := s.channelEnabled(channel)
	if err != nil {
		return err
	}
	if !enabled {
		return ErrFeatureDisabled
	}
	col, verifiedCol, _ := contactColumns(channel)

	var code *pendingCode
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var (
			u    User
			dest string
		)
		err := tx.QueryRow(ctx, fmt.Sprintf(`SELECT %s, u.%s FROM users u WHERE u.username = $1 AND u.%s IS NOT NULL`,
			userCols("u"), col, verifiedCol), NormalizeUsername(username)).Scan(userDest(&u, &dest)...)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := s.checkCodeRate(ctx, tx, u.ID, purposeResetPassword); err != nil {
			slog.InfoContext(ctx, "password reset rate limited", "user_id", u.ID)
			return nil
		}
		c, err := s.issueCode(ctx, tx, u, purposeResetPassword, channel, dest)
		code = &c
		return err
	})
	if err != nil {
		return err
	}
	if code != nil {
		s.deliver(ctx, *code)
	}
	return nil
}

// ConfirmPasswordReset sets a new password using a reset code. On success all
// sessions are revoked and any login lockout is cleared.
func (s *Service) ConfirmPasswordReset(ctx context.Context, username, code, newPassword string) error {
	if !s.cfg.Recovery.EmailEnabled && !s.cfg.Recovery.SMSEnabled {
		return ErrFeatureDisabled
	}
	if !validPasswordLength(newPassword) {
		return ErrInvalidPassword
	}
	var bad bool
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var u User
		err := tx.QueryRow(ctx, `SELECT `+userCols("u")+` FROM users u WHERE u.username = $1`,
			NormalizeUsername(username)).Scan(userDest(&u)...)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrInvalidCode
		}
		if err != nil {
			return err
		}
		_, ok, err := s.consumeCode(ctx, tx, u.ID, purposeResetPassword, code)
		if err != nil {
			return err
		}
		if !ok {
			bad = true
			return nil // commit the attempt counter
		}
		// Checked only after the code, so it can't reveal whether the account
		// exists. A weak password rolls back, leaving the code usable.
		if err := checkPasswordStrength(newPassword, u.Username, u.DisplayName); err != nil {
			return err
		}
		hash, err := hashPassword(newPassword, s.cfg.Hash)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE one_time_codes SET consumed_at = now()
			WHERE user_id = $1 AND purpose = $2 AND consumed_at IS NULL`, u.ID, purposeResetPassword); err != nil {
			return err
		}
		return setPassword(ctx, tx, u, hash)
	})
	if err != nil {
		return err
	}
	if bad {
		return ErrInvalidCode
	}
	return nil
}

// Codes ------------------------------------------------------------------------

type pendingCode struct {
	channel string
	msg     delivery.Message
}

// checkCodeRate enforces the resend interval and hourly cap per user and purpose.
func (s *Service) checkCodeRate(ctx context.Context, q db.Querier, userID uuid.UUID, purpose string) error {
	c := s.cfg.Recovery
	now := s.now()
	var (
		lastHour int
		latest   *time.Time
	)
	if err := q.QueryRow(ctx, `SELECT count(*), max(created_at) FROM one_time_codes
		WHERE user_id = $1 AND purpose = $2 AND created_at > $3`,
		userID, purpose, now.Add(-time.Hour)).Scan(&lastHour, &latest); err != nil {
		return err
	}
	if lastHour >= c.MaxCodesPerHour || (latest != nil && now.Sub(*latest) < c.ResendInterval) {
		return ErrTooManyCodes
	}
	return nil
}

// issueCode stores a new 6-digit code (superseding earlier unused ones for the
// same purpose) and returns the message to deliver once the transaction commits.
func (s *Service) issueCode(ctx context.Context, tx pgx.Tx, u User, purpose, channel, dest string) (pendingCode, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		return pendingCode{}, err
	}
	code := fmt.Sprintf("%06d", n.Int64())
	id := uuid.New()
	now := s.now()
	if _, err := tx.Exec(ctx, `UPDATE one_time_codes SET consumed_at = $3
		WHERE user_id = $1 AND purpose = $2 AND consumed_at IS NULL`, u.ID, purpose, now); err != nil {
		return pendingCode{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO one_time_codes
		(id, user_id, purpose, channel, destination, code_hash, expires_at, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		id, u.ID, purpose, channel, dest, hashCode(id, code), now.Add(s.cfg.Recovery.CodeTTL), now); err != nil {
		return pendingCode{}, err
	}
	return pendingCode{channel: channel, msg: s.codeMessage(u, purpose, channel, dest, code)}, nil
}

// consumeCode checks code against the user's newest live code for purpose.
// ok=false means wrong, expired or exhausted (attempts are counted; the caller
// must commit). On success the code is marked used and its destination returned.
func (s *Service) consumeCode(ctx context.Context, tx pgx.Tx, userID uuid.UUID, purpose, code string) (dest string, ok bool, err error) {
	var (
		id       uuid.UUID
		hash     []byte
		attempts int
	)
	err = tx.QueryRow(ctx, `SELECT id, code_hash, attempts, destination FROM one_time_codes
		WHERE user_id = $1 AND purpose = $2 AND consumed_at IS NULL AND expires_at > $3
		ORDER BY created_at DESC LIMIT 1 FOR UPDATE`, userID, purpose, s.now(),
	).Scan(&id, &hash, &attempts, &dest)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if attempts >= s.cfg.Recovery.MaxAttempts {
		return "", false, nil
	}
	if subtle.ConstantTimeCompare(hashCode(id, strings.TrimSpace(code)), hash) != 1 {
		_, err := tx.Exec(ctx, `UPDATE one_time_codes SET attempts = attempts + 1 WHERE id = $1`, id)
		return "", false, err
	}
	_, err = tx.Exec(ctx, `UPDATE one_time_codes SET consumed_at = $2 WHERE id = $1`, id, s.now())
	return dest, err == nil, err
}

func hashCode(id uuid.UUID, code string) []byte {
	h := sha256.Sum256([]byte(id.String() + ":" + code))
	return h[:]
}

func (s *Service) codeMessage(u User, purpose, channel, dest, code string) delivery.Message {
	app, mins := s.cfg.Recovery.AppName, int(s.cfg.Recovery.CodeTTL.Minutes())
	var subject, body string
	switch purpose {
	case purposeResetPassword:
		subject = fmt.Sprintf("Your %s password reset code", app)
		body = fmt.Sprintf("Your %s password reset code for @%s is %s. It expires in %d minutes.\n\n"+
			"If you didn't ask to reset your password, ignore this message. Your password has not been changed.",
			app, u.Username, code, mins)
		if channel == channelSMS {
			body = fmt.Sprintf("%s password reset code: %s (expires in %d min). Don't share it with anyone.", app, code, mins)
		}
	default:
		subject = fmt.Sprintf("Confirm your %s %s", app, map[string]string{channelEmail: "email address", channelSMS: "phone number"}[channel])
		body = fmt.Sprintf("Your %s verification code for @%s is %s. It expires in %d minutes.\n\n"+
			"If you didn't add this address to a %s account, ignore this message.", app, u.Username, code, mins, app)
		if channel == channelSMS {
			body = fmt.Sprintf("%s verification code: %s (expires in %d min). Don't share it with anyone.", app, code, mins)
		}
	}
	return delivery.Message{To: dest, Subject: subject, Body: body}
}

// deliver sends a code. By default this happens in the background so response
// times don't reveal whether a message was sent.
func (s *Service) deliver(ctx context.Context, c pendingCode) {
	sender := s.cfg.Recovery.Email
	if c.channel == channelSMS {
		sender = s.cfg.Recovery.SMS
	}
	send := func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		if err := sender.Send(ctx, c.msg); err != nil {
			slog.ErrorContext(ctx, "code delivery failed", "channel", c.channel, "err", err)
		}
	}
	if s.cfg.Recovery.SynchronousDelivery {
		send()
		return
	}
	go send()
}
