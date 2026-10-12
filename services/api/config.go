package main

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"parley/services/api/internal/delivery"
	"parley/services/api/internal/identity"
)

// Login lockout:
//
//	PARLEY_LOGIN_MAX_FAILURES          default 5 (0 disables lockout)
//	PARLEY_LOGIN_LOCKOUT               default "5m"
//	PARLEY_LOGIN_FAILURE_WINDOW        default "15m"; older failures are forgotten
//	PARLEY_LOGIN_LOCKOUT_SCOPE         "account_ip" (default) or "account"
//	PARLEY_LOGIN_ACCOUNT_MAX_FAILURES  default 20; account-wide cap in account_ip scope (0 disables)
//	PARLEY_TRUSTED_PROXIES             comma-separated CIDRs whose X-Forwarded-For is trusted
func lockoutConfig() (identity.LockoutConfig, error) {
	c := identity.DefaultLockout
	var err error
	if c.MaxFailures, err = intEnv("PARLEY_LOGIN_MAX_FAILURES", c.MaxFailures); err != nil {
		return c, err
	}
	if c.Duration, err = durationEnv("PARLEY_LOGIN_LOCKOUT", c.Duration); err != nil {
		return c, err
	}
	if c.Window, err = durationEnv("PARLEY_LOGIN_FAILURE_WINDOW", c.Window); err != nil {
		return c, err
	}
	if c.AccountMaxFailures, err = intEnv("PARLEY_LOGIN_ACCOUNT_MAX_FAILURES", c.AccountMaxFailures); err != nil {
		return c, err
	}
	switch scope := identity.LockoutScope(envOr("PARLEY_LOGIN_LOCKOUT_SCOPE", string(c.Scope))); scope {
	case identity.ScopeAccount, identity.ScopeAccountIP:
		c.Scope = scope
	default:
		return c, fmt.Errorf(`PARLEY_LOGIN_LOCKOUT_SCOPE must be "account_ip" or "account", got %q`, scope)
	}
	return c, nil
}

// Account recovery. Both channels are off unless configured:
//
//	PARLEY_RECOVERY_EMAIL   "off" (default), "smtp", or "log"
//	PARLEY_RECOVERY_SMS     "off" (default), "twilio", or "log"
//
// "log" writes codes to the server log instead of sending them. It exists so
// the flows can be tested without a mail or SMS provider and must never be
// used in production: anyone who can read the logs can reset any password.
//
//	PARLEY_SMTP_HOST, PARLEY_SMTP_PORT (default 587), PARLEY_SMTP_USERNAME,
//	PARLEY_SMTP_PASSWORD, PARLEY_SMTP_FROM, PARLEY_SMTP_TLS ("starttls" default, or "implicit")
//	PARLEY_TWILIO_ACCOUNT_SID, PARLEY_TWILIO_AUTH_TOKEN, PARLEY_TWILIO_FROM
func recoveryConfig() (identity.RecoveryConfig, error) {
	var c identity.RecoveryConfig

	switch mode := envOr("PARLEY_RECOVERY_EMAIL", "off"); mode {
	case "off":
	case "log":
		c.EmailEnabled, c.Email = true, delivery.Log{Channel: "email"}
		warnLogDelivery("PARLEY_RECOVERY_EMAIL")
	case "smtp":
		s, err := smtpSender()
		if err != nil {
			return c, err
		}
		c.EmailEnabled, c.Email = true, s
	default:
		return c, fmt.Errorf(`PARLEY_RECOVERY_EMAIL must be "off", "smtp" or "log", got %q`, mode)
	}

	switch mode := envOr("PARLEY_RECOVERY_SMS", "off"); mode {
	case "off":
	case "log":
		c.SMSEnabled, c.SMS = true, delivery.Log{Channel: "sms"}
		warnLogDelivery("PARLEY_RECOVERY_SMS")
	case "twilio":
		t := &delivery.Twilio{
			AccountSID: os.Getenv("PARLEY_TWILIO_ACCOUNT_SID"),
			AuthToken:  os.Getenv("PARLEY_TWILIO_AUTH_TOKEN"),
			From:       os.Getenv("PARLEY_TWILIO_FROM"),
		}
		if t.AccountSID == "" || t.AuthToken == "" || t.From == "" {
			return c, errors.New("PARLEY_RECOVERY_SMS=twilio requires PARLEY_TWILIO_ACCOUNT_SID, PARLEY_TWILIO_AUTH_TOKEN and PARLEY_TWILIO_FROM")
		}
		c.SMSEnabled, c.SMS = true, t
	default:
		return c, fmt.Errorf(`PARLEY_RECOVERY_SMS must be "off", "twilio" or "log", got %q`, mode)
	}
	return c, nil
}

func smtpSender() (*delivery.SMTP, error) {
	port, err := intEnv("PARLEY_SMTP_PORT", 587)
	if err != nil {
		return nil, err
	}
	s := &delivery.SMTP{
		Host:     os.Getenv("PARLEY_SMTP_HOST"),
		Port:     port,
		Username: os.Getenv("PARLEY_SMTP_USERNAME"),
		Password: os.Getenv("PARLEY_SMTP_PASSWORD"),
		From:     os.Getenv("PARLEY_SMTP_FROM"),
	}
	if s.Host == "" || s.From == "" {
		return nil, errors.New("PARLEY_RECOVERY_EMAIL=smtp requires PARLEY_SMTP_HOST and PARLEY_SMTP_FROM")
	}
	switch tlsMode := envOr("PARLEY_SMTP_TLS", "starttls"); tlsMode {
	case "starttls":
	case "implicit":
		s.ImplicitTLS = true
	default:
		return nil, fmt.Errorf(`PARLEY_SMTP_TLS must be "starttls" or "implicit", got %q`, tlsMode)
	}
	return s, nil
}

func warnLogDelivery(name string) {
	slog.Warn(strings.Repeat("!", 72))
	slog.Warn(name + "=log: verification and password-reset codes are written to this log " +
		"instead of being sent. Anyone who can read these logs can take over any account. " +
		"Development and testing only.")
	slog.Warn(strings.Repeat("!", 72))
}

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func intEnv(name string, def int) (int, error) {
	v := os.Getenv(name)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%s must be a non-negative integer, got %q", name, v)
	}
	return n, nil
}

func durationEnv(name string, def time.Duration) (time.Duration, error) {
	v := os.Getenv(name)
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", name, err)
	}
	return d, nil
}
