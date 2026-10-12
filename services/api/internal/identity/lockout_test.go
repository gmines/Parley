package identity

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

const ipA, ipB = "198.51.100.1", "198.51.100.2"

func withLockout(scope LockoutScope) func(*Config) {
	return func(c *Config) {
		c.Lockout = DefaultLockout
		c.Lockout.Scope = scope
	}
}

func failLogins(t *testing.T, s *Service, username, ip string, n int) error {
	t.Helper()
	var last error
	for i := 0; i < n; i++ {
		_, last = s.Login(context.Background(), username, "wrong-password-here", ip)
	}
	return last
}

func TestLockoutAfterFiveFailures(t *testing.T) {
	s, clock := newServiceWith(t, withLockout(ScopeAccountIP))
	ctx := context.Background()
	mustRegister(t, s, Registration{Username: "alice"})

	if err := failLogins(t, s, "alice", ipA, 4); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("after 4 failures err = %v, want ErrBadCredentials", err)
	}
	err := failLogins(t, s, "alice", ipA, 1)
	var locked *LockedError
	if !errors.As(err, &locked) || locked.RetryAfter != 5*time.Minute {
		t.Fatalf("5th failure err = %v, want LockedError(5m)", err)
	}

	// While locked, even the right password is refused.
	if _, err := s.Login(ctx, "alice", strongPassword, ipA); !errors.Is(err, ErrLockedOut) {
		t.Fatalf("correct password while locked err = %v", err)
	}
	clock.advance(4*time.Minute + 59*time.Second)
	if _, err := s.Login(ctx, "alice", strongPassword, ipA); !errors.Is(err, ErrLockedOut) {
		t.Fatalf("just before expiry err = %v", err)
	}
	clock.advance(2 * time.Second)
	if _, err := s.Login(ctx, "alice", strongPassword, ipA); err != nil {
		t.Fatalf("after lock expiry: %v", err)
	}

	// The count starts over after the lock.
	if err := failLogins(t, s, "alice", ipA, 4); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("fresh count after expiry err = %v", err)
	}
}

func TestLockoutScopeAccountIP(t *testing.T) {
	s, _ := newServiceWith(t, withLockout(ScopeAccountIP))
	mustRegister(t, s, Registration{Username: "alice"})

	failLogins(t, s, "alice", ipA, 5)
	// The attacker's IP is locked; the real user elsewhere is not.
	if _, err := s.Login(context.Background(), "alice", strongPassword, ipB); err != nil {
		t.Fatalf("login from another IP: %v", err)
	}
}

func TestLockoutScopeAccount(t *testing.T) {
	s, _ := newServiceWith(t, withLockout(ScopeAccount))
	mustRegister(t, s, Registration{Username: "alice"})

	failLogins(t, s, "alice", ipA, 5)
	if _, err := s.Login(context.Background(), "alice", strongPassword, ipB); !errors.Is(err, ErrLockedOut) {
		t.Fatalf("account scope, other IP err = %v, want locked", err)
	}
}

func TestLockoutAccountWideCap(t *testing.T) {
	s, _ := newServiceWith(t, withLockout(ScopeAccountIP))
	mustRegister(t, s, Registration{Username: "alice"})

	// Distributed guessing: 4 failures from each of 5 IPs never trips the
	// per-IP limit but reaches the account-wide cap of 20.
	for i := 0; i < 5; i++ {
		failLogins(t, s, "alice", fmt.Sprintf("203.0.113.%d", i), 4)
	}
	if _, err := s.Login(context.Background(), "alice", strongPassword, "192.0.2.99"); !errors.Is(err, ErrLockedOut) {
		t.Fatalf("after 20 distributed failures err = %v, want locked", err)
	}
}

func TestLockoutUnknownUserLooksTheSame(t *testing.T) {
	s, _ := newServiceWith(t, withLockout(ScopeAccountIP))
	if err := failLogins(t, s, "nobody", ipA, 4); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("unknown user err = %v", err)
	}
	if err := failLogins(t, s, "nobody", ipA, 1); !errors.Is(err, ErrLockedOut) {
		t.Fatalf("unknown user 5th failure err = %v, want locked like a real account", err)
	}
}

func TestLockoutSuccessResetsCount(t *testing.T) {
	s, _ := newServiceWith(t, withLockout(ScopeAccountIP))
	mustRegister(t, s, Registration{Username: "alice"})

	failLogins(t, s, "alice", ipA, 4)
	if _, err := s.Login(context.Background(), "alice", strongPassword, ipA); err != nil {
		t.Fatal(err)
	}
	if err := failLogins(t, s, "alice", ipA, 4); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("count not reset by success: %v", err)
	}
}

func TestLockoutOldFailuresForgotten(t *testing.T) {
	s, clock := newServiceWith(t, withLockout(ScopeAccountIP))
	mustRegister(t, s, Registration{Username: "alice"})

	failLogins(t, s, "alice", ipA, 4)
	clock.advance(16 * time.Minute) // beyond the 15m window
	if err := failLogins(t, s, "alice", ipA, 1); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("stale failures still counted: %v", err)
	}
}

func TestLockoutConcurrentFailures(t *testing.T) {
	s, _ := newServiceWith(t, withLockout(ScopeAccountIP))
	mustRegister(t, s, Registration{Username: "alice"})

	// 20 simultaneous bad logins must not slip past the limit through lost updates.
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.Login(context.Background(), "alice", "wrong-password-here", ipA)
		}()
	}
	wg.Wait()
	if _, err := s.Login(context.Background(), "alice", strongPassword, ipA); !errors.Is(err, ErrLockedOut) {
		t.Fatalf("after 20 concurrent failures err = %v, want locked", err)
	}
}

func TestLockoutDisabled(t *testing.T) {
	s, _ := newServiceWith(t, nil) // zero LockoutConfig
	mustRegister(t, s, Registration{Username: "alice"})
	if err := failLogins(t, s, "alice", ipA, 10); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("lockout disabled err = %v", err)
	}
}

func TestUsernameWildcardDoesNotClearOthers(t *testing.T) {
	s, _ := newServiceWith(t, withLockout(ScopeAccountIP))
	mustRegister(t, s, Registration{Username: "a_b"})
	mustRegister(t, s, Registration{Username: "axb"})

	failLogins(t, s, "axb", ipA, 5)
	if err := clearAllFailures(context.Background(), s.pool, "a_b"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Login(context.Background(), "axb", strongPassword, ipA); !errors.Is(err, ErrLockedOut) {
		t.Fatalf("clearing a_b unlocked axb: %v", err)
	}
}
