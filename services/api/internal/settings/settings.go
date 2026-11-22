// Package settings stores the server settings an operator can change while
// Parley runs (from the admin app), and layers them over the deployment's
// environment configuration.
//
// Each group (lockout, sessions, email, sms) is either absent, meaning the
// environment's value applies, or a complete override. The "mfa" group lives
// only in the database (a missing row means MFA is required); identity reads
// it directly in SQL so every User it returns is current.
//
// Secrets (SMTP password, Twilio auth token) are sealed with a key derived
// from PARLEY_MFA_KEY and never returned once saved.
package settings

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/mail"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"parley/services/api/internal/delivery"
	"parley/services/api/internal/identity"
	"parley/services/api/internal/sealbox"
)

const (
	KeyMFA      = "mfa"
	KeyLockout  = "lockout"
	KeySessions = "sessions"
	KeyEmail    = "email"
	KeySMS      = "sms"
)

// MFA is the server-wide default for accounts whose policy is "inherit".
type MFA struct {
	Require bool `json:"require"`
}

// Lockout mirrors identity.LockoutConfig, with durations as strings ("5m").
type Lockout struct {
	MaxFailures        int    `json:"max_failures"`
	Duration           string `json:"duration"`
	Window             string `json:"window"`
	Scope              string `json:"scope"`
	AccountMaxFailures int    `json:"account_max_failures"`
}

// Sessions sets token lifetimes for sessions issued or refreshed from now on.
type Sessions struct {
	AccessTTL  string `json:"access_ttl"`
	RefreshTTL string `json:"refresh_ttl"`
}

// Email is outgoing mail for sign-up, reset and notices. Mode is "off" or "smtp".
type Email struct {
	Mode     string `json:"mode"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Username string `json:"username"`
	From     string `json:"from"`
	TLS      string `json:"tls"` // "starttls" or "implicit"
	// Password is write-only: set it to change the stored one, leave it empty
	// to keep it.
	Password       string `json:"password,omitempty"`
	PasswordSealed []byte `json:"password_sealed,omitempty"`
}

// SMS is outgoing text messages. Mode is "off" or "twilio".
type SMS struct {
	Mode       string `json:"mode"`
	AccountSID string `json:"account_sid"`
	From       string `json:"from"`
	// AuthToken is write-only, like Email.Password.
	AuthToken       string `json:"auth_token,omitempty"`
	AuthTokenSealed []byte `json:"auth_token_sealed,omitempty"`
}

// Meta records who last changed a group.
type Meta struct {
	UpdatedAt time.Time `json:"updated_at"`
	UpdatedBy string    `json:"updated_by"`
}

// Snapshot is everything stored. A nil group uses the deployment's value.
type Snapshot struct {
	MFA      MFA
	Lockout  *Lockout
	Sessions *Sessions
	Email    *Email
	SMS      *SMS
	Meta     map[string]Meta
}

type Store struct {
	pool *pgxpool.Pool
	key  []byte
}

// NewStore takes PARLEY_MFA_KEY, from which the settings key is derived.
func NewStore(pool *pgxpool.Pool, mfaKey []byte) (*Store, error) {
	k, err := sealbox.Derive(mfaKey, "server settings")
	if err != nil {
		return nil, err
	}
	return &Store{pool: pool, key: k}, nil
}

// Load reads every stored group.
func (s *Store) Load(ctx context.Context) (Snapshot, error) {
	snap := Snapshot{MFA: MFA{Require: true}, Meta: map[string]Meta{}}
	rows, err := s.pool.Query(ctx, `SELECT key, value, updated_at, updated_by FROM server_settings`)
	if err != nil {
		return snap, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			key string
			raw []byte
			m   Meta
		)
		if err := rows.Scan(&key, &raw, &m.UpdatedAt, &m.UpdatedBy); err != nil {
			return snap, err
		}
		snap.Meta[key] = m
		var dst any
		switch key {
		case KeyMFA:
			dst = &snap.MFA
		case KeyLockout:
			snap.Lockout = &Lockout{}
			dst = snap.Lockout
		case KeySessions:
			snap.Sessions = &Sessions{}
			dst = snap.Sessions
		case KeyEmail:
			snap.Email = &Email{}
			dst = snap.Email
		case KeySMS:
			snap.SMS = &SMS{}
			dst = snap.SMS
		default:
			continue
		}
		if err := json.Unmarshal(raw, dst); err != nil {
			return snap, fmt.Errorf("setting %q: %w", key, err)
		}
	}
	return snap, rows.Err()
}

// Fingerprint changes whenever any setting does; cheap enough to poll.
func (s *Store) Fingerprint(ctx context.Context) (string, error) {
	var (
		n      int
		latest *time.Time
	)
	if err := s.pool.QueryRow(ctx, `SELECT count(*), max(updated_at) FROM server_settings`).Scan(&n, &latest); err != nil {
		return "", err
	}
	if latest == nil {
		return fmt.Sprint(n), nil
	}
	return fmt.Sprintf("%d@%d", n, latest.UnixNano()), nil
}

func (s *Store) write(ctx context.Context, tx pgx.Tx, key string, v any, by string) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO server_settings (key, value, updated_at, updated_by) VALUES ($1, $2, clock_timestamp(), $3)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at, updated_by = excluded.updated_by`,
		key, raw, by)
	return err
}

