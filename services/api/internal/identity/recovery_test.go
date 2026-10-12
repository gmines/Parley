package identity

import (
	"context"
	"errors"
	"testing"
	"time"

	"parley/services/api/internal/delivery"
)

const newPassword = "velvet-thunder-pine"

func TestRecoveryDisabledByDefault(t *testing.T) {
	s, _ := newServiceWith(t, nil)
	ctx := context.Background()

	if f := s.Features(); f.EmailRecovery || f.SMSRecovery {
		t.Fatalf("features = %+v, want all off", f)
	}
	if _, err := s.Register(ctx, Registration{Username: "alice", Password: strongPassword, Email: "a@example.org"}); !errors.Is(err, ErrFeatureDisabled) {
		t.Fatalf("register with email err = %v", err)
	}
	if _, err := s.Register(ctx, Registration{Username: "alice", Password: strongPassword, Phone: "+15551234567"}); !errors.Is(err, ErrFeatureDisabled) {
		t.Fatalf("register with phone err = %v", err)
	}
	sess := mustRegister(t, s, Registration{Username: "alice"})
	for _, ch := range []string{"email", "sms"} {
		if err := s.RequestPasswordReset(ctx, "alice", ch); !errors.Is(err, ErrFeatureDisabled) {
			t.Errorf("reset request %s err = %v", ch, err)
		}
		if err := s.SetContact(ctx, sess.User.ID, ch, "x", strongPassword, ipA); !errors.Is(err, ErrFeatureDisabled) {
			t.Errorf("set contact %s err = %v", ch, err)
		}
	}
	if err := s.ConfirmPasswordReset(ctx, "alice", "123456", newPassword); !errors.Is(err, ErrFeatureDisabled) {
		t.Fatalf("confirm err = %v", err)
	}
}

func TestEmailPasswordResetFlow(t *testing.T) {
	email, sms := &delivery.Recorder{}, &delivery.Recorder{}
	s, clock := newServiceWith(t, recoveryEnabled(email, sms))
	ctx := context.Background()

	first := mustRegister(t, s, Registration{Username: "alice", Email: " Alice@Example.org "})
	if first.User.Email != "alice@example.org" || first.User.EmailVerified {
		t.Fatalf("user after register = %+v, want normalized, unverified email", first.User)
	}

	// Unverified addresses can't be used for reset.
	clock.advance(2 * time.Minute)
	if err := s.RequestPasswordReset(ctx, "alice", "email"); err != nil {
		t.Fatal(err)
	}
	if n := len(email.Messages()); n != 1 {
		t.Fatalf("messages = %d, want only the verification email", n)
	}

	if _, err := s.VerifyContact(ctx, first.User.ID, "email", "000000"); !errors.Is(err, ErrInvalidCode) {
		t.Fatalf("wrong verification code err = %v", err)
	}
	u, err := s.VerifyContact(ctx, first.User.ID, "email", lastCode(t, email, "alice@example.org"))
	if err != nil || !u.EmailVerified {
		t.Fatalf("verify: user %+v err %v", u, err)
	}

	if err := s.RequestPasswordReset(ctx, "ALICE", "email"); err != nil {
		t.Fatal(err)
	}
	code := lastCode(t, email, "alice@example.org")

	if err := s.ConfirmPasswordReset(ctx, "alice", "999999", newPassword); !errors.Is(err, ErrInvalidCode) {
		t.Fatalf("wrong reset code err = %v", err)
	}
	// A weak new password is rejected without burning the code.
	if err := s.ConfirmPasswordReset(ctx, "alice", code, "12345678"); !errors.Is(err, ErrWeakPassword) {
		t.Fatalf("weak new password err = %v", err)
	}
	if err := s.ConfirmPasswordReset(ctx, "alice", code, newPassword); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if err := s.ConfirmPasswordReset(ctx, "alice", code, "another-fine-phrase"); !errors.Is(err, ErrInvalidCode) {
		t.Fatalf("code reuse err = %v", err)
	}

	if _, err := s.Login(ctx, "alice", strongPassword, ipA); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("old password err = %v", err)
	}
	if _, err := s.Login(ctx, "alice", newPassword, ipA); err != nil {
		t.Fatalf("new password: %v", err)
	}
	// Every pre-reset session is signed out.
	if _, err := s.Refresh(ctx, first.RefreshToken); !errors.Is(err, ErrInvalidRefresh) {
		t.Fatalf("old refresh token err = %v", err)
	}
}

