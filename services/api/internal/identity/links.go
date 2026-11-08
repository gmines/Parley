package identity

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"parley/pkg/db"
	"parley/services/api/internal/delivery"
)

const (
	linkActivate = "activate"
	linkReset    = "reset_password"
)

var (
	ErrInvalidLink     = errors.New("this link is invalid or has expired; request a new one")
	ErrContactRequired = errors.New("enter an email address or a phone number, but not both")
)

// Signup is a request for a new account. Exactly one of Email and Phone is set.
type Signup struct {
	Username    string
	DisplayName string
	Email       string
	Phone       string
}

// Signup creates an inactive account and sends its activation link. An address
// that already belongs to an account gets the same answer and no message, so
// this can't be used to find out who has an account. Usernames are public, so
// a taken one is reported.
func (s *Service) Signup(ctx context.Context, in Signup, clientIP string) error {
	username := NormalizeUsername(in.Username)
	if !validUsername(username) {
		return ErrInvalidUsername
	}
	displayName := strings.TrimSpace(in.DisplayName)
	if displayName == "" {
		displayName = username
	}
	if !validDisplayName(displayName) {
		return ErrInvalidDisplayName
	}
	if (strings.TrimSpace(in.Email) == "") == (strings.TrimSpace(in.Phone) == "") {
		return ErrContactRequired
	}
	email, phone, err := s.validateContacts(in.Email, in.Phone)
	if err != nil {
		return err
	}
	channel, dest := channelEmail, email
	if phone != "" {
		channel, dest = channelSMS, phone
	}
	col, verifiedCol, _ := contactColumns(channel)

	var link *pendingCode
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		link = nil
		now, c := s.now(), s.cfg.Recovery
		// Abandoned sign-ups release their username and address.
		if _, err := tx.Exec(ctx, `DELETE FROM users u WHERE u.activated_at IS NULL AND u.created_at < $1
			AND NOT EXISTS (SELECT 1 FROM account_links l WHERE l.user_id = u.id AND l.consumed_at IS NULL AND l.expires_at > $2)`,
			now.Add(-c.ActivationTTL), now); err != nil {
			return err
		}

		var perIP int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM account_links WHERE request_ip = $1 AND created_at > $2`,
			clientIP, now.Add(-time.Hour)).Scan(&perIP); err != nil {
			return err
		}
		if perIP >= c.MaxSignupsPerIPHour {
			return ErrTooManyCodes
		}

		var pending *uuid.UUID
		var id uuid.UUID
		err := tx.QueryRow(ctx, fmt.Sprintf(`SELECT id FROM users WHERE %s = $1 AND activated_at IS NULL FOR UPDATE`, col),
			dest).Scan(&id)
		if err == nil {
			pending = &id
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}

		// The username answer comes first, so it can't depend on the address.
		var holder uuid.UUID
		err = tx.QueryRow(ctx, `SELECT id FROM users WHERE username = $1`, username).Scan(&holder)
		if err == nil && (pending == nil || holder != *pending) {
			return ErrUsernameTaken
		}
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}

		var inUse bool
		if err := tx.QueryRow(ctx, fmt.Sprintf(`SELECT EXISTS (SELECT 1 FROM users WHERE %s = $1 AND %s IS NOT NULL)`, col, verifiedCol),
			dest).Scan(&inUse); err != nil {
			return err
		}
		if inUse {
			slog.InfoContext(ctx, "signup for an address already in use")
			return nil
		}

		var u User
		if pending != nil {
			if err := s.checkLinkRate(ctx, tx, *pending, linkActivate); err != nil {
				slog.InfoContext(ctx, "signup link rate limited", "user_id", *pending)
				return nil
			}
			err = tx.QueryRow(ctx, `UPDATE users u SET username = $2, display_name = $3 WHERE u.id = $1 RETURNING `+userCols("u"),
				*pending, username, displayName).Scan(userDest(&u)...)
		} else {
			err = tx.QueryRow(ctx, `INSERT INTO users (username, display_name, email, phone) VALUES ($1, $2, $3, $4)
				RETURNING `+userCols("users"), username, displayName, nullable(email), nullable(phone)).Scan(userDest(&u)...)
		}
		if db.IsUniqueViolation(err, "users_username_key") {
			return ErrUsernameTaken
		}
		if db.IsUniqueViolation(err, "") { // lost a race with the same address
			return ErrTooManyCodes
		}
		if err != nil {
			return fmt.Errorf("save signup: %w", err)
		}
		l, err := s.issueLink(ctx, tx, u, linkActivate, channel, dest, clientIP)
		link = &l
		return err
	})
	if err != nil {
		return err
	}
	if link != nil {
		s.deliver(ctx, *link)
	}
	return nil
}

// RequestPasswordReset sends a reset link to a verified contact of the account
// named by identifier (username, email or phone). It reports success whether
// or not anything was sent, so it can't be used to probe accounts.
func (s *Service) RequestPasswordReset(ctx context.Context, identifier, clientIP string) error {
	if !s.cfg.Recovery.EmailEnabled && !s.cfg.Recovery.SMSEnabled {
		return ErrFeatureDisabled
	}
	identifier = strings.TrimSpace(identifier)
	// Usernames can't contain '@' or '+', so the three forms can't collide.
	email, _ := normalizeEmail(identifier)
	phone, _ := normalizePhone(identifier)

	var link *pendingCode
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var u User
		err := tx.QueryRow(ctx, `SELECT `+userCols("u")+` FROM users u WHERE u.activated_at IS NOT NULL AND (
			u.username = $1 OR (u.email = $2 AND u.email_verified_at IS NOT NULL) OR (u.phone = $3 AND u.phone_verified_at IS NOT NULL))`,
			NormalizeUsername(identifier), email, phone).Scan(userDest(&u)...)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		c := s.cfg.Recovery
		emailOK := u.EmailVerified && c.EmailEnabled && phone == ""
		smsOK := u.PhoneVerified && c.SMSEnabled && email == ""
		var channel, dest string
		switch {
		case emailOK:
			channel, dest = channelEmail, u.Email
		case smsOK:
			channel, dest = channelSMS, u.Phone
		default:
			return nil
		}
		if err := s.checkLinkRate(ctx, tx, u.ID, linkReset); err != nil {
			slog.InfoContext(ctx, "password reset rate limited", "user_id", u.ID)
			return nil
		}
		l, err := s.issueLink(ctx, tx, u, linkReset, channel, dest, clientIP)
		link = &l
		return err
	})
	if err != nil {
		return err
	}
	if link != nil {
		s.deliver(ctx, *link)
	}
	return nil
}

// LinkInfo is what the account page shows before asking for a password.
type LinkInfo struct {
	Purpose  string `json:"purpose"`
	Username string `json:"username"`
}

// InspectLink describes a live link without using it, so a mail scanner
// following the URL can't burn it.
func (s *Service) InspectLink(ctx context.Context, token string) (LinkInfo, error) {
	var info LinkInfo
	err := s.pool.QueryRow(ctx, `SELECT l.purpose, u.username FROM account_links l JOIN users u ON u.id = l.user_id
		WHERE l.token_hash = $1 AND l.consumed_at IS NULL AND l.expires_at > $2`,
		hashToken(token), s.now()).Scan(&info.Purpose, &info.Username)
	if errors.Is(err, pgx.ErrNoRows) {
		return LinkInfo{}, ErrInvalidLink
	}
	return info, err
}

// CompleteLink sets the password chosen on the account page and uses up the
// link. Activation also verifies the address the link was sent to; a reset
// signs out every session. A reset never touches MFA: the next login still
// needs the second factor.
func (s *Service) CompleteLink(ctx context.Context, token, newPassword string) error {
	var notify *User
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var (
			id                     uuid.UUID
			purpose, channel, dest string
			u                      User
		)
		err := tx.QueryRow(ctx, `SELECT l.id, l.purpose, l.channel, l.destination, `+userCols("u")+`
			FROM account_links l JOIN users u ON u.id = l.user_id
			WHERE l.token_hash = $1 AND l.consumed_at IS NULL AND l.expires_at > $2
			FOR UPDATE OF l, u`, hashToken(token), s.now()).
			Scan(append([]any{&id, &purpose, &channel, &dest}, userDest(&u)...)...)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrInvalidLink
		}
		if err != nil {
			return err
		}
		// Argon2 runs only for a valid token, and a rejected password rolls
		// back, leaving the link usable.
		if !validPasswordLength(newPassword) {
			return ErrInvalidPassword
		}
		if err := checkPasswordStrength(newPassword, u.Username, u.DisplayName); err != nil {
			return err
		}
		hash, err := hashPassword(newPassword, s.cfg.Hash)
		if err != nil {
			return err
		}
		col, verifiedCol, _ := contactColumns(channel)
		switch purpose {
		case linkActivate:
			_, err := tx.Exec(ctx, fmt.Sprintf(`UPDATE users SET password_hash = $2, activated_at = $3, %s = $3
				WHERE id = $1 AND %s = $4`, verifiedCol, col), u.ID, hash, s.now(), dest)
			if db.IsUniqueViolation(err, "") {
				return ErrContactInUse
			}
			if err != nil {
				return err
			}
		case linkReset:
			// The contact may have been changed or removed since the link was sent.
			current, verified := u.Email, u.EmailVerified
			if channel == channelSMS {
				current, verified = u.Phone, u.PhoneVerified
			}
			if !verified || current != dest {
				return ErrInvalidLink
			}
			if err := setPassword(ctx, tx, u, hash); err != nil {
				return err
			}
			notify = &u
		default:
			return ErrInvalidLink
		}
		_, err = tx.Exec(ctx, `UPDATE account_links SET consumed_at = $3 WHERE user_id = $1 AND purpose = $2 AND consumed_at IS NULL`,
			u.ID, purpose, s.now())
		return err
	})
	if err == nil && notify != nil {
		s.notify(ctx, *notify, "Your password was changed",
			"Your password was just reset and all devices were signed out. If this wasn't you, contact your server administrator.")
	}
	return err
}

// checkLinkRate enforces the resend interval and hourly cap per user and purpose.
func (s *Service) checkLinkRate(ctx context.Context, q db.Querier, userID uuid.UUID, purpose string) error {
	c := s.cfg.Recovery
	now := s.now()
	var (
		lastHour int
		latest   *time.Time
	)
	if err := q.QueryRow(ctx, `SELECT count(*), max(created_at) FROM account_links
		WHERE user_id = $1 AND purpose = $2 AND created_at > $3`,
		userID, purpose, now.Add(-time.Hour)).Scan(&lastHour, &latest); err != nil {
		return err
	}
	if lastHour >= c.MaxCodesPerHour || (latest != nil && now.Sub(*latest) < c.ResendInterval) {
		return ErrTooManyCodes
	}
	return nil
}

// issueLink stores a new link, superseding earlier unused ones for the same
// purpose, and returns the message to deliver once the transaction commits.
func (s *Service) issueLink(ctx context.Context, tx pgx.Tx, u User, purpose, channel, dest, clientIP string) (pendingCode, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return pendingCode{}, err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	now := s.now()
	ttl := s.cfg.Recovery.ResetTTL
	if purpose == linkActivate {
		ttl = s.cfg.Recovery.ActivationTTL
	}
	if _, err := tx.Exec(ctx, `UPDATE account_links SET consumed_at = $3 WHERE user_id = $1 AND purpose = $2 AND consumed_at IS NULL`,
		u.ID, purpose, now); err != nil {
		return pendingCode{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO account_links (user_id, purpose, channel, destination, token_hash, request_ip, expires_at, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		u.ID, purpose, channel, dest, hashToken(token), clientIP, now.Add(ttl), now); err != nil {
		return pendingCode{}, err
	}
	// The token goes in the fragment, which browsers never send to a server,
	// so it stays out of CDN, proxy and access logs.
	url := strings.TrimRight(s.cfg.Recovery.AccountURL, "/") + "/account/set-password#t=" + token
	return pendingCode{channel: channel, msg: s.linkMessage(u, purpose, channel, dest, url, ttl)}, nil
}

func (s *Service) linkMessage(u User, purpose, channel, dest, url string, ttl time.Duration) delivery.Message {
	app := s.cfg.Recovery.AppName
	expires := fmt.Sprintf("%d minutes", int(ttl.Minutes()))
	if ttl >= 2*time.Hour {
		expires = fmt.Sprintf("%d hours", int(ttl.Hours()))
	}
	var subject, body, sms string
	if purpose == linkActivate {
		subject = fmt.Sprintf("Activate your %s account", app)
		body = fmt.Sprintf("Welcome to %s, @%s.\n\nOpen this link to activate your account and choose a password:\n\n%s\n\n"+
			"It expires in %s. If you didn't sign up, ignore this message and no account will be created.", app, u.Username, url, expires)
		sms = fmt.Sprintf("Activate your %s account: %s (expires in %s)", app, url, expires)
	} else {
		subject = fmt.Sprintf("Reset your %s password", app)
		body = fmt.Sprintf("Open this link to choose a new password for @%s:\n\n%s\n\n"+
			"It expires in %s. If you didn't ask for this, ignore this message. Your password has not been changed.", u.Username, url, expires)
		sms = fmt.Sprintf("Reset your %s password: %s (expires in %s)", app, url, expires)
	}
	if channel == channelSMS {
		body = sms
	}
	return delivery.Message{To: dest, Subject: subject, Body: body}
}

// notify tells the account's owner about a security-relevant change, on the
// first verified contact. Best effort.
func (s *Service) notify(ctx context.Context, u User, subject, body string) {
	c := s.cfg.Recovery
	switch {
	case u.EmailVerified && c.EmailEnabled:
		s.deliver(ctx, pendingCode{channel: channelEmail, msg: delivery.Message{To: u.Email, Subject: c.AppName + ": " + subject, Body: body}})
	case u.PhoneVerified && c.SMSEnabled:
		s.deliver(ctx, pendingCode{channel: channelSMS, msg: delivery.Message{To: u.Phone, Subject: subject, Body: c.AppName + ": " + body}})
	}
}
