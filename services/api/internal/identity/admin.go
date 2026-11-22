package identity

// Operations for the admin app. Callers are trusted operators: these skip the
// password and rate checks a user's own requests go through, and the admin
// package records each one in its audit log.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"parley/pkg/db"
	"parley/services/api/internal/delivery"
)

var (
	ErrInvalidMFAPolicy  = errors.New(`MFA policy must be "inherit", "required" or "exempt"`)
	ErrNoAccountPage     = errors.New("reset links need the account page: set PARLEY_ACCOUNT_PUBLIC_URL (accountWeb.enabled in the Helm chart)")
	ErrNoVerifiedContact = errors.New("this account has no verified email or phone on an enabled channel; create a link to hand over instead")
	ErrNotActivated      = errors.New("this account hasn't been activated yet")
)

// AdminLinkTTL is how long a reset link created for hand-delivery lasts.
const AdminLinkTTL = 24 * time.Hour

// MFA policies (users.mfa_policy).
const (
	MFAPolicyInherit  = "inherit"
	MFAPolicyRequired = "required"
	MFAPolicyExempt   = "exempt"
)

// lockUser loads an account for update.
func lockUser(ctx context.Context, tx pgx.Tx, id uuid.UUID) (User, error) {
	var u User
	err := tx.QueryRow(ctx, `SELECT `+userCols("u")+` FROM users u WHERE u.id = $1 FOR UPDATE`, id).Scan(userDest(&u)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrUserNotFound
	}
	return u, err
}

func revokeSessions(ctx context.Context, q db.Querier, userID uuid.UUID) error {
	_, err := q.Exec(ctx, `UPDATE refresh_tokens SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL`, userID)
	return err
}

// AdminSetDisplayName changes an account's display name.
func (s *Service) AdminSetDisplayName(ctx context.Context, userID uuid.UUID, name string) (User, error) {
	name = strings.TrimSpace(name)
	if !validDisplayName(name) {
		return User{}, ErrInvalidDisplayName
	}
	var u User
	err := s.pool.QueryRow(ctx, `UPDATE users u SET display_name = $2 WHERE u.id = $1 RETURNING `+userCols("u"), userID, name).
		Scan(userDest(&u)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrUserNotFound
	}
	return u, err
}

// AdminSetContact sets, replaces or (with an empty value) removes an account's
// email ("email") or phone ("sms"). With verified, the new value counts as
// verified at once, so it works for password reset even while delivery is off.
// Without it, a verification code goes to the new address, which the user
// enters in the app. The previous verified address, if any, is told.
func (s *Service) AdminSetContact(ctx context.Context, userID uuid.UUID, channel, value string, verified bool) (User, error) {
	if channel != channelEmail && channel != channelSMS {
		return User{}, ErrInvalidChannel
	}
	value = strings.TrimSpace(value)
	if value != "" {
		var err error
		if value, err = normalizeContact(channel, value); err != nil {
			return User{}, err
		}
	}
	if value != "" && !verified {
		if on, _ := s.channelEnabled(channel); !on {
			return User{}, ErrFeatureDisabled
		}
	}
	col, verifiedCol, purpose := contactColumns(channel)
	var (
		before, after User
		code          *pendingCode
	)
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		code = nil
		var err error
		if before, err = lockUser(ctx, tx, userID); err != nil {
			return err
		}
		switch {
		case value == "":
			_, err = tx.Exec(ctx, fmt.Sprintf(`UPDATE users SET %s = NULL, %s = NULL WHERE id = $1`, col, verifiedCol), userID)
		case verified:
			_, err = tx.Exec(ctx, fmt.Sprintf(`UPDATE users SET %s = $2, %s = $3 WHERE id = $1`, col, verifiedCol), userID, value, s.now())
		default:
			if _, err = tx.Exec(ctx, fmt.Sprintf(`UPDATE users SET %s = $2, %s = NULL WHERE id = $1`, col, verifiedCol), userID, value); err != nil {
				return err
			}
			var c pendingCode
			c, err = s.issueCode(ctx, tx, before, purpose, channel, value)
			code = &c
		}
		if db.IsUniqueViolation(err, "") {
			return ErrContactInUse
		}
		if err != nil {
			return err
		}
		after, err = getUser(ctx, tx, userID)
		return err
	})
	if err != nil {
		return User{}, err
	}
	if code != nil {
		s.deliver(ctx, *code)
	}
	// Tell the address that was replaced, so a hijacked admin can't move an
	// account away quietly.
	old, oldVerified, now := before.Email, before.EmailVerified, after.Email
	if channel == channelSMS {
		old, oldVerified, now = before.Phone, before.PhoneVerified, after.Phone
	}
	if oldVerified && old != now {
		what := "email address"
		if channel == channelSMS {
			what = "phone number"
		}
		s.notifyAt(ctx, channel, old, "Your "+what+" was changed",
			fmt.Sprintf("An administrator changed the %s on your account @%s. If you didn't ask for this, contact your server administrator.", what, before.Username))
	}
	return after, nil
}

