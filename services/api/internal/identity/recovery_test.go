package identity

import (
	"context"
	"errors"
	"testing"

	"parley/services/api/internal/delivery"
)

const newPassword = "velvet-thunder-pine"

func TestRecoveryDisabledByDefault(t *testing.T) {
	s, _ := newServiceWith(t, nil)
	ctx := context.Background()
	if f := s.Features(); f.EmailRecovery || f.SMSRecovery || f.Signup {
		t.Fatalf("features = %+v, want all off", f)
	}
	if err := s.Signup(ctx, Signup{Username: "alice", Email: "a@example.org"}, ipA); !errors.Is(err, ErrFeatureDisabled) {
		t.Fatalf("signup err = %v", err)
	}
	if err := s.RequestPasswordReset(ctx, "alice", ipA); !errors.Is(err, ErrFeatureDisabled) {
		t.Fatalf("reset err = %v", err)
	}
	sess := mustRegister(t, s, Registration{Username: "alice"})
	for _, ch := range []string{"email", "sms"} {
		if err := s.SetContact(ctx, sess.User.ID, ch, "x", strongPassword, ipA); !errors.Is(err, ErrFeatureDisabled) {
			t.Errorf("set contact %s err = %v", ch, err)
		}
	}
}

func TestPhoneContactFlow(t *testing.T) {
	email, sms := &delivery.Recorder{}, &delivery.Recorder{}
	s, _ := newServiceWith(t, recoveryEnabled(email, sms))
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
	u, err = s.RemoveContact(ctx, sess.User.ID, "sms", strongPassword, ipA)
	if err != nil || u.Phone != "" || u.PhoneVerified {
		t.Fatalf("remove phone: %+v %v", u, err)
	}
}

func TestVerifiedContactMustBeUnique(t *testing.T) {
	email := &delivery.Recorder{}
	s, _ := newServiceWith(t, recoveryEnabled(email, &delivery.Recorder{}))
	ctx := context.Background()
	verifiedUser(t, s, "erin", "shared@example.org")

	// A second account may enter the address, but can't verify it.
	sess := mustRegister(t, s, Registration{Username: "frank"})
	if err := s.SetContact(ctx, sess.User.ID, "email", "shared@example.org", strongPassword, ipA); err != nil {
		t.Fatal(err)
	}
	if _, err := s.VerifyContact(ctx, sess.User.ID, "email", lastCode(t, email, "shared@example.org")); !errors.Is(err, ErrContactInUse) {
		t.Fatalf("duplicate verified email err = %v", err)
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
