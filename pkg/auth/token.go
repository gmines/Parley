// Package auth issues and verifies the short-lived access tokens shared by all
// Parley services. Tokens are EdDSA (Ed25519) JWTs: only the api service holds
// the private key, every other service verifies with the public key.
package auth

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

const (
	issuer = "parley"
	// Restricted tokens use their own issuers instead of a claim: a verifier
	// that knows nothing about them (every service's Verify, including older
	// builds) rejects them outright, so a new restriction can't fail open.
	enrollmentIssuer = "parley-mfa-enroll"
	challengeIssuer  = "parley-mfa-challenge"
)

var ErrInvalidToken = errors.New("invalid token")

// Claims carried in an access token.
type Claims struct {
	UserID   uuid.UUID
	Username string
}

type jwtClaims struct {
	Username string `json:"usr"`
	jwt.RegisteredClaims
}

// Signer mints access tokens.
type Signer struct {
	key ed25519.PrivateKey
	ttl atomic.Int64 // time.Duration; adjustable while running
	now func() time.Time
}

func NewSigner(key ed25519.PrivateKey, ttl time.Duration) *Signer {
	s := &Signer{key: key, now: time.Now}
	s.ttl.Store(int64(ttl))
	return s
}

// TTL is the lifetime of access tokens issued from now on.
func (s *Signer) TTL() time.Duration { return time.Duration(s.ttl.Load()) }

// SetTTL changes the lifetime of tokens issued from now on.
func (s *Signer) SetTTL(d time.Duration) { s.ttl.Store(int64(d)) }

// Sign returns a signed access token and its expiry.
func (s *Signer) Sign(c Claims) (string, time.Time, error) {
	return s.sign(issuer, c, s.TTL())
}

// SignEnrollment returns a token that can only be used to set up MFA. Accounts
// without MFA (where it's required) get these instead of access tokens.
func (s *Signer) SignEnrollment(c Claims) (string, time.Time, error) {
	return s.sign(enrollmentIssuer, c, s.TTL())
}

// SignChallenge returns a token proving the password step of a login, to be
// presented with the MFA code.
func (s *Signer) SignChallenge(c Claims, ttl time.Duration) (string, time.Time, error) {
	return s.sign(challengeIssuer, c, ttl)
}

func (s *Signer) sign(iss string, c Claims, ttl time.Duration) (string, time.Time, error) {
	now := s.now()
	exp := now.Add(ttl)
	tok := jwt.NewWithClaims(jwt.SigningMethodEdDSA, jwtClaims{
		Username: c.Username,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    iss,
			Subject:   c.UserID.String(),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(exp),
		},
	})
	signed, err := tok.SignedString(s.key)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("sign token: %w", err)
	}
	return signed, exp, nil
}

// Verifier validates access tokens.
type Verifier struct {
	key ed25519.PublicKey
}

func NewVerifier(key ed25519.PublicKey) *Verifier {
	return &Verifier{key: key}
}

// Verify accepts full access tokens only.
func (v *Verifier) Verify(token string) (Claims, error) { return v.verify(issuer, token) }

// VerifyEnrollment accepts only MFA enrollment tokens.
func (v *Verifier) VerifyEnrollment(token string) (Claims, error) {
	return v.verify(enrollmentIssuer, token)
}

// VerifyChallenge accepts only MFA login challenge tokens.
func (v *Verifier) VerifyChallenge(token string) (Claims, error) {
	return v.verify(challengeIssuer, token)
}

func (v *Verifier) verify(iss, token string) (Claims, error) {
	parser := jwt.NewParser(
		jwt.WithValidMethods([]string{jwt.SigningMethodEdDSA.Alg()}),
		jwt.WithIssuer(iss),
		jwt.WithExpirationRequired(),
		jwt.WithLeeway(5*time.Second),
	)
	var jc jwtClaims
	_, err := parser.ParseWithClaims(token, &jc, func(*jwt.Token) (any, error) { return v.key, nil })
	if err != nil {
		return Claims{}, ErrInvalidToken
	}
	id, err := uuid.Parse(jc.Subject)
	if err != nil {
		return Claims{}, ErrInvalidToken
	}
	return Claims{UserID: id, Username: jc.Username}, nil
}

// ParsePrivateKey decodes a base64 (std encoding) 32-byte Ed25519 seed.
func ParsePrivateKey(b64Seed string) (ed25519.PrivateKey, error) {
	seed, err := base64.StdEncoding.DecodeString(b64Seed)
	if err != nil {
		return nil, fmt.Errorf("decode signing key: %w", err)
	}
	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("signing key must be a %d-byte seed, got %d bytes", ed25519.SeedSize, len(seed))
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

// EncodePublicKey returns the base64 (std encoding) form of a verification key.
func EncodePublicKey(k ed25519.PublicKey) string { return base64.StdEncoding.EncodeToString(k) }

// ParsePublicKey decodes a base64 (std encoding) Ed25519 public key.
func ParsePublicKey(b64 string) (ed25519.PublicKey, error) {
	k, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, fmt.Errorf("decode verify key: %w", err)
	}
	if len(k) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("verify key must be %d bytes, got %d", ed25519.PublicKeySize, len(k))
	}
	return ed25519.PublicKey(k), nil
}
