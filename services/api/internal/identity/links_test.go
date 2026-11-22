package identity

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"parley/services/api/internal/delivery"
)

func linkService(t *testing.T) (*Service, *fakeClock, *delivery.Recorder, *delivery.Recorder) {
	email, sms := &delivery.Recorder{}, &delivery.Recorder{}
	s, clock := newServiceWith(t, recoveryEnabled(email, sms))
	return s, clock, email, sms
}

func TestSignupActivation(t *testing.T) {
	s, _, email, _ := linkService(t)
	ctx := context.Background()
	if err := s.Signup(ctx, Signup{Username: "  Alice ", Email: "Alice@Example.org"}, ipA); err != nil {
		t.Fatal(err)
	}
	m, _ := email.Last("alice@example.org")
	if !strings.Contains(m.Body, "https://account.example.org/account/set-password#t=") {
		t.Fatalf("body = %q", m.Body)
	}
	token := lastLink(t, email, "alice@example.org")

	if _, err := s.Login(ctx, "alice", strongPassword, ipA); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("login before activation err = %v", err)
	}
	info, err := s.InspectLink(ctx, token)
	if err != nil || info.Purpose != linkActivate || info.Username != "alice" {
		t.Fatalf("inspect: %+v %v", info, err)
	}
	// Inspecting (or a scanner fetching the page) doesn't use it up, nor does a weak password.
	if err := s.CompleteLink(ctx, token, "12345678"); !errors.Is(err, ErrWeakPassword) {
		t.Fatalf("weak password err = %v", err)
	}
	if err := s.CompleteLink(ctx, token, strongPassword); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteLink(ctx, token, newPassword); !errors.Is(err, ErrInvalidLink) {
		t.Fatalf("reuse err = %v", err)
	}
	sess, err := s.Login(ctx, "alice", strongPassword, ipA)
	if err != nil || !sess.User.EmailVerified || sess.User.MFAEnabled {
		t.Fatalf("login: %+v %v", sess.User, err)
	}
}

func TestSignupByPhoneAndValidation(t *testing.T) {
	s, _, _, sms := linkService(t)
	ctx := context.Background()
	if err := s.Signup(ctx, Signup{Username: "bob", Phone: "+1 (555) 123-4567"}, ipA); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteLink(ctx, lastLink(t, sms, "+15551234567"), strongPassword); err != nil {
		t.Fatal(err)
	}
	sess, err := s.Login(ctx, "bob", strongPassword, ipA)
	if err != nil || !sess.User.PhoneVerified {
		t.Fatalf("login: %+v %v", sess.User, err)
	}
	for _, c := range []struct {
		in   Signup
		want error
	}{
		{Signup{Username: "a", Email: "a@example.org"}, ErrInvalidUsername},
		{Signup{Username: "has space", Email: "a@example.org"}, ErrInvalidUsername},
		{Signup{Username: "carl"}, ErrContactRequired},
		{Signup{Username: "carl", Email: "a@example.org", Phone: "+15551112222"}, ErrContactRequired},
		{Signup{Username: "carl", Email: "nope"}, ErrInvalidEmail},
		{Signup{Username: "carl", DisplayName: "bad\x00", Email: "a@example.org"}, ErrInvalidDisplayName},
		{Signup{Username: "bob", Email: "other@example.org"}, ErrUsernameTaken},
	} {
		if err := s.Signup(ctx, c.in, ipA); !errors.Is(err, c.want) {
			t.Errorf("Signup(%+v) err = %v, want %v", c.in, err, c.want)
		}
	}
}

func TestSignupDoesNotRevealAddresses(t *testing.T) {
	s, _, email, _ := linkService(t)
	ctx := context.Background()
	verifiedUser(t, s, "erin", "erin@example.org")
	before := len(email.Messages())
	if err := s.Signup(ctx, Signup{Username: "newbie", Email: "erin@example.org"}, ipA); err != nil {
		t.Fatalf("signup for used address err = %v, want silent success", err)
	}
	if len(email.Messages()) != before {
		t.Fatal("message sent to an address that already has an account")
	}
}

func TestSignupRateLimitsAndReuse(t *testing.T) {
	s, clock, email, _ := linkService(t)
	ctx := context.Background()
	addr := "dana@example.org"
	s.Signup(ctx, Signup{Username: "dana", Email: addr}, ipA)
	first := lastLink(t, email, addr)
	s.Signup(ctx, Signup{Username: "dana", Email: addr}, ipA) // within 60s: dropped
	if n := len(email.Messages()); n != 1 {
		t.Fatalf("sent %d, want 1", n)
	}
	// Retrying later may fix a typo in the username, and supersedes the old link.
	clock.advance(61 * time.Second)
	if err := s.Signup(ctx, Signup{Username: "dana2", Email: addr}, ipA); err != nil {
		t.Fatal(err)
	}
	if _, err := s.InspectLink(ctx, first); !errors.Is(err, ErrInvalidLink) {
		t.Fatalf("superseded link err = %v", err)
	}
	info, err := s.InspectLink(ctx, lastLink(t, email, addr))
	if err != nil || info.Username != "dana2" {
		t.Fatalf("inspect %+v %v", info, err)
	}
	for i := 0; i < 10; i++ {
		clock.advance(61 * time.Second)
		s.Signup(ctx, Signup{Username: "dana2", Email: addr}, ipA)
	}
	if n := len(email.Messages()); n != 5 {
		t.Fatalf("sent %d in an hour, want cap of 5", n)
	}
}

