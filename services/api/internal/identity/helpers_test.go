package identity

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"regexp"
	"sync"
	"testing"
	"time"

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
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	cfg := Config{Signer: auth.NewSigner(priv, time.Minute), RefreshTTL: time.Hour, Hash: testHash}
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

func mustRegister(t *testing.T, s *Service, in Registration) Session {
	t.Helper()
	if in.Password == "" {
		in.Password = strongPassword
	}
	sess, err := s.Register(context.Background(), in)
	if err != nil {
		t.Fatalf("register %s: %v", in.Username, err)
	}
	return sess
}

// recoveryEnabled turns on both channels with in-memory senders.
func recoveryEnabled(email, sms *delivery.Recorder) func(*Config) {
	return func(c *Config) {
		c.Recovery = RecoveryConfig{
			EmailEnabled: true, Email: email,
			SMSEnabled: true, SMS: sms,
			SynchronousDelivery: true,
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
