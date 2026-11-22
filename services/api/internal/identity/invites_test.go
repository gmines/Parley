package identity

import (
	"context"
	"errors"
	"strings"
	"testing"

	"parley/services/api/internal/delivery"
)

// inviteService builds a Service with email and invites enabled.
func inviteService(t *testing.T, dailyLimit int) (*Service, *delivery.Recorder) {
	t.Helper()
	email := &delivery.Recorder{}
	s, _ := newServiceWith(t, func(c *Config) {
		c.Recovery = RecoveryConfig{
			EmailEnabled:        true,
			Email:               email,
			SynchronousDelivery: true,
			AccountURL:          "https://account.example.org",
			InvitesEnabled:      true,
			InviteDailyLimit:    dailyLimit,
		}
	})
	return s, email
}

func TestInviteActivationPicksUsername(t *testing.T) {
	s, email := inviteService(t, 5)
	ctx := context.Background()
	inviter := verifiedUser(t, s, "amy", "amy@example.org")

	if err := s.Invite(ctx, inviter.User.ID, "Bob@Example.org", ipA); err != nil {
		t.Fatal(err)
	}
	m, ok := email.Last("bob@example.org")
	if !ok || !strings.Contains(m.Subject, "invited you") {
		t.Fatalf("invite message = %+v (ok=%v)", m, ok)
	}
	token := lastLink(t, email, "bob@example.org")

	// The invite hides the placeholder username and asks the invitee to pick one.
	info, err := s.InspectLink(ctx, token)
	if err != nil || info.Purpose != linkActivate || !info.PickUsername || info.Username != "" {
		t.Fatalf("inspect: %+v %v", info, err)
	}

	// A username is required for an invite.
	if _, err := s.CompleteLink(ctx, token, strongPassword, "", ""); !errors.Is(err, ErrInvalidUsername) {
		t.Fatalf("missing username err = %v", err)
	}

	res, err := s.CompleteLink(ctx, token, strongPassword, "  Bob_99 ", "Bobby")
	if err != nil {
		t.Fatal(err)
	}
	if res.InvitedBy == nil || *res.InvitedBy != inviter.User.ID {
		t.Fatalf("InvitedBy = %v, want %v", res.InvitedBy, inviter.User.ID)
	}

	sess, err := s.Login(ctx, "bob_99", strongPassword, ipA)
	if err != nil || !sess.User.EmailVerified {
		t.Fatalf("login: %+v %v", sess.User, err)
	}
	if sess.User.DisplayName != "Bobby" {
		t.Fatalf("display name = %q", sess.User.DisplayName)
	}
	if res.UserID != sess.User.ID {
		t.Fatalf("UserID = %v, want %v", res.UserID, sess.User.ID)
	}
}

func TestInviteDisabled(t *testing.T) {
	s, _ := inviteService(t, 5)
	// Turn invites back off.
	rt := s.Runtime()
	rt.Recovery.InvitesEnabled = false
	s.SetRuntime(rt)
	ctx := context.Background()
	inviter := verifiedUser(t, s, "amy", "amy@example.org")
	if err := s.Invite(ctx, inviter.User.ID, "bob@example.org", ipA); !errors.Is(err, ErrInvitesDisabled) {
		t.Fatalf("err = %v, want ErrInvitesDisabled", err)
	}
}

func TestInviteNeedsEmail(t *testing.T) {
	// Invites enabled but email off: the feature can't work.
	s, _ := newServiceWith(t, func(c *Config) {
		c.Recovery = RecoveryConfig{AccountURL: "https://account.example.org", InvitesEnabled: true}
	})
	ctx := context.Background()
	inviter := verifiedUser(t, s, "amy", "amy@example.org")
	if err := s.Invite(ctx, inviter.User.ID, "bob@example.org", ipA); !errors.Is(err, ErrFeatureDisabled) {
		t.Fatalf("err = %v, want ErrFeatureDisabled", err)
	}
}

func TestInviteDailyLimit(t *testing.T) {
	s, _ := inviteService(t, 1)
	ctx := context.Background()
	inviter := verifiedUser(t, s, "amy", "amy@example.org")
	if err := s.Invite(ctx, inviter.User.ID, "bob@example.org", ipA); err != nil {
		t.Fatal(err)
	}
	if err := s.Invite(ctx, inviter.User.ID, "carol@example.org", ipA); !errors.Is(err, ErrInviteLimit) {
		t.Fatalf("second invite err = %v, want ErrInviteLimit", err)
	}
}

func TestInvitePrivacyForExistingAccount(t *testing.T) {
	s, email := inviteService(t, 5)
	ctx := context.Background()
	inviter := verifiedUser(t, s, "amy", "amy@example.org")
	// carol already has an active, verified account.
	verifiedUser(t, s, "carol", "carol@example.org")

	if err := s.Invite(ctx, inviter.User.ID, "carol@example.org", ipA); err != nil {
		t.Fatalf("invite to existing address err = %v", err)
	}
	if _, ok := email.Last("carol@example.org"); ok {
		t.Fatal("an invite was sent to an address already in use")
	}
}

func TestInviteUsernameTakenAtActivation(t *testing.T) {
	s, email := inviteService(t, 5)
	ctx := context.Background()
	inviter := verifiedUser(t, s, "amy", "amy@example.org")
	if err := s.Invite(ctx, inviter.User.ID, "bob@example.org", ipA); err != nil {
		t.Fatal(err)
	}
	token := lastLink(t, email, "bob@example.org")
	// "amy" is taken by the inviter.
	if _, err := s.CompleteLink(ctx, token, strongPassword, "amy", ""); !errors.Is(err, ErrUsernameTaken) {
		t.Fatalf("err = %v, want ErrUsernameTaken", err)
	}
	// The link survives a rejected username, so a different one still works.
	if _, err := s.CompleteLink(ctx, token, strongPassword, "bob", ""); err != nil {
		t.Fatalf("retry with free username: %v", err)
	}
}
