package identity

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"parley/pkg/auth"
	"parley/pkg/db/dbtest"
	"parley/services/api/internal/delivery"
	"parley/services/api/migrations"
)

const strongPassword = "plum-orbit-candle-42"

// fakeClock is a settable time source shared with the Service.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// newServiceWith builds a Service on a fresh schema with a fake clock.
// configure may adjust the config (lockout, recovery) before construction.
func newServiceWith(t *testing.T, configure func(*Config)) (*Service, *fakeClock) {
	t.Helper()
	pool := dbtest.New(t, migrations.FS)
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	cfg := Config{Signer: auth.NewSigner(priv, time.Minute), Verifier: auth.NewVerifier(pub), MFAKey: make([]byte, 32), RefreshTTL: time.Hour, Hash: testHash}
	if configure != nil {
		configure(&cfg)
	}
	s, err := NewService(pool, cfg)
	if err != nil {
		t.Fatal(err)
	}
	clock := &fakeClock{t: time.Now()}
	s.now = clock.now
	return s, clock
}

// Registration describes an already-active account for tests that aren't
// about sign-up. Email and Phone are stored unverified.
type Registration struct {
	Username, Password, DisplayName, Email, Phone string
}

// mustRegister creates an active account without MFA, as migrated accounts are.
func mustRegister(t *testing.T, s *Service, in Registration) Session {
	t.Helper()
	if in.Password == "" {
		in.Password = strongPassword
	}
	if in.DisplayName == "" {
		in.DisplayName = in.Username
	}
	hash, err := hashPassword(in.Password, s.cfg.Hash)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	var u User
	if err := s.pool.QueryRow(ctx, `INSERT INTO users (username, display_name, password_hash, email, phone, activated_at)
		VALUES ($1, $2, $3, $4, $5, now()) RETURNING `+userCols("users"),
		in.Username, in.DisplayName, hash, nullable(in.Email), nullable(in.Phone)).Scan(userDest(&u)...); err != nil {
		t.Fatalf("register %s: %v", in.Username, err)
	}
	sess, err := s.newSession(ctx, s.pool, u, uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	return sess
}

// verifiedUser is mustRegister with a verified email address.
func verifiedUser(t *testing.T, s *Service, username, addr string) Session {
	t.Helper()
	sess := mustRegister(t, s, Registration{Username: username, Email: addr})
	if _, err := s.pool.Exec(context.Background(), `UPDATE users SET email_verified_at = now() WHERE id = $1`, sess.User.ID); err != nil {
		t.Fatal(err)
	}
	sess.User.EmailVerified = true
	return sess
}

var linkRe = regexp.MustCompile(`#t=([A-Za-z0-9_-]{43})`)

// lastLink extracts the token from the newest message sent to `to`.
func lastLink(t *testing.T, r *delivery.Recorder, to string) string {
	t.Helper()
	m, ok := r.Last(to)
	if !ok {
		t.Fatalf("no message sent to %s", to)
	}
	match := linkRe.FindStringSubmatch(m.Body)
	if match == nil {
		t.Fatalf("no link in message body %q", m.Body)
	}
	return match[1]
}

// recoveryEnabled turns on both channels with in-memory senders.
func recoveryEnabled(email, sms *delivery.Recorder) func(*Config) {
	return func(c *Config) {
		c.Recovery = RecoveryConfig{
			EmailEnabled: true, Email: email,
			SMSEnabled: true, SMS: sms,
			SynchronousDelivery: true,
			AccountURL:          "https://account.example.org",
		}
	}
}

var codeRe = regexp.MustCompile(`\b(\d{6})\b`)

// lastCode extracts the 6-digit code from the newest message sent to `to`.
func lastCode(t *testing.T, r *delivery.Recorder, to string) string {
	t.Helper()
	m, ok := r.Last(to)
	if !ok {
		t.Fatalf("no message sent to %s", to)
	}
	match := codeRe.FindStringSubmatch(m.Body)
	if match == nil {
		t.Fatalf("no code in message body %q", m.Body)
	}
	return match[1]
}