// AdminSendResetLink sends a password reset link to the account's verified
// email or phone, like a "forgot password" request but without its rate limit,
// and reports the channel used.
func (s *Service) AdminSendResetLink(ctx context.Context, userID uuid.UUID) (string, error) {
	if strings.TrimSpace(s.cfg().Recovery.AccountURL) == "" {
		return "", ErrNoAccountPage
	}
	var link pendingCode
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		u, err := s.resettable(ctx, tx, userID)
		if err != nil {
			return err
		}
		c := s.cfg().Recovery
		var channel, dest string
		switch {
		case u.EmailVerified && c.EmailEnabled:
			channel, dest = channelEmail, u.Email
		case u.PhoneVerified && c.SMSEnabled:
			channel, dest = channelSMS, u.Phone
		default:
			return ErrNoVerifiedContact
		}
		link, err = s.issueLink(ctx, tx, u, linkReset, channel, dest, "admin")
		return err
	})
	if err != nil {
		return "", err
	}
	s.deliver(ctx, link)
	return link.channel, nil
}

// AdminCreateResetLink returns a password reset link for the admin to hand to
// the user, for accounts with no way to receive one. It lasts AdminLinkTTL and
// replaces any earlier unused reset link.
func (s *Service) AdminCreateResetLink(ctx context.Context, userID uuid.UUID) (string, time.Time, error) {
	if strings.TrimSpace(s.cfg().Recovery.AccountURL) == "" {
		return "", time.Time{}, ErrNoAccountPage
	}
	var url string
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		u, err := s.resettable(ctx, tx, userID)
		if err != nil {
			return err
		}
		url, err = s.storeLink(ctx, tx, u, linkReset, channelAdmin, "", "admin", AdminLinkTTL)
		return err
	})
	if err != nil {
		return "", time.Time{}, err
	}
	return url, s.now().Add(AdminLinkTTL), nil
}

func (s *Service) resettable(ctx context.Context, tx pgx.Tx, userID uuid.UUID) (User, error) {
	u, err := lockUser(ctx, tx, userID)
	if err != nil {
		return User{}, err
	}
	var active bool
	if err := tx.QueryRow(ctx, `SELECT activated_at IS NOT NULL FROM users WHERE id = $1`, userID).Scan(&active); err != nil {
		return User{}, err
	}
	switch {
	case !active:
		return User{}, ErrNotActivated
	case u.Disabled:
		return User{}, ErrAccountDisabled
	}
	return u, nil
}

// AdminResetMFAByID clears an account's authenticator and recovery codes and
// signs it out everywhere. If MFA is required for it, the next login enrolls.
func (s *Service) AdminResetMFAByID(ctx context.Context, userID uuid.UUID) error {
	var u User
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		if u, err = lockUser(ctx, tx, userID); err != nil {
			return err
		}
		return clearMFA(ctx, tx, userID)
	})
	if err == nil && u.MFAEnabled {
		s.notify(ctx, u, "Two-factor authentication was removed",
			"An administrator removed the authenticator from your account and signed out all devices. If you didn't ask for this, contact your server administrator.")
	}
	return err
}

// AdminSetMFAPolicy sets whether this account must use MFA. Tightening it
// takes effect at each session's next refresh (within the access token
// lifetime): the session drops to enrollment-only until the user sets it up.
func (s *Service) AdminSetMFAPolicy(ctx context.Context, userID uuid.UUID, policy string) (User, error) {
	switch policy {
	case MFAPolicyInherit, MFAPolicyRequired, MFAPolicyExempt:
	default:
		return User{}, ErrInvalidMFAPolicy
	}
	var u User
	err := s.pool.QueryRow(ctx, `UPDATE users u SET mfa_policy = $2 WHERE u.id = $1 RETURNING `+userCols("u"), userID, policy).
		Scan(userDest(&u)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrUserNotFound
	}
	return u, err
}

