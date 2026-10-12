package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"regexp"
	"testing"

	"parley/services/api/internal/delivery"
	"parley/services/api/internal/identity"
)

func TestLoginLockoutOverHTTP(t *testing.T) {
	base := newTestServerWith(t, func(c *identity.Config) { c.Lockout = identity.DefaultLockout })
	register(t, base, "ivy")

	// A client that forges X-Forwarded-For on every attempt must still be
	// counted by its real address (no trusted proxies are configured).
	login := func(password, xff string) *http.Response {
		body, _ := json.Marshal(map[string]string{"username": "ivy", "password": password})
		req, _ := http.NewRequest("POST", base+"/v1/auth/login", bytes.NewReader(body))
		req.Header.Set("X-Forwarded-For", xff)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	for i, xff := range []string{"1.1.1.1", "2.2.2.2", "3.3.3.3", "4.4.4.4"} {
		if resp := login("wrong-password-here", xff); resp.StatusCode != 401 {
			t.Fatalf("failure %d: status %d, want 401", i+1, resp.StatusCode)
		}
	}
	resp := login("wrong-password-here", "5.5.5.5")
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") != "300" {
		t.Fatalf("5th failure: status %d Retry-After %q, want 429 and 300", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
	if resp := login("plum-orbit-candle-42", "6.6.6.6"); resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("correct password while locked: status %d, want 429", resp.StatusCode)
	}
}

func TestRecoveryEndpointsDisabled(t *testing.T) {
	base := newTestServer(t)
	anon := &client{t: t, base: base}

	var f identity.Features
	anon.do("GET", "/v1/auth/features", nil, 200, &f)
	if f.EmailRecovery || f.SMSRecovery {
		t.Fatalf("features = %+v, want all off", f)
	}
	anon.do("POST", "/v1/auth/password-reset/request", map[string]string{"username": "x", "channel": "email"}, 403, nil)
	anon.do("POST", "/v1/auth/register", map[string]string{"username": "jo", "password": "plum-orbit-candle-42", "email": "jo@example.org"}, 403, nil)

	c, _ := register(t, base, "kim")
	c.do("PUT", "/v1/users/me/email", map[string]string{"value": "kim@example.org", "password": "plum-orbit-candle-42"}, 403, nil)
	c.do("PUT", "/v1/users/me/fax", map[string]string{"value": "1", "password": "plum-orbit-candle-42"}, 404, nil)
}

func TestPasswordResetOverHTTP(t *testing.T) {
	email := &delivery.Recorder{}
	base := newTestServerWith(t, func(c *identity.Config) {
		c.Recovery = identity.RecoveryConfig{EmailEnabled: true, Email: email, SynchronousDelivery: true}
	})
	code := func() string {
		m, ok := email.Last("lee@example.org")
		if !ok {
			t.Fatal("no email sent")
		}
		return regexp.MustCompile(`\b\d{6}\b`).FindString(m.Body)
	}

	anon := &client{t: t, base: base}
	var f identity.Features
	anon.do("GET", "/v1/auth/features", nil, 200, &f)
	if !f.EmailRecovery || f.SMSRecovery {
		t.Fatalf("features = %+v", f)
	}

	var sess identity.Session
	anon.do("POST", "/v1/auth/register", map[string]string{
		"username": "lee", "password": "plum-orbit-candle-42", "email": "lee@example.org",
	}, 201, &sess)
	c := &client{t: t, base: base, token: sess.AccessToken}
	var u identity.User
	c.do("POST", "/v1/users/me/email/verify", map[string]string{"code": code()}, 200, &u)
	if !u.EmailVerified {
		t.Fatalf("user = %+v", u)
	}

	anon.do("POST", "/v1/auth/password-reset/request", map[string]string{"username": "lee", "channel": "email"}, 202, nil)
	anon.do("POST", "/v1/auth/password-reset/request", map[string]string{"username": "ghost", "channel": "email"}, 202, nil)
	anon.do("POST", "/v1/auth/password-reset/confirm", map[string]string{"username": "lee", "code": "000000", "new_password": "velvet-thunder-pine"}, 400, nil)
	anon.do("POST", "/v1/auth/password-reset/confirm", map[string]string{"username": "lee", "code": code(), "new_password": "velvet-thunder-pine"}, 204, nil)
	anon.do("POST", "/v1/auth/login", map[string]string{"username": "lee", "password": "velvet-thunder-pine"}, 200, nil)

	// Change password while logged in returns a new session.
	var next identity.Session
	anon.do("POST", "/v1/auth/login", map[string]string{"username": "lee", "password": "velvet-thunder-pine"}, 200, &next)
	c.token = next.AccessToken
	c.do("POST", "/v1/users/me/password", map[string]string{"current_password": "velvet-thunder-pine", "new_password": "copper-lantern-meadow"}, 200, &next)
	anon.do("POST", "/v1/auth/login", map[string]string{"username": "lee", "password": "copper-lantern-meadow"}, 200, nil)
}
