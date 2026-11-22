// Package totp implements RFC 6238 time-based one-time passwords as
// authenticator apps (Google Authenticator, 1Password, Authy, ...) expect them:
// SHA-1, 6 digits, 30-second steps.
package totp

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/url"
	"strings"
	"time"
)

const (
	Period       = 30
	Window       = 1 // steps of clock drift accepted either side
	SecretLength = 20
)

// Encoding is how secrets are shown to people and apps.
var Encoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewSecret returns a random secret.
func NewSecret() ([]byte, error) {
	s := make([]byte, SecretLength)
	_, err := rand.Read(s)
	return s, err
}

// Code returns the code for a step.
func Code(secret []byte, step int64) string {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(step))
	mac := hmac.New(sha1.New, secret)
	mac.Write(msg[:])
	sum := mac.Sum(nil)
	offset := sum[len(sum)-1] & 0x0f
	n := binary.BigEndian.Uint32(sum[offset:offset+4]) & 0x7fffffff
	return fmt.Sprintf("%06d", n%1_000_000)
}

// Match returns the step matching code, if it is newer than after.
func Match(secret []byte, code string, now time.Time, after int64) (int64, bool) {
	current := now.Unix() / Period
	code = strings.TrimSpace(code)
	for step := current - Window; step <= current+Window; step++ {
		if step > after && subtle.ConstantTimeCompare([]byte(Code(secret, step)), []byte(code)) == 1 {
			return step, true
		}
	}
	return 0, false
}

// URI is the otpauth:// link an authenticator app imports (usually as a QR code).
func URI(issuer, account string, secret []byte) string {
	q := url.Values{"secret": {Encoding.EncodeToString(secret)}, "issuer": {issuer}, "algorithm": {"SHA1"}, "digits": {"6"}, "period": {"30"}}
	return "otpauth://totp/" + url.PathEscape(issuer+":"+account) + "?" + q.Encode()
}

// Generate returns the code for a base32 secret at t, as an authenticator app
// would. Used by tests.
func Generate(secret string, t time.Time) (string, error) {
	raw, err := Encoding.DecodeString(secret)
	if err != nil {
		return "", err
	}
	return Code(raw, t.Unix()/Period), nil
}