// PutMFA sets the server-wide MFA default.
func (s *Store) PutMFA(ctx context.Context, m MFA, by string) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error { return s.write(ctx, tx, KeyMFA, m, by) })
}

// PutLockout validates and stores a lockout override.
func (s *Store) PutLockout(ctx context.Context, l Lockout, by string) error {
	if _, err := l.Parse(); err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error { return s.write(ctx, tx, KeyLockout, l, by) })
}

// PutSessions validates and stores token lifetimes.
func (s *Store) PutSessions(ctx context.Context, x Sessions, by string) error {
	if _, _, err := x.Parse(); err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error { return s.write(ctx, tx, KeySessions, x, by) })
}

// PutEmail validates and stores the email settings. An empty Password keeps
// the stored one. accountURL is the deployment's PARLEY_ACCOUNT_PUBLIC_URL,
// which links need.
func (s *Store) PutEmail(ctx context.Context, e Email, accountURL, by string) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		cur, err := s.lockGroup(ctx, tx, KeyEmail)
		if err != nil {
			return err
		}
		e.PasswordSealed = nil
		if e.Password != "" {
			if e.PasswordSealed, err = sealbox.Seal(s.key, []byte("email.password"), []byte(e.Password)); err != nil {
				return err
			}
		} else if cur != nil {
			var old Email
			if err := json.Unmarshal(cur, &old); err == nil {
				e.PasswordSealed = old.PasswordSealed
			}
		}
		e.Password = ""
		if _, err := s.EmailSender(e, accountURL); err != nil {
			return err
		}
		return s.write(ctx, tx, KeyEmail, e, by)
	})
}

// PutSMS validates and stores the SMS settings. An empty AuthToken keeps the
// stored one.
func (s *Store) PutSMS(ctx context.Context, x SMS, accountURL, by string) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		cur, err := s.lockGroup(ctx, tx, KeySMS)
		if err != nil {
			return err
		}
		x.AuthTokenSealed = nil
		if x.AuthToken != "" {
			if x.AuthTokenSealed, err = sealbox.Seal(s.key, []byte("sms.auth_token"), []byte(x.AuthToken)); err != nil {
				return err
			}
		} else if cur != nil {
			var old SMS
			if err := json.Unmarshal(cur, &old); err == nil {
				x.AuthTokenSealed = old.AuthTokenSealed
			}
		}
		x.AuthToken = ""
		if _, err := s.SMSSender(x, accountURL); err != nil {
			return err
		}
		return s.write(ctx, tx, KeySMS, x, by)
	})
}

func (s *Store) lockGroup(ctx context.Context, tx pgx.Tx, key string) ([]byte, error) {
	var raw []byte
	err := tx.QueryRow(ctx, `SELECT value FROM server_settings WHERE key = $1 FOR UPDATE`, key).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return raw, err
}

// Revert deletes an override so the deployment's value applies again. For
// "mfa" this means MFA is required.
func (s *Store) Revert(ctx context.Context, key, by string) error {
	switch key {
	case KeyLockout, KeySessions, KeyEmail, KeySMS:
	case KeyMFA:
		return s.PutMFA(ctx, MFA{Require: true}, by)
	default:
		return fmt.Errorf("unknown setting %q", key)
	}
	// The row count changes, so Fingerprint notices.
	_, err := s.pool.Exec(ctx, `DELETE FROM server_settings WHERE key = $1`, key)
	return err
}

