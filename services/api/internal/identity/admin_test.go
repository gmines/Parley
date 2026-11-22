package identity

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"parley/services/api/internal/delivery"
)

func setRequireMFA(t *testing.T, s *Service, require bool) {
	t.Helper()
	if _, err := s.pool.Exec(context.Background(), `UPDATE server_settings SET value = jsonb_build_object('require', $1::boolean) WHERE key = 'mfa'`,
		require); err != nil {
		t.Fatal(err)
	}
}

// fullToken reports whether a session's access token works beyond MFA setup.
func fullToken(s *Service, sess Session) bool {
	_, err := s.cfg().Verifier.Verify(sess.AccessToken)
	return err == nil
}

func TestMFAPolicy(t *testing.T) {
	s, _ := newServiceWith(t, nil)
	ctx := context.Background()
	login := func(name string) Session {
		t.Helper()
		sess, err := s.Login(ctx, name, strongPassword, "10.0.0.1")
		if err != nil {
			t.Fatal(err)
		}
		return sess
	}
	mustRegister(t, s, Registration{Username: "ann"})
	bob := mustRegister(t, s, Registration{Username: "bob"})

	// Default: required for everyone.
	if sess := login("ann"); fullToken(s, sess) || !sess.User.MFARequired {
		t.Fatalf("default policy gave a full session: %+v", sess.User)
	}

	// Server default off.
	setRequireMFA(t, s, false)
	if sess := login("ann"); !fullToken(s, sess) || sess.User.MFARequired {
		t.Fatalf("with MFA optional, got a restricted session: %+v", sess.User)
	}
	// ...but a per-user requirement wins.
	if _, err := s.AdminSetMFAPolicy(ctx, bob.User.ID, MFAPolicyRequired); err != nil {
		t.Fatal(err)
	}
	if sess := login("bob"); fullToken(s, sess) || !sess.User.MFARequired {
		t.Fatal("required policy not enforced")
	}

	// Server default on, user exempt.
	setRequireMFA(t, s, true)
	if _, err := s.AdminSetMFAPolicy(ctx, bob.User.ID, MFAPolicyExempt); err != nil {
		t.Fatal(err)
	}
	sess := login("bob")
	if !fullToken(s, sess) || sess.User.MFARequired {
		t.Fatal("exempt user got a restricted session")
	}
	// Tightening reaches an existing session at its next refresh.
	if _, err := s.AdminSetMFAPolicy(ctx, bob.User.ID, MFAPolicyInherit); err != nil {
		t.Fatal(err)
	}
	next, err := s.Refresh(ctx, sess.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	if fullToken(s, next) {
		t.Fatal("refresh after tightening still gave a full token")
	}

	// An exempt user may still opt in, and then needs the second factor.
	if _, err := s.AdminSetMFAPolicy(ctx, bob.User.ID, MFAPolicyExempt); err != nil {
		t.Fatal(err)
	}
	enrollUser(t, s, login("bob"))
	if sess := login("bob"); !sess.MFARequired || sess.AccessToken != "" {
		t.Fatal("opted-in user wasn't challenged for a code")
	}

	if _, err := s.AdminSetMFAPolicy(ctx, bob.User.ID, "sometimes"); !errors.Is(err, ErrInvalidMFAPolicy) {
		t.Fatalf("bad policy: %v", err)
	}
}

func TestDisabledAccount(t *testing.T) {
	s, _ := newServiceWith(t, nil)
	ctx := context.Background()
	setRequireMFA(t, s, false)
	sess := mustRegister(t, s, Registration{Username: "cat"})

	if _, err := s.AdminSetDisabled(ctx, sess.User.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Refresh(ctx, sess.RefreshToken); !errors.Is(err, ErrInvalidRefresh) {
		t.Fatalf("refresh of a disabled account: %v", err)
	}
	if _, err := s.Login(ctx, "cat", strongPassword, "10.0.0.1"); !errors.Is(err, ErrAccountDisabled) {
		t.Fatalf("login to a disabled account: %v", err)
	}
	// A wrong password doesn't reveal that the account is disabled.
	if _, err := s.Login(ctx, "cat", "wrong-password-123", "10.0.0.1"); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("wrong password on a disabled account: %v", err)
	}
	if _, err := s.AdminSetDisabled(ctx, sess.User.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Login(ctx, "cat", strongPassword, "10.0.0.1"); err != nil {
		t.Fatalf("login after re-enabling: %v", err)
	}
}

func TestAdminResetLinks(t *testing.T) {
	s, _, email, _ := linkService(t)
	ctx := context.Background()
	nobody := mustRegister(t, s, Registration{Username: "dan"})
	withEmail := verifiedUser(t, s, "eve", "eve@example.org")

	// No verified contact: can't send, but can create a link to hand over.
	if _, err := s.AdminSendResetLink(ctx, nobody.User.ID); !errors.Is(err, ErrNoVerifiedContact) {
		t.Fatalf("send without contact: %v", err)
	}
	url, expires, err := s.AdminCreateResetLink(ctx, nobody.User.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(url, "https://account.example.org/account/set-password#t=") || time.Until(expires) < 23*time.Hour {
		t.Fatalf("link %q expires %v", url, expires)
	}
	token := linkRe.FindStringSubmatch(url)[1]
	if info, err := s.InspectLink(ctx, token); err != nil || info.Username != "dan" || info.Purpose != "reset_password" {
		t.Fatalf("inspect: %+v %v", info, err)
	}
	if _, err := s.CompleteLink(ctx, token, "harbor-violet-engine-77", "", ""); err != nil {
		t.Fatalf("complete admin link: %v", err)
	}
	if _, err := s.Login(ctx, "dan", "harbor-violet-engine-77", "10.0.0.1"); err != nil {
		t.Fatalf("login with new password: %v", err)
	}
	if _, err := s.CompleteLink(ctx, token, "another-password-88", "", ""); !errors.Is(err, ErrInvalidLink) {
		t.Fatalf("admin link reused: %v", err)
	}

	// With a verified email, the link is sent there.
	channel, err := s.AdminSendResetLink(ctx, withEmail.User.ID)
	if err != nil || channel != "email" {
		t.Fatalf("send: %q %v", channel, err)
	}
	lastLink(t, email, "eve@example.org")

	// A disabled account can't be reset, and its outstanding links die.
	url, _, err = s.AdminCreateResetLink(ctx, withEmail.User.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdminSetDisabled(ctx, withEmail.User.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CompleteLink(ctx, linkRe.FindStringSubmatch(url)[1], "harbor-violet-engine-77", "", ""); !errors.Is(err, ErrInvalidLink) {
		t.Fatalf("link for disabled account: %v", err)
	}
	if _, _, err := s.AdminCreateResetLink(ctx, withEmail.User.ID); !errors.Is(err, ErrAccountDisabled) {
		t.Fatalf("create for disabled account: %v", err)
	}
}

func TestAdminResetLinkNeedsAccountPage(t *testing.T) {
	s, _ := newServiceWith(t, nil)
	sess := mustRegister(t, s, Registration{Username: "fay"})
	if _, _, err := s.AdminCreateResetLink(context.Background(), sess.User.ID); !errors.Is(err, ErrNoAccountPage) {
		t.Fatalf("got %v", err)
	}
}

func TestAdminSetContact(t *testing.T) {
	s, _, email, sms := linkService(t)
	ctx := context.Background()
	gus := verifiedUser(t, s, "gus", "old@example.org")
	other := verifiedUser(t, s, "hal", "taken@example.org")
	_ = other

	// Marked verified at once, and the old address is told.
	u, err := s.AdminSetContact(ctx, gus.User.ID, "email", "  New@Example.org ", true)
	if err != nil {
		t.Fatal(err)
	}
	if u.Email != "new@example.org" || !u.EmailVerified {
		t.Fatalf("after set: %+v", u)
	}
	if m, ok := email.Last("old@example.org"); !ok || !strings.Contains(m.Body, "administrator changed") {
		t.Fatalf("old address not notified: %+v", m)
	}

	// Another account's verified address can't be taken.
	if _, err := s.AdminSetContact(ctx, gus.User.ID, "email", "taken@example.org", true); !errors.Is(err, ErrContactInUse) {
		t.Fatalf("taken address: %v", err)
	}
	if _, err := s.AdminSetContact(ctx, gus.User.ID, "email", "not-an-address", true); !errors.Is(err, ErrInvalidEmail) {
		t.Fatalf("invalid: %v", err)
	}

	// Unverified: the user gets a code, and the app's verify flow finishes it.
	u, err = s.AdminSetContact(ctx, gus.User.ID, "sms", "+1 (555) 010-2030", false)
	if err != nil {
		t.Fatal(err)
	}
	if u.Phone != "+15550102030" || u.PhoneVerified {
		t.Fatalf("phone: %+v", u)
	}
	if u, err = s.VerifyContact(ctx, gus.User.ID, "sms", lastCode(t, sms, "+15550102030")); err != nil || !u.PhoneVerified {
		t.Fatalf("verify: %+v %v", u, err)
	}

	// Removal.
	if u, err = s.AdminSetContact(ctx, gus.User.ID, "email", "", false); err != nil || u.Email != "" || u.EmailVerified {
		t.Fatalf("remove: %+v %v", u, err)
	}
}

func TestAdminSetContactVerifiedWorksWithDeliveryOff(t *testing.T) {
	s, _ := newServiceWith(t, nil)
	ctx := context.Background()
	sess := mustRegister(t, s, Registration{Username: "ivy"})
	if _, err := s.AdminSetContact(ctx, sess.User.ID, "email", "ivy@example.org", false); !errors.Is(err, ErrFeatureDisabled) {
		t.Fatalf("unverified with email off: %v", err)
	}
	if u, err := s.AdminSetContact(ctx, sess.User.ID, "email", "ivy@example.org", true); err != nil || !u.EmailVerified {
		t.Fatalf("verified with email off: %+v %v", u, err)
	}
}

func TestSetRuntime(t *testing.T) {
	s, _ := newServiceWith(t, nil)
	ctx := context.Background()
	rec := &delivery.Recorder{}
	r := s.Runtime()
	r.AccessTTL = 7 * time.Minute
	r.RefreshTTL = 3 * time.Hour
	r.Lockout = LockoutConfig{MaxFailures: 2, Duration: time.Minute, Window: time.Hour, Scope: ScopeAccount}
	r.Recovery.EmailEnabled, r.Recovery.Email, r.Recovery.AccountURL = true, rec, "https://account.example.org"
	s.SetRuntime(r)

	if !s.Features().EmailRecovery {
		t.Fatal("email not enabled by SetRuntime")
	}
	setRequireMFA(t, s, false)
	mustRegister(t, s, Registration{Username: "jo"})
	sess, err := s.Login(ctx, "jo", strongPassword, "10.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Until(sess.AccessExpiresAt); d < 6*time.Minute || d > 8*time.Minute {
		t.Fatalf("access TTL %v, want ~7m", d)
	}
	if d := time.Until(sess.RefreshExpiresAt); d < 2*time.Hour || d > 4*time.Hour {
		t.Fatalf("refresh TTL %v, want ~3h", d)
	}
	for range 2 {
		s.Login(ctx, "jo", "wrong-password-xyz", "10.0.0.2")
	}
	if _, err := s.Login(ctx, "jo", strongPassword, "10.0.0.3"); !errors.Is(err, ErrLockedOut) {
		t.Fatalf("lockout from runtime settings: %v", err)
	}
}

func TestAdminCreateUser(t *testing.T) {
	s, _, email, _ := linkService(t)
	ctx := context.Background()

	// Create an account; the welcome email carries the activation link.
	res, err := s.AdminCreateUser(ctx, NewUser{Username: "  Ivy ", DisplayName: " Ivy Jones ", Email: "Ivy@Example.org"})
	if err != nil {
		t.Fatal(err)
	}
	if res.User.Username != "ivy" || res.User.DisplayName != "Ivy Jones" || !res.Emailed {
		t.Fatalf("created %+v emailed=%v", res.User, res.Emailed)
	}
	if !strings.HasPrefix(res.URL, "https://account.example.org/account/set-password#t=") {
		t.Fatalf("url %q", res.URL)
	}
	m, ok := email.Last("ivy@example.org")
	if !ok || !strings.Contains(m.Body, res.URL) {
		t.Fatalf("welcome email: %v %q", ok, m.Body)
	}

	// Can't sign in until activated; the link activates, verifies email, sets the password.
	if _, err := s.Login(ctx, "ivy", strongPassword, ipA); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("login before activation: %v", err)
	}
	if _, err := s.CompleteLink(ctx, linkRe.FindStringSubmatch(res.URL)[1], strongPassword, "", ""); err != nil {
		t.Fatalf("activate: %v", err)
	}
	if _, err := s.Login(ctx, "ivy", strongPassword, ipA); err != nil {
		t.Fatalf("login after activation: %v", err)
	}

	// Duplicate username and duplicate email are reported plainly.
	if _, err := s.AdminCreateUser(ctx, NewUser{Username: "ivy", Email: "other@example.org"}); !errors.Is(err, ErrUsernameTaken) {
		t.Fatalf("dup username: %v", err)
	}
	if _, err := s.AdminCreateUser(ctx, NewUser{Username: "ivy2", Email: "ivy@example.org"}); !errors.Is(err, ErrContactInUse) {
		t.Fatalf("dup email: %v", err)
	}

	// Bad input.
	if _, err := s.AdminCreateUser(ctx, NewUser{Username: "x", Email: "a@example.org"}); !errors.Is(err, ErrInvalidUsername) {
		t.Fatalf("bad username: %v", err)
	}
	if _, err := s.AdminCreateUser(ctx, NewUser{Username: "noemail", Email: ""}); !errors.Is(err, ErrInvalidEmail) {
		t.Fatalf("missing email: %v", err)
	}
}

func TestAdminCreateUserDeliveryOff(t *testing.T) {
	s, _ := newServiceWith(t, func(c *Config) { c.Recovery.AccountURL = "https://account.example.org" })
	ctx := context.Background()
	// Email delivery is off: no message is sent, but the link comes back to hand over.
	res, err := s.AdminCreateUser(ctx, NewUser{Username: "jack", Email: "jack@example.org"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Emailed {
		t.Fatal("reported emailed with delivery off")
	}
	if _, err := s.CompleteLink(ctx, linkRe.FindStringSubmatch(res.URL)[1], strongPassword, "", ""); err != nil {
		t.Fatalf("activate handed-over link: %v", err)
	}
	if _, err := s.Login(ctx, "jack", strongPassword, ipA); err != nil {
		t.Fatalf("login: %v", err)
	}
}

func TestAdminCreateUserNeedsAccountPage(t *testing.T) {
	s, _ := newServiceWith(t, nil)
	if _, err := s.AdminCreateUser(context.Background(), NewUser{Username: "kim", Email: "kim@example.org"}); !errors.Is(err, ErrNoAccountPage) {
		t.Fatalf("got %v", err)
	}
}