func TestSignupPerIPCap(t *testing.T) {
	s, _, _, _ := linkService(t)
	s.cfg().Recovery.MaxSignupsPerIPHour = 2
	ctx := context.Background()
	for i, name := range []string{"aa", "bb"} {
		if err := s.Signup(ctx, Signup{Username: name, Email: name + "@example.org"}, ipA); err != nil {
			t.Fatalf("signup %d: %v", i, err)
		}
	}
	if err := s.Signup(ctx, Signup{Username: "cc", Email: "cc@example.org"}, ipA); !errors.Is(err, ErrTooManyCodes) {
		t.Fatalf("over cap err = %v", err)
	}
	if err := s.Signup(ctx, Signup{Username: "cc", Email: "cc@example.org"}, ipB); err != nil {
		t.Fatalf("other address: %v", err)
	}
}

func TestAbandonedSignupReleasesUsername(t *testing.T) {
	s, clock, email, _ := linkService(t)
	ctx := context.Background()
	s.Signup(ctx, Signup{Username: "squat", Email: "a@example.org"}, ipA)
	token := lastLink(t, email, "a@example.org")
	if err := s.Signup(ctx, Signup{Username: "squat", Email: "b@example.org"}, ipB); !errors.Is(err, ErrUsernameTaken) {
		t.Fatalf("while pending err = %v", err)
	}
	clock.advance(25 * time.Hour)
	if err := s.CompleteLink(ctx, token, strongPassword); !errors.Is(err, ErrInvalidLink) {
		t.Fatalf("expired err = %v", err)
	}
	if err := s.Signup(ctx, Signup{Username: "squat", Email: "b@example.org"}, ipB); err != nil {
		t.Fatalf("after expiry: %v", err)
	}
}

func TestPasswordResetLink(t *testing.T) {
	s, clock, email, _ := linkService(t)
	ctx := context.Background()
	addr := "gina@example.org"
	first := verifiedUser(t, s, "gina", addr)

	if err := s.RequestPasswordReset(ctx, "GINA", ipA); err != nil {
		t.Fatal(err)
	}
	expired := lastLink(t, email, addr)
	clock.advance(31 * time.Minute)
	if err := s.CompleteLink(ctx, expired, newPassword); !errors.Is(err, ErrInvalidLink) {
		t.Fatalf("expired err = %v", err)
	}
	// By email address this time.
	if err := s.RequestPasswordReset(ctx, "Gina@Example.org", ipA); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteLink(ctx, lastLink(t, email, addr), newPassword); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Login(ctx, "gina", strongPassword, ipA); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("old password err = %v", err)
	}
	if _, err := s.Login(ctx, "gina", newPassword, ipA); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Refresh(ctx, first.RefreshToken); !errors.Is(err, ErrInvalidRefresh) {
		t.Fatalf("old session err = %v", err)
	}
	if m, _ := email.Last(addr); !strings.Contains(m.Subject, "password was changed") {
		t.Fatalf("no notice sent; last subject %q", m.Subject)
	}
}

func TestResetIgnoresUnverifiedAndUnknown(t *testing.T) {
	s, _, email, _ := linkService(t)
	ctx := context.Background()
	mustRegister(t, s, Registration{Username: "hal", Email: "hal@example.org"})
	for _, id := range []string{"hal", "hal@example.org", "nobody", "nobody@example.org", "+15550001111", ""} {
		if err := s.RequestPasswordReset(ctx, id, ipA); err != nil {
			t.Errorf("%q err = %v, want silent success", id, err)
		}
	}
	if n := len(email.Messages()); n != 0 {
		t.Fatalf("sent %d, want 0", n)
	}
}

func TestResetLinkDiesWithContactChange(t *testing.T) {
	s, _, email, _ := linkService(t)
	ctx := context.Background()
	addr := "ivy@example.org"
	sess := verifiedUser(t, s, "ivy", addr)
	s.RequestPasswordReset(ctx, "ivy", ipA)
	token := lastLink(t, email, addr)
	if _, err := s.RemoveContact(ctx, sess.User.ID, "email", strongPassword, ipA); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteLink(ctx, token, newPassword); !errors.Is(err, ErrInvalidLink) {
		t.Fatalf("err = %v", err)
	}
}

func TestResetClearsLockout(t *testing.T) {
	email := &delivery.Recorder{}
	s, clock := newServiceWith(t, func(c *Config) {
		recoveryEnabled(email, &delivery.Recorder{})(c)
		withLockout(ScopeAccount)(c)
	})
	ctx := context.Background()
	verifiedUser(t, s, "kay", "kay@example.org")
	failLogins(t, s, "kay", ipA, 5)
	clock.advance(2 * time.Minute)
	s.RequestPasswordReset(ctx, "kay", ipA)
	if err := s.CompleteLink(ctx, lastLink(t, email, "kay@example.org"), newPassword); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Login(ctx, "kay", newPassword, ipA); err != nil {
		t.Fatalf("login right after reset: %v", err)
	}
}