// Validation ------------------------------------------------------------------

// ValidationError is a setting value an operator has to fix.
type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return e.Msg }

func invalid(format string, a ...any) error { return &ValidationError{Msg: fmt.Sprintf(format, a...)} }

func parseDur(name, v string, lo, hi time.Duration) (time.Duration, error) {
	d, err := time.ParseDuration(strings.TrimSpace(v))
	if err != nil {
		return 0, invalid("%s: enter a duration like 15m, 12h or 720h", name)
	}
	if d < lo || d > hi {
		return 0, invalid("%s must be between %s and %s", name, FormatDuration(lo), FormatDuration(hi))
	}
	return d, nil
}

// FormatDuration writes d the way people type it: "15m", "720h", "1h30m".
func FormatDuration(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

// Parse validates the override.
func (l Lockout) Parse() (identity.LockoutConfig, error) {
	var c identity.LockoutConfig
	var err error
	if l.MaxFailures < 0 || l.MaxFailures > 100 {
		return c, invalid("max failures must be between 0 (off) and 100")
	}
	if l.AccountMaxFailures < 0 || l.AccountMaxFailures > 1000 {
		return c, invalid("account-wide max failures must be between 0 (off) and 1000")
	}
	if c.Duration, err = parseDur("lockout duration", l.Duration, time.Minute, 24*time.Hour); err != nil {
		return c, err
	}
	if c.Window, err = parseDur("failure window", l.Window, time.Minute, 24*time.Hour); err != nil {
		return c, err
	}
	switch identity.LockoutScope(l.Scope) {
	case identity.ScopeAccount, identity.ScopeAccountIP:
	default:
		return c, invalid(`scope must be "account_ip" or "account"`)
	}
	c.MaxFailures, c.AccountMaxFailures, c.Scope = l.MaxFailures, l.AccountMaxFailures, identity.LockoutScope(l.Scope)
	return c, nil
}

// Parse validates the override. Access tokens can't be revoked, so their
// lifetime is capped; the floor keeps clients from refreshing constantly.
func (x Sessions) Parse() (access, refresh time.Duration, err error) {
	if access, err = parseDur("access token lifetime", x.AccessTTL, time.Minute, 24*time.Hour); err != nil {
		return
	}
	if refresh, err = parseDur("session lifetime", x.RefreshTTL, time.Hour, 366*24*time.Hour); err != nil {
		return
	}
	if refresh <= access {
		err = invalid("session lifetime must be longer than the access token lifetime")
	}
	return
}

// EmailSender builds the sender for e, or nil if it's off.
func (s *Store) EmailSender(e Email, accountURL string) (delivery.Sender, error) {
	switch e.Mode {
	case "off":
		return nil, nil
	case "smtp":
	default:
		return nil, invalid(`email mode must be "off" or "smtp"`)
	}
	if err := CheckAccountURL(accountURL); err != nil {
		return nil, invalid("%s", err.Error())
	}
	host := strings.TrimSpace(e.Host)
	if host == "" || strings.ContainsAny(host, " /:") {
		return nil, invalid("enter the SMTP server's host name")
	}
	if e.Port < 1 || e.Port > 65535 {
		return nil, invalid("SMTP port must be between 1 and 65535")
	}
	if _, err := mail.ParseAddress(e.From); err != nil {
		return nil, invalid(`"From" must be an address like no-reply@example.org or "Parley <no-reply@example.org>"`)
	}
	if strings.ContainsAny(e.From+e.Username, "\r\n") {
		return nil, invalid("line breaks aren't allowed")
	}
	smtp := &delivery.SMTP{Host: host, Port: e.Port, Username: strings.TrimSpace(e.Username), From: e.From}
	switch e.TLS {
	case "starttls":
	case "implicit":
		smtp.ImplicitTLS = true
	default:
		return nil, invalid(`TLS must be "starttls" or "implicit"`)
	}
	if smtp.Username != "" {
		if e.PasswordSealed == nil {
			return nil, invalid("enter the SMTP password")
		}
		pw, err := sealbox.Open(s.key, []byte("email.password"), e.PasswordSealed)
		if err != nil {
			return nil, invalid("the stored SMTP password can't be read (was PARLEY_MFA_KEY changed?); enter it again")
		}
		smtp.Password = string(pw)
	}
	return smtp, nil
}

// SMSSender builds the sender for x, or nil if it's off.
func (s *Store) SMSSender(x SMS, accountURL string) (delivery.Sender, error) {
	switch x.Mode {
	case "off":
		return nil, nil
	case "twilio":
	default:
		return nil, invalid(`SMS mode must be "off" or "twilio"`)
	}
	if err := CheckAccountURL(accountURL); err != nil {
		return nil, invalid("%s", err.Error())
	}
	sid, from := strings.TrimSpace(x.AccountSID), strings.TrimSpace(x.From)
	if !strings.HasPrefix(sid, "AC") || len(sid) != 34 {
		return nil, invalid("the Twilio account SID starts with AC and is 34 characters long")
	}
	if from == "" {
		return nil, invalid(`"From" must be a Twilio number (+15551234567) or a Messaging Service SID (MG...)`)
	}
	if x.AuthTokenSealed == nil {
		return nil, invalid("enter the Twilio auth token")
	}
	tok, err := sealbox.Open(s.key, []byte("sms.auth_token"), x.AuthTokenSealed)
	if err != nil {
		return nil, invalid("the stored Twilio auth token can't be read (was PARLEY_MFA_KEY changed?); enter it again")
	}
	return &delivery.Twilio{AccountSID: sid, AuthToken: string(tok), From: from}, nil
}

// CheckAccountURL requires https, since the account page carries new
// passwords and link tokens. Plain http is allowed only for this machine.
func CheckAccountURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return errors.New("PARLEY_ACCOUNT_PUBLIC_URL is required when email or SMS is enabled, e.g. https://account.example.org")
	}
	if host := u.Hostname(); u.Scheme != "https" && !(u.Scheme == "http" && (host == "localhost" || host == "127.0.0.1")) {
		return errors.New("PARLEY_ACCOUNT_PUBLIC_URL must use https")
	}
	return nil
}

