package identity

import (
	"context"
	"errors"
	"testing"
	"time"

	"parley/services/api/internal/delivery"
	"parley/services/api/internal/totp"
)

func TestTOTPVector(t *testing.T) {
	// RFC 6238 appendix B, SHA-1, truncated to 6 digits.
	secret := []byte("12345678901234567890")
	for unix, want := range map[int64]string{59: "287082", 1111111109: "081804", 2000000000: "279037"} {
		if got := totp.Code(secret, unix/totp.Period); got != want {
			t.Errorf("t=%d got %s want %s", unix, got, want)
		}
	}
}

func enrollUser(t *testing.T, s *Service, sess Session) (string, Enrolled) {
	t.Helper()
	ctx := context.Background()
	e, err := s.StartEnrollment(ctx, sess.User.ID)
	if err != nil {
		t.Fatal(err)
	}
	code, _ := GenerateTOTP(e.Secret, s.now())
	out, err := s.ConfirmEnrollment(ctx, sess.User.ID, code, ipA)
	if err != nil {
		t.Fatal(err)
	}
	return e.Secret, out
}

func TestExistingUserIsForcedToEnroll(t *testing.T) {
	s, clock := newServiceWith(t, nil)
	ctx := context.Background()
	v := s.cfg().Verifier
	// Two devices signed in before MFA existed.
	a := mustRegister(t, s, Registration{Username: "alice"})
	b, err := s.Login(ctx, "alice", strongPassword, ipA)
	if err != nil {
		t.Fatal(err)
	}

	// Their next refresh is restricted; nothing else accepts it.
	r, err := s.Refresh(ctx, a.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.Verify(r.AccessToken); err == nil {
		t.Fatal("restricted token passed Verify")
	}
	if _, err := v.VerifyEnrollment(r.AccessToken); err != nil {
		t.Fatalf("VerifyEnrollment: %v", err)
	}

	if _, err := s.ConfirmEnrollment(ctx, r.User.ID, "000000", ipA); !errors.Is(err, ErrMFANotStarted) {
		t.Fatalf("confirm before start err = %v", err)
	}
	if _, err := s.StartEnrollment(ctx, r.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConfirmEnrollment(ctx, r.User.ID, "000000", ipA); !errors.Is(err, ErrInvalidMFACode) {
		t.Fatalf("wrong code err = %v", err)
	}
	clock.advance(time.Minute)
	secret, out := enrollUser(t, s, r)
	if !out.User.MFAEnabled || len(out.RecoveryCodes) != recoveryCodes {
		t.Fatalf("enrolled = %+v", out)
	}
	if _, err := v.Verify(out.AccessToken); err != nil {
		t.Fatalf("full token: %v", err)
	}
	_ = secret
	for name, tok := range map[string]string{"a": r.RefreshToken, "b": b.RefreshToken} {
		if _, err := s.Refresh(ctx, tok); !errors.Is(err, ErrInvalidRefresh) {
			t.Errorf("pre-MFA session %s survived enrollment: %v", name, err)
		}
	}
	if _, err := s.StartEnrollment(ctx, r.User.ID); !errors.Is(err, ErrMFAAlreadyEnabled) {
		t.Fatalf("re-enroll err = %v", err)
	}
	// The new session stays full across refresh.
	next, err := s.Refresh(ctx, out.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.Verify(next.AccessToken); err != nil {
		t.Fatalf("refreshed token: %v", err)
	}
}

func TestLoginRequiresSecondFactor(t *testing.T) {
	s, clock := newServiceWith(t, withLockout(ScopeAccountIP))
	ctx := context.Background()
	secret, out := enrollUser(t, s, mustRegister(t, s, Registration{Username: "bob"}))
	v := s.cfg().Verifier

	clock.advance(2 * time.Minute)
	ch, err := s.Login(ctx, "bob", strongPassword, ipA)
	if err != nil || !ch.MFARequired || ch.AccessToken != "" || ch.RefreshToken != "" {
		t.Fatalf("login: %+v %v", ch, err)
	}
	if _, err := v.Verify(ch.MFAToken); err == nil {
		t.Fatal("challenge token passed Verify")
	}
	if _, err := s.CompleteMFALogin(ctx, ch.MFAToken, "000000", ipA); !errors.Is(err, ErrInvalidMFACode) {
		t.Fatalf("wrong code err = %v", err)
	}
	code, _ := GenerateTOTP(secret, s.now())
	sess, err := s.CompleteMFALogin(ctx, ch.MFAToken, code, ipA)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.Verify(sess.AccessToken); err != nil {
		t.Fatalf("access token: %v", err)
	}
	// The same code can't be replayed.
	if _, err := s.CompleteMFALogin(ctx, ch.MFAToken, code, ipA); !errors.Is(err, ErrInvalidMFACode) {
		t.Fatalf("replay err = %v", err)
	}
	// Recovery codes work once, in any formatting.
	rc := out.RecoveryCodes[0]
	if _, err := s.CompleteMFALogin(ctx, ch.MFAToken, " "+rc+" ", ipA); err != nil {
		t.Fatalf("recovery code: %v", err)
	}
	if _, err := s.CompleteMFALogin(ctx, ch.MFAToken, rc, ipA); !errors.Is(err, ErrInvalidMFACode) {
		t.Fatalf("recovery reuse err = %v", err)
	}
	// Other token types aren't challenges, and challenges expire.
	if _, err := s.CompleteMFALogin(ctx, out.AccessToken, code, ipA); !errors.Is(err, ErrMFAChallenge) {
		t.Fatalf("access token as challenge err = %v", err)
	}
}

func TestMFAFailuresLockAndSurvivePasswordReset(t *testing.T) {
	email := &delivery.Recorder{}
	s, clock := newServiceWith(t, func(c *Config) {
		recoveryEnabled(email, &delivery.Recorder{})(c)
		withLockout(ScopeAccountIP)(c)
	})
	ctx := context.Background()
	addr := "carl@example.org"
	secret, _ := enrollUser(t, s, verifiedUser(t, s, "carl", addr))
	clock.advance(2 * time.Minute)
	ch, err := s.Login(ctx, "carl", strongPassword, ipA)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		_, err = s.CompleteMFALogin(ctx, ch.MFAToken, "000000", ipA)
	}
	if !errors.Is(err, ErrLockedOut) {
		t.Fatalf("after 5 wrong codes err = %v", err)
	}
	if err := s.RequestPasswordReset(ctx, "carl", ipA); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteLink(ctx, lastLink(t, email, addr), newPassword); err != nil {
		t.Fatal(err)
	}
	code, _ := GenerateTOTP(secret, s.now())
	if _, err := s.CompleteMFALogin(ctx, ch.MFAToken, code, ipA); !errors.Is(err, ErrLockedOut) {
		t.Fatalf("correct code while locked err = %v", err)
	}
	// And the reset didn't remove MFA.
	after, err := s.Login(ctx, "carl", newPassword, ipA)
	if err != nil || !after.MFARequired {
		t.Fatalf("login after reset: %+v %v", after, err)
	}
}

func TestResetMFA(t *testing.T) {
	s, clock := newServiceWith(t, nil)
	ctx := context.Background()
	first := mustRegister(t, s, Registration{Username: "dee"})
	secret, out := enrollUser(t, s, first)
	clock.advance(2 * time.Minute)
	code, _ := GenerateTOTP(secret, s.now())

	if _, err := s.ResetMFA(ctx, first.User.ID, "wrong-password-here", code, ipA); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("wrong password err = %v", err)
	}
	if _, err := s.ResetMFA(ctx, first.User.ID, strongPassword, "000000", ipA); !errors.Is(err, ErrInvalidMFACode) {
		t.Fatalf("wrong code err = %v", err)
	}
	sess, err := s.ResetMFA(ctx, first.User.ID, strongPassword, code, ipA)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.cfg().Verifier.VerifyEnrollment(sess.AccessToken); err != nil || sess.User.MFAEnabled {
		t.Fatalf("after reset: %+v %v", sess.User, err)
	}
	if _, err := s.Refresh(ctx, out.RefreshToken); !errors.Is(err, ErrInvalidRefresh) {
		t.Fatalf("old session err = %v", err)
	}
}

func TestAdminResetMFA(t *testing.T) {
	s, clock := newServiceWith(t, nil)
	ctx := context.Background()
	enrollUser(t, s, mustRegister(t, s, Registration{Username: "eve"}))
	clock.advance(time.Minute)
	if err := s.AdminResetMFA(ctx, "nobody"); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("unknown err = %v", err)
	}
	if err := s.AdminResetMFA(ctx, "eve"); err != nil {
		t.Fatal(err)
	}
	sess, err := s.Login(ctx, "eve", strongPassword, ipA)
	if err != nil || sess.MFARequired || sess.User.MFAEnabled {
		t.Fatalf("login: %+v %v", sess, err)
	}
}

func TestSealedSecretIsBoundToUser(t *testing.T) {
	s, _ := newServiceWith(t, nil)
	a := mustRegister(t, s, Registration{Username: "aa"})
	b := mustRegister(t, s, Registration{Username: "bb"})
	sealed, err := s.seal(a.User.ID, []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.open(b.User.ID, sealed); err == nil {
		t.Fatal("sealed secret opened for another user")
	}
	if got, err := s.open(a.User.ID, sealed); err != nil || string(got) != "secret" {
		t.Fatalf("open: %q %v", got, err)
	}
}