// AdminSetDisabled disables (signing out everywhere) or re-enables an
// account. Access tokens already issued keep working until they expire.
func (s *Service) AdminSetDisabled(ctx context.Context, userID uuid.UUID, disabled bool) (User, error) {
	var u User
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var at *time.Time
		if disabled {
			now := s.now()
			at = &now
		}
		err := tx.QueryRow(ctx, `UPDATE users u SET disabled_at = CASE WHEN $2::timestamptz IS NULL THEN NULL ELSE coalesce(u.disabled_at, $2) END
			WHERE u.id = $1 RETURNING `+userCols("u"), userID, at).Scan(userDest(&u)...)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrUserNotFound
		}
		if err != nil {
			return err
		}
		if disabled {
			return revokeSessions(ctx, tx, userID)
		}
		return nil
	})
	return u, err
}

// AdminRevokeSessions signs an account out of every device (at the next
// refresh; access tokens already issued last until they expire).
func (s *Service) AdminRevokeSessions(ctx context.Context, userID uuid.UUID) error {
	var exists bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM users WHERE id = $1)`, userID).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return ErrUserNotFound
	}
	return revokeSessions(ctx, s.pool, userID)
}

// notifyAt sends a security notice to a specific address, if its channel is on.
func (s *Service) notifyAt(ctx context.Context, channel, to, subject, body string) {
	c := s.cfg().Recovery
	switch {
	case channel == channelEmail && c.EmailEnabled:
		s.deliver(ctx, pendingCode{channel: channelEmail, msg: delivery.Message{To: to, Subject: c.AppName + ": " + subject, Body: body}})
	case channel == channelSMS && c.SMSEnabled:
		s.deliver(ctx, pendingCode{channel: channelSMS, msg: delivery.Message{To: to, Subject: subject, Body: c.AppName + ": " + body}})
	}
}

// NewUser is the account an admin asks AdminCreateUser to create.
type NewUser struct {
	Username    string
	DisplayName string
	Email       string
}

// CreatedUser is the result of AdminCreateUser: the new (inactive) account, the
// activation link, when it expires, and whether a welcome email was sent.
type CreatedUser struct {
	User      User
	URL       string
	ExpiresAt time.Time
	Emailed   bool
}

// AdminCreateUser creates an inactive account directly, bypassing the public
// signup feature flag (the caller is a trusted operator). It issues an
// activation link that lets the user choose a password; completing it verifies
// the email and activates the account. The link is always returned so the admin
// can hand it over, and is also emailed as a welcome message when email
// delivery is on. Unlike public signup, duplicate username/email are reported
// plainly, since there is no account-probing concern for an admin.
func (s *Service) AdminCreateUser(ctx context.Context, in NewUser) (CreatedUser, error) {
	if strings.TrimSpace(s.cfg().Recovery.AccountURL) == "" {
		return CreatedUser{}, ErrNoAccountPage
	}
	username := NormalizeUsername(in.Username)
	if !validUsername(username) {
		return CreatedUser{}, ErrInvalidUsername
	}
	displayName := strings.TrimSpace(in.DisplayName)
	if displayName == "" {
		displayName = username
	}
	if !validDisplayName(displayName) {
		return CreatedUser{}, ErrInvalidDisplayName
	}
	if strings.TrimSpace(in.Email) == "" {
		return CreatedUser{}, ErrInvalidEmail
	}
	email, err := normalizeEmail(in.Email)
	if err != nil {
		return CreatedUser{}, err
	}

	ttl := s.cfg().Recovery.ActivationTTL
	var (
		u    User
		url  string
		link pendingCode
	)
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM users WHERE username = $1)`, username).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return ErrUsernameTaken
		}
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM users WHERE email = $1)`, email).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return ErrContactInUse
		}
		err := tx.QueryRow(ctx, `INSERT INTO users (username, display_name, email) VALUES ($1, $2, $3)
			RETURNING `+userCols("users"), username, displayName, email).Scan(userDest(&u)...)
		if db.IsUniqueViolation(err, "users_username_key") {
			return ErrUsernameTaken
		}
		if db.IsUniqueViolation(err, "") { // lost a race on the email
			return ErrContactInUse
		}
		if err != nil {
			return fmt.Errorf("create user: %w", err)
		}
		url, err = s.storeLink(ctx, tx, u, linkActivate, channelEmail, email, "admin", ttl)
		if err != nil {
			return err
		}
		link = pendingCode{channel: channelEmail, msg: s.linkMessage(u, linkActivate, channelEmail, email, url, ttl)}
		return nil
	})
	if err != nil {
		return CreatedUser{}, err
	}
	emailed := false
	if s.cfg().Recovery.EmailEnabled {
		s.deliver(ctx, link)
		emailed = true
	}
	return CreatedUser{User: u, URL: url, ExpiresAt: s.now().Add(ttl), Emailed: emailed}, nil
}
