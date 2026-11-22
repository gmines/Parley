package identity

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"testing"
	"time"

	"parley/pkg/auth"
	"parley/pkg/db/dbtest"
	"parley/services/api/migrations"
)

// Cheap parameters so tests stay fast; production uses DefaultHashParams.
var testHash = HashParams{Memory: 1024, Time: 1, Threads: 1}

func TestPasswordHashRoundTrip(t *testing.T) {
	h, err := hashPassword("correct horse", testHash)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := verifyPassword("correct horse", h); err != nil || !ok {
		t.Fatalf("verify correct password: ok=%v err=%v", ok, err)
	}
	if ok, _ := verifyPassword("wrong horse", h); ok {
		t.Fatal("wrong password verified")
	}
	if _, err := verifyPassword("x", "$bcrypt$nope"); !errors.Is(err, errBadHash) {
		t.Fatalf("malformed hash err = %v", err)
	}
}

func newService(t *testing.T) (*Service, *auth.Verifier) {
	pool := dbtest.New(t, migrations.FS)
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	s, err := NewService(pool, Config{Signer: auth.NewSigner(priv, time.Minute), Verifier: auth.NewVerifier(pub), MFAKey: make([]byte, 32), RefreshTTL: time.Hour, Hash: testHash})
	if err != nil {
		t.Fatal(err)
	}
	return s, auth.NewVerifier(pub)
}

func TestRefreshRotationAndReuseDetection(t *testing.T) {
	s, _ := newService(t)
	ctx := context.Background()
	first := mustRegister(t, s, Registration{Username: "carol"})

	second, err := s.Refresh(ctx, first.RefreshToken)
	if err != nil {
		t.Fatalf("first refresh: %v", err)
	}
	if second.RefreshToken == first.RefreshToken {
		t.Fatal("refresh token was not rotated")
	}

	third, err := s.Refresh(ctx, second.RefreshToken)
	if err != nil {
		t.Fatalf("second refresh: %v", err)
	}

	// Replaying a token whose replacement was used is treated as theft: it
	// fails and kills the family.
	if _, err := s.Refresh(ctx, first.RefreshToken); !errors.Is(err, ErrInvalidRefresh) {
		t.Fatalf("reuse err = %v, want ErrInvalidRefresh", err)
	}
	if _, err := s.Refresh(ctx, third.RefreshToken); !errors.Is(err, ErrInvalidRefresh) {
		t.Fatalf("token after reuse err = %v, want ErrInvalidRefresh (family revoked)", err)
	}

	// A separate login is a separate family and is unaffected.
	other, err := s.Login(ctx, "carol", "plum-orbit-candle-42", "203.0.113.1")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Logout(ctx, other.RefreshToken); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Refresh(ctx, other.RefreshToken); !errors.Is(err, ErrInvalidRefresh) {
		t.Fatalf("refresh after logout err = %v", err)
	}
	if _, err := s.Refresh(ctx, "garbage"); !errors.Is(err, ErrInvalidRefresh) {
		t.Fatalf("garbage token err = %v", err)
	}
}

// A client that never got a refresh response retries with the old token.
func TestRefreshRetryAfterLostResponse(t *testing.T) {
	s, _ := newService(t)
	ctx := context.Background()
	now := time.Now()
	s.now = func() time.Time { return now }
	first := mustRegister(t, s, Registration{Username: "dora"})
	lost, err := s.Refresh(ctx, first.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}

	// Retried twice (both responses lost), then the client keeps the last one.
	if _, err := s.Refresh(ctx, first.RefreshToken); err != nil {
		t.Fatalf("retry: %v", err)
	}
	got, err := s.Refresh(ctx, first.RefreshToken)
	if err != nil {
		t.Fatalf("second retry: %v", err)
	}
	if _, err := s.Refresh(ctx, lost.RefreshToken); !errors.Is(err, ErrInvalidRefresh) {
		t.Fatalf("lost replacement err = %v, want ErrInvalidRefresh", err)
	}
	// Revoking the undelivered replacements didn't end the session.
	next, err := s.Refresh(ctx, got.RefreshToken)
	if err != nil {
		t.Fatalf("refresh after retry: %v", err)
	}

	// Past the window, a replay revokes the family as usual.
	second, err := s.Refresh(ctx, next.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(RefreshRetryWindow + time.Minute)
	if _, err := s.Refresh(ctx, next.RefreshToken); !errors.Is(err, ErrInvalidRefresh) {
		t.Fatalf("late replay err = %v, want ErrInvalidRefresh", err)
	}
	if _, err := s.Refresh(ctx, second.RefreshToken); !errors.Is(err, ErrInvalidRefresh) {
		t.Fatalf("after late replay err = %v, want ErrInvalidRefresh (family revoked)", err)
	}
}