// Resolution ------------------------------------------------------------------

// Resolve layers snap over base (the environment's configuration). A group
// that fails to apply keeps base's value and is reported in problems, so one
// bad setting can't take the server down.
func (s *Store) Resolve(base identity.Runtime, snap Snapshot) (identity.Runtime, map[string]error) {
	out, problems := base, map[string]error{}
	if snap.Lockout != nil {
		if c, err := snap.Lockout.Parse(); err != nil {
			problems[KeyLockout] = err
		} else {
			out.Lockout = c
		}
	}
	if snap.Sessions != nil {
		if a, r, err := snap.Sessions.Parse(); err != nil {
			problems[KeySessions] = err
		} else {
			out.AccessTTL, out.RefreshTTL = a, r
		}
	}
	if snap.Email != nil {
		if sender, err := s.EmailSender(*snap.Email, base.Recovery.AccountURL); err != nil {
			problems[KeyEmail] = err
		} else {
			out.Recovery.Email, out.Recovery.EmailEnabled = sender, sender != nil
		}
	}
	if snap.SMS != nil {
		if sender, err := s.SMSSender(*snap.SMS, base.Recovery.AccountURL); err != nil {
			problems[KeySMS] = err
		} else {
			out.Recovery.SMS, out.Recovery.SMSEnabled = sender, sender != nil
		}
	}
	return out, problems
}

// Apply loads the stored settings and hands the result to apply.
func (s *Store) Apply(ctx context.Context, base identity.Runtime, apply func(identity.Runtime)) error {
	snap, err := s.Load(ctx)
	if err != nil {
		return err
	}
	rt, problems := s.Resolve(base, snap)
	for k, err := range problems {
		slog.ErrorContext(ctx, "stored setting ignored; using the deployment's value", "setting", k, "err", err)
	}
	apply(rt)
	return nil
}

// Watch re-applies the settings whenever they change, checking every interval,
// until ctx ends. Errors are logged and the last good settings stay in effect.
func (s *Store) Watch(ctx context.Context, base identity.Runtime, apply func(identity.Runtime), every time.Duration) {
	last, _ := s.Fingerprint(ctx)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		fp, err := s.Fingerprint(ctx)
		if err != nil {
			slog.WarnContext(ctx, "checking settings", "err", err)
			continue
		}
		if fp == last {
			continue
		}
		if err := s.Apply(ctx, base, apply); err != nil {
			slog.WarnContext(ctx, "loading settings", "err", err)
			continue
		}
		last = fp
		slog.InfoContext(ctx, "settings changed; applied")
	}
}