func TestSMSResetAndPhoneNormalization(t *testing.T) {
	email, sms := &delivery.Recorder{}, &delivery.Recorder{}
	s, clock := newServiceWith(t, recoveryEnabled(email, sms))
	ctx := context.Background()
	sess := mustRegister(t, s, Registration{Username: "bob"})

	if err := s.SetContact(ctx, sess.User.ID, "sms", "555-1234", strongPassword, ipA); !errors.Is(err, ErrInvalidPhone) {
		t.Fatalf("non-E.164 phone err = %v", err)
	}
	if err := s.SetContact(ctx, sess.User.ID, "sms", "+1 (555) 123-4567", "wrong-password-here", ipA); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("set contact with wrong password err = %v", err)
	}
	if err := s.SetContact(ctx, sess.User.ID, "sms", "+1 (555) 123-4567", strongPassword, ipA); err != nil {
		t.Fatal(err)
	}
	u, err := s.VerifyContact(ctx, sess.User.ID, "sms", lastCode(t, sms, "+15551234567"))
	if err != nil || u.Phone != "+15551234567" || !u.PhoneVerified {
		t.Fatalf("verify phone: %+v %v", u, err)
	}

	clock.advance(2 * time.Minute)
	if err := s.RequestPasswordReset(ctx, "bob", "sms"); err != nil {
		t.Fatal(err)
	}
	if err := s.ConfirmPasswordReset(ctx, "bob", lastCode(t, sms, "+15551234567"), newPassword); err != nil {
		t.Fatal(err)
	}
	// No email on this account, so an email reset sends nothing.
	if err := s.RequestPasswordReset(ctx, "bob", "email"); err != nil || len(email.Messages()) != 0 {
		t.Fatalf("email reset without email: err %v, messages %d", err, len(email.Messages()))
	}

	u, err = s.RemoveContact(ctx, sess.User.ID, "sms", newPassword, ipA)
	if err != nil || u.Phone != "" || u.PhoneVerified {
		t.Fatalf("remove phone: %+v %v", u, err)
	}
}

func verifiedUser(t *testing.T, s *Service, rec *delivery.Recorder, username, addr string) Session {
	t.Helper()
	sess := mustRegister(t, s, Registration{Username: username, Email: addr})
	if _, err := s.VerifyContact(context.Background(), sess.User.ID, "email", lastCode(t, rec, addr)); err != nil {
		t.Fatal(err)
	}
	return sess
}

func TestResetCodeAttemptLimitAndExpiry(t *testing.T) {
	email := &delivery.Recorder{}
	s, clock := newServiceWith(t, recoveryEnabled(email, &delivery.Recorder{}))
	ctx := context.Background()
	verifiedUser(t, s, email, "carol", "carol@example.org")

	clock.advance(2 * time.Minute)
	s.RequestPasswordReset(ctx, "carol", "email")
	code := lastCode(t, email, "carol@example.org")
	for i := 0; i < 5; i++ {
		s.ConfirmPasswordReset(ctx, "carol", "000000", newPassword)
	}
	if err := s.ConfirmPasswordReset(ctx, "carol", code, newPassword); !errors.Is(err, ErrInvalidCode) {
		t.Fatalf("right code after 5 wrong guesses err = %v, want exhausted", err)
	}

	clock.advance(2 * time.Minute)
	s.RequestPasswordReset(ctx, "carol", "email")
	code = lastCode(t, email, "carol@example.org")
	clock.advance(15*time.Minute + time.Second)
	if err := s.ConfirmPasswordReset(ctx, "carol", code, newPassword); !errors.Is(err, ErrInvalidCode) {
		t.Fatalf("expired code err = %v", err)
	}
}

