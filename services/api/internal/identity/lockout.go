package identity

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"

	"parley/pkg/db"
)

// LockoutScope selects what failed logins are counted against.
type LockoutScope string

const (
	// ScopeAccountIP counts failures per (account, client IP): an attacker
	// locks out only themselves, not the real user on another network. A
	// separate, higher per-account cap (AccountMaxFailures) still applies
	// across all IPs to slow distributed guessing.
	ScopeAccountIP LockoutScope = "account_ip"
	// ScopeAccount counts all failures for an account together. Strongest
	// against guessing, but anyone who knows a username can keep it locked.
	ScopeAccount LockoutScope = "account"
)

type LockoutConfig struct {
	MaxFailures        int           // failures before locking; 0 disables lockout
	Duration           time.Duration // how long a lock lasts
	Window             time.Duration // failures older than this are forgotten
	Scope              LockoutScope
	AccountMaxFailures int // per-account cap across all IPs (ScopeAccountIP only); 0 disables
}

var DefaultLockout = LockoutConfig{
	MaxFailures:        5,
	Duration:           5 * time.Minute,
	Window:             15 * time.Minute,
	Scope:              ScopeAccountIP,
	AccountMaxFailures: 20,
}

var ErrLockedOut = errors.New("too many failed attempts")

// LockedError reports a lockout and when to retry.
type LockedError struct {
	RetryAfter time.Duration
}

func (e *LockedError) Error() string {
	mins := int(math.Ceil(e.RetryAfter.Minutes()))
	unit := "minutes"
	if mins == 1 {
		unit = "minute"
	}
	return fmt.Sprintf("Too many failed login attempts. Try again in %d %s.", mins, unit)
}

func (e *LockedError) Is(target error) bool { return target == ErrLockedOut }

type throttleKey struct {
	key string
	max int
}

func (s *Service) throttleKeys(username, ip string) []throttleKey {
	c := s.cfg.Lockout
	if c.MaxFailures <= 0 {
		return nil
	}
	if c.Scope == ScopeAccount {
		return []throttleKey{{"a:" + username, c.MaxFailures}}
	}
	keys := []throttleKey{{"aip:" + username + "|" + ip, c.MaxFailures}}
	if c.AccountMaxFailures > 0 {
		keys = append(keys, throttleKey{"a:" + username, c.AccountMaxFailures})
	}
	return keys
}

// checkLocked returns a *LockedError if any key is currently locked.
func (s *Service) checkLocked(ctx context.Context, keys []throttleKey) error {
	if len(keys) == 0 {
		return nil
	}
	names := make([]string, len(keys))
	for i, k := range keys {
		names[i] = k.key
	}
	var until *time.Time
	if err := s.pool.QueryRow(ctx,
		`SELECT max(locked_until) FROM login_throttle WHERE key = ANY($1) AND locked_until > $2`,
		names, s.now(),
	).Scan(&until); err != nil {
		return err
	}
	if until == nil {
		return nil
	}
	return &LockedError{RetryAfter: until.Sub(s.now())}
}

// recordFailure counts a failed attempt against every key and returns a
// *LockedError if this failure tripped a lock.
func (s *Service) recordFailure(ctx context.Context, keys []throttleKey) error {
	c := s.cfg.Lockout
	var locked *LockedError
	for _, k := range keys {
		err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
			now := s.now()
			if _, err := tx.Exec(ctx, `INSERT INTO login_throttle (key, failures, window_start)
				VALUES ($1, 0, $2) ON CONFLICT (key) DO NOTHING`, k.key, now); err != nil {
				return err
			}
			var (
				failures    int
				windowStart time.Time
				lockedUntil *time.Time
			)
			if err := tx.QueryRow(ctx, `SELECT failures, window_start, locked_until FROM login_throttle
				WHERE key = $1 FOR UPDATE`, k.key).Scan(&failures, &windowStart, &lockedUntil); err != nil {
				return err
			}
			// Start a fresh count after an expired lock or a quiet period.
			if (lockedUntil != nil && !lockedUntil.After(now)) || now.Sub(windowStart) > c.Window {
				failures, windowStart, lockedUntil = 0, now, nil
			}
			failures++
			if failures >= k.max {
				until := now.Add(c.Duration)
				lockedUntil = &until
				locked = &LockedError{RetryAfter: c.Duration}
				failures = 0
			}
			_, err := tx.Exec(ctx, `UPDATE login_throttle SET failures = $2, window_start = $3, locked_until = $4
				WHERE key = $1`, k.key, failures, windowStart, lockedUntil)
			return err
		})
		if err != nil {
			return err
		}
	}
	if locked != nil {
		return locked
	}
	return nil
}

// clearFailures forgets failures after a successful login. In ScopeAccountIP
// only the caller's (account, IP) key is cleared: the account-wide counter
// keeps tracking guesses from other addresses.
func (s *Service) clearFailures(ctx context.Context, keys []throttleKey) error {
	if len(keys) == 0 {
		return nil
	}
	_, err := s.pool.Exec(ctx, `DELETE FROM login_throttle WHERE key = $1`, keys[0].key)
	return err
}

// clearAllFailures unlocks an account entirely (e.g. after a password reset).
func clearAllFailures(ctx context.Context, q db.Querier, username string) error {
	// starts_with, not LIKE: usernames may contain '_', a LIKE wildcard.
	_, err := q.Exec(ctx, `DELETE FROM login_throttle WHERE key = $1 OR starts_with(key, $2)`,
		"a:"+username, "aip:"+username+"|")
	return err
}
