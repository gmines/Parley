package auth

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestSignVerifyRoundTrip(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	s := NewSigner(priv, time.Minute)
	want := Claims{UserID: uuid.New(), Username: "alice"}

	tok, exp, err := s.Sign(want)
	if err != nil {
		t.Fatal(err)
	}
	if time.Until(exp) <= 0 {
		t.Fatalf("expiry %v is not in the future", exp)
	}
	got, err := NewVerifier(pub).Verify(tok)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("claims = %+v, want %+v", got, want)
	}
}

func TestVerifyRejects(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	otherPub, _, _ := ed25519.GenerateKey(rand.Reader)
	claims := Claims{UserID: uuid.New(), Username: "bob"}

	expired := NewSigner(priv, time.Minute)
	expired.now = func() time.Time { return time.Now().Add(-time.Hour) }
	expiredTok, _, _ := expired.Sign(claims)
	validTok, _, _ := NewSigner(priv, time.Minute).Sign(claims)

	cases := map[string]struct {
		key   ed25519.PublicKey
		token string
	}{
		"expired":   {pub, expiredTok},
		"wrong key": {otherPub, validTok},
		"garbage":   {pub, "not.a.jwt"},
		"tampered":  {pub, validTok[:len(validTok)-2] + "xx"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := NewVerifier(tc.key).Verify(tc.token); err != ErrInvalidToken {
				t.Fatalf("err = %v, want ErrInvalidToken", err)
			}
		})
	}
}
