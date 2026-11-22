package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

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
	base := newTestServerWith(t, func(c *identity.Config) { c.Recovery = identity.RecoveryConfig{} })
	anon := &client{t: t, base: base}
	var f identity.Features
	anon.do("GET", "/v1/auth/features", nil, 200, &f)
	if f.EmailRecovery || f.SMSRecovery || f.Signup {
		t.Fatalf("features = %+v, want all off", f)
	}
	anon.do("POST", "/v1/auth/password-reset/request", map[string]string{"identifier": "x"}, 403, nil)
	anon.do("POST", "/v1/auth/signup", map[string]string{"username": "jo", "email": "jo@example.org"}, 403, nil)
}

func TestContactRoutes(t *testing.T) {
	base := newTestServer(t)
	c, _ := register(t, base, "kim")
	c.do("PUT", "/v1/users/me/fax", map[string]string{"value": "1", "password": testPassword}, 404, nil)
}

// A restricted token reaches enrollment and nothing else; a full token can't
// use enrollment; and sessions that predate MFA can't skip it.
func TestMFAEnforcementOverHTTP(t *testing.T) {
	base := newTestServer(t)
	signup(t, base, "lee")
	anon := &client{t: t, base: base}

	var device1, device2 identity.Session
	login := map[string]string{"username": "lee", "password": testPassword}
	anon.do("POST", "/v1/auth/login", login, 200, &device1)
	anon.do("POST", "/v1/auth/login", login, 200, &device2)
	if device1.User.MFAEnabled {
		t.Fatal("new account reports MFA enabled")
	}

	restricted := &client{t: t, base: base, token: device1.AccessToken}
	for _, path := range []string{"/v1/users/me", "/v1/friends", "/v1/conversations", "/v1/blocks"} {
		restricted.do("GET", path, nil, 401, nil)
	}
	restricted.do("POST", "/v1/users/me/password", map[string]string{"current_password": testPassword, "new_password": "velvet-thunder-pine"}, 401, nil)
	var refreshed identity.Session
	anon.do("POST", "/v1/auth/refresh", map[string]string{"refresh_token": device1.RefreshToken}, 200, &refreshed)
	(&client{t: t, base: base, token: refreshed.AccessToken}).do("GET", "/v1/users/me", nil, 401, nil)
	anon.do("POST", "/v1/auth/mfa/enroll/start", nil, 401, nil)

	secret, out := enroll(t, base, refreshed.AccessToken)
	if len(out.RecoveryCodes) != 10 || !out.User.MFAEnabled {
		t.Fatalf("enrolled = %+v", out)
	}
	full := &client{t: t, base: base, token: out.AccessToken}
	full.do("GET", "/v1/users/me", nil, 200, nil)
	// Full tokens reach enrollment (for accounts opting in), but it's done.
	full.do("POST", "/v1/auth/mfa/enroll/start", nil, 409, nil)
	// The other device's password-only session is gone.
	anon.do("POST", "/v1/auth/refresh", map[string]string{"refresh_token": device2.RefreshToken}, 401, nil)

	var challenge struct {
		MFAToken string `json:"mfa_token"`
	}
	anon.do("POST", "/v1/auth/login", login, 200, &challenge)
	(&client{t: t, base: base, token: challenge.MFAToken}).do("GET", "/v1/users/me", nil, 401, nil)
	anon.do("POST", "/v1/auth/mfa/login", map[string]string{"mfa_token": challenge.MFAToken, "code": "000000"}, 400, nil)
	anon.do("POST", "/v1/auth/mfa/login", map[string]string{"mfa_token": challenge.MFAToken, "code": out.RecoveryCodes[0]}, 200, nil)
	anon.do("POST", "/v1/auth/mfa/login", map[string]string{"mfa_token": challenge.MFAToken, "code": out.RecoveryCodes[0]}, 400, nil)
	_ = secret
}

func TestPasswordResetLinkOverHTTP(t *testing.T) {
	base := newTestServer(t)
	register(t, base, "pat")
	v, _ := testServers.Load(base)
	ts := v.(testServer)
	anon := &client{t: t, base: base}
	internal := &client{t: t, base: ts.internal}

	before := len(ts.email.Messages())
	anon.do("POST", "/v1/auth/password-reset/request", map[string]string{"identifier": "ghost"}, 202, nil)
	if len(ts.email.Messages()) != before {
		t.Fatal("message sent for unknown account")
	}
	anon.do("POST", "/v1/auth/password-reset/request", map[string]string{"identifier": "pat@example.org"}, 202, nil)
	token := lastLink(t, ts.email, "pat@example.org")

	var info identity.LinkInfo
	internal.do("POST", "/internal/v1/links/inspect", map[string]string{"token": token}, 200, &info)
	if info.Purpose != "reset_password" || info.Username != "pat" {
		t.Fatalf("info = %+v", info)
	}
	internal.do("POST", "/internal/v1/links/complete", map[string]string{"token": token, "new_password": "12345678"}, 400, nil)
	internal.do("POST", "/internal/v1/links/complete", map[string]string{"token": token, "new_password": "velvet-thunder-pine"}, 204, nil)
	internal.do("POST", "/internal/v1/links/complete", map[string]string{"token": token, "new_password": "copper-lantern-meadow"}, 400, nil)

	// A reset must not bypass the second factor.
	var challenge map[string]any
	anon.do("POST", "/v1/auth/login", map[string]string{"username": "pat", "password": "velvet-thunder-pine"}, 200, &challenge)
	if challenge["mfa_required"] != true || challenge["access_token"] != nil {
		t.Fatalf("login after reset = %v", challenge)
	}
	// The public API doesn't serve the internal routes.
	anon.do("POST", "/internal/v1/links/complete", map[string]string{"token": token, "new_password": "velvet-thunder-pine"}, 404, nil)
}