func TestResetRequestRateLimitsAndUnknownUsers(t *testing.T) {
	email := &delivery.Recorder{}
	s, clock := newServiceWith(t, recoveryEnabled(email, &delivery.Recorder{}))
	ctx := context.Background()
	verifiedUser(t, s, email, "dana", "dana@example.org")
	base := len(email.Messages())

	if err := s.RequestPasswordReset(ctx, "nobody", "email"); err != nil {
		t.Fatalf("unknown user err = %v, want silent success", err)
	}
	if err := s.RequestPasswordReset(ctx, "dana", "fax"); !errors.Is(err, ErrInvalidChannel) {
		t.Fatalf("bad channel err = %v", err)
	}

	sent := func() int { return len(email.Messages()) - base }
	s.RequestPasswordReset(ctx, "dana", "email")
	s.RequestPasswordReset(ctx, "dana", "email") // within 60s: silently dropped
	if sent() != 1 {
		t.Fatalf("sent %d, want 1 (resend interval)", sent())
	}
	for i := 0; i < 10; i++ {
		clock.advance(61 * time.Second)
		s.RequestPasswordReset(ctx, "dana", "email")
	}
	if sent() != 5 {
		t.Fatalf("sent %d within an hour, want cap of 5", sent())
	}
	// Only the newest code works.
	if err := s.ConfirmPasswordReset(ctx, "dana", lastCode(t, email, "dana@example.org"), newPassword); err != nil {
		t.Fatal(err)
	}
}

func TestVerifiedContactMustBeUnique(t *testing.T) {
	email := &delivery.Recorder{}
	s, _ := newServiceWith(t, recoveryEnabled(email, &delivery.Recorder{}))
	verifiedUser(t, s, email, "erin", "shared@example.org")

	// A second account may enter the address, but can't verify it.
	sess := mustRegister(t, s, Registration{Username: "frank", Email: "shared@example.org"})
	if _, err := s.VerifyContact(context.Background(), sess.User.ID, "email", lastCode(t, email, "shared@example.org")); !errors.Is(err, ErrContactInUse) {
		t.Fatalf("duplicate verified email err = %v", err)
	}
}

func TestResetClearsLockout(t *testing.T) {
	email := &delivery.Recorder{}
	s, clock := newServiceWith(t, func(c *Config) {
		recoveryEnabled(email, &delivery.Recorder{})(c)
		withLockout(ScopeAccount)(c)
	})
	ctx := context.Background()
	verifiedUser(t, s, email, "gina", "gina@example.org")

	failLogins(t, s, "gina", ipA, 5)
	clock.advance(2 * time.Minute)
	s.RequestPasswordReset(ctx, "gina", "email")
	if err := s.ConfirmPasswordReset(ctx, "gina", lastCode(t, email, "gina@example.org"), newPassword); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Login(ctx, "gina", newPassword, ipA); err != nil {
		t.Fatalf("login right after reset: %v", err)
	}
}

func TestChangePassword(t *testing.T) {
	s, _ := newServiceWith(t, withLockout(ScopeAccountIP))
	ctx := context.Background()
	old := mustRegister(t, s, Registration{Username: "hank"})
	other, err := s.Login(ctx, "hank", strongPassword, ipB)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.ChangePassword(ctx, old.User.ID, "wrong-password-here", newPassword, ipA); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("wrong current password err = %v", err)
	}
	if _, err := s.ChangePassword(ctx, old.User.ID, strongPassword, "12345678", ipA); !errors.Is(err, ErrWeakPassword) {
		t.Fatalf("weak new password err = %v", err)
	}
	sess, err := s.ChangePassword(ctx, old.User.ID, strongPassword, newPassword, ipA)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Refresh(ctx, sess.RefreshToken); err != nil {
		t.Fatalf("new session refresh: %v", err)
	}
	for name, tok := range map[string]string{"this device": old.RefreshToken, "other device": other.RefreshToken} {
		if _, err := s.Refresh(ctx, tok); !errors.Is(err, ErrInvalidRefresh) {
			t.Errorf("%s session survived password change: %v", name, err)
		}
	}

	// Wrong current passwords count towards the lockout.
	for i := 0; i < 5; i++ {
		_, err = s.ChangePassword(ctx, old.User.ID, "wrong-password-here", "another-fine-phrase", ipA)
	}
	if !errors.Is(err, ErrLockedOut) {
		t.Fatalf("after 5 wrong current passwords err = %v, want locked", err)
	}
}
