package admin

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"parley/pkg/auth"
	"parley/pkg/db/dbtest"
	"parley/services/api/internal/delivery"
	"parley/services/api/internal/identity"
	"parley/services/api/internal/settings"
	"parley/services/api/internal/totp"
	"parley/services/api/migrations"
)

var fastHash = identity.HashParams{Memory: 1024, Time: 1, Threads: 1}

type harness struct {
	t        *testing.T
	srv      *httptest.Server
	accounts *Accounts
	ident    *identity.Service
	email    *delivery.Recorder
	creds    Credentials
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	pool := dbtest.New(t, migrations.FS)
	key := make([]byte, 32)
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	email := &delivery.Recorder{}
	ident, err := identity.NewService(pool, identity.Config{
		Signer: auth.NewSigner(priv, time.Minute), Verifier: auth.NewVerifier(pub), MFAKey: key, RefreshTTL: time.Hour, Hash: fastHash,
		Lockout: identity.DefaultLockout,
		Recovery: identity.RecoveryConfig{EmailEnabled: true, Email: email, SynchronousDelivery: true,
			AccountURL: "https://account.example.org"},
	})
	if err != nil {
		t.Fatal(err)
	}
	store, err := settings.NewStore(pool, key)
	if err != nil {
		t.Fatal(err)
	}
	accounts, err := NewAccounts(pool, key, fastHash)
	if err != nil {
		t.Fatal(err)
	}
	creds, err := accounts.Create(context.Background(), "root")
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{Accounts: accounts, Identity: ident, Settings: store, Pool: pool, Base: ident.Runtime(), InsecureCookie: true}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return &harness{t: t, srv: srv, accounts: accounts, ident: ident, email: email, creds: creds}
}

// client is a browser: it keeps the session cookie.
type client struct {
	h    *harness
	http *http.Client
}

func (h *harness) client() *client {
	jar, _ := cookiejar.New(nil)
	return &client{h: h, http: &http.Client{Jar: jar}}
}

// do sends a request the way the admin page does, and decodes the answer.
func (c *client) do(method, path string, body any, want int, out any) {
	c.h.t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, c.h.srv.URL+path, r)
	req.Header.Set("X-Parley-Admin", "1")
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		c.h.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != want {
		c.h.t.Fatalf("%s %s: status %d, want %d: %s", method, path, resp.StatusCode, want, raw)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			c.h.t.Fatalf("%s %s: %v: %s", method, path, err, raw)
		}
	}
}

func (h *harness) code(at time.Time) string {
	c, err := totp.Generate(h.creds.Secret, at)
	if err != nil {
		h.t.Fatal(err)
	}
	return c
}

func (h *harness) signedIn() *client {
	h.t.Helper()
	c := h.client()
	c.do("POST", "/api/login", map[string]string{"username": "root", "password": h.creds.Password, "code": h.code(time.Now())}, 200, nil)
	return c
}

func TestLogin(t *testing.T) {
	h := newHarness(t)
	c := h.client()
	c.do("GET", "/api/me", nil, 401, nil)
	c.do("GET", "/api/users", nil, 401, nil)

	// The password alone, or a wrong code, isn't enough.
	c.do("POST", "/api/login", map[string]string{"username": "root", "password": h.creds.Password, "code": ""}, 401, nil)
	c.do("POST", "/api/login", map[string]string{"username": "root", "password": "nope", "code": h.code(time.Now())}, 401, nil)

	now := time.Now()
	c.do("POST", "/api/login", map[string]string{"username": "root", "password": h.creds.Password, "code": h.code(now)}, 200, nil)
	var me Admin
	c.do("GET", "/api/me", nil, 200, &me)
	if me.Username != "root" {
		t.Fatalf("me = %+v", me)
	}
	// A code works once.
	h.client().do("POST", "/api/login", map[string]string{"username": "root", "password": h.creds.Password, "code": h.code(now)}, 401, nil)

	c.do("POST", "/api/logout", nil, 204, nil)
	c.do("GET", "/api/me", nil, 401, nil)
}

func TestLoginLockout(t *testing.T) {
	h := newHarness(t)
	c := h.client()
	for range maxLoginFailures {
		c.do("POST", "/api/login", map[string]string{"username": "root", "password": "wrong", "code": "000000"}, 401, nil)
	}
	// Locked even with the right credentials.
	c.do("POST", "/api/login", map[string]string{"username": "root", "password": h.creds.Password, "code": h.code(time.Now())}, 429, nil)
	// Resetting from the CLI clears it.
	creds, err := h.accounts.ResetCredentials(context.Background(), "root")
	if err != nil {
		t.Fatal(err)
	}
	h.creds = creds
	h.signedIn()
}

func TestSessionExpiresWhenIdle(t *testing.T) {
	h := newHarness(t)
	c := h.signedIn()
	h.accounts.now = func() time.Time { return time.Now().Add(SessionIdle + time.Minute) }
	c.do("GET", "/api/me", nil, 401, nil)
}

func TestDisabledAdminIsSignedOut(t *testing.T) {
	h := newHarness(t)
	c := h.signedIn()
	if err := h.accounts.SetDisabled(context.Background(), "root", true); err != nil {
		t.Fatal(err)
	}
	c.do("GET", "/api/me", nil, 401, nil)
}

func TestRequestGuards(t *testing.T) {
	h := newHarness(t)
	c := h.signedIn()
	post := func(mutate func(*http.Request)) int {
		req, _ := http.NewRequest("POST", h.srv.URL+"/api/logout", nil)
		req.Header.Set("X-Parley-Admin", "1")
		mutate(req)
		resp, err := c.http.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if got := post(func(r *http.Request) { r.Header.Del("X-Parley-Admin") }); got != 403 {
		t.Errorf("missing CSRF header: %d", got)
	}
	if got := post(func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") }); got != 403 {
		t.Errorf("foreign origin: %d", got)
	}
	if got := post(func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") }); got != 403 {
		t.Errorf("cross-site fetch: %d", got)
	}
	// DNS rebinding: an attacker's hostname resolving to 127.0.0.1.
	if got := post(func(r *http.Request) { r.Host = "evil.example:8090" }); got != http.StatusMisdirectedRequest {
		t.Errorf("foreign Host: %d", got)
	}
	// The page itself carries a strict CSP.
	resp, err := http.Get(h.srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'self'") || !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Errorf("CSP = %q", csp)
	}
	c.do("GET", "/api/me", nil, 200, nil) // still signed in: the refused logouts did nothing
}

func registerUser(t *testing.T, h *harness, username string) UserRow {
	t.Helper()
	hash, _ := identity.HashPassword("plum-orbit-candle-42", fastHash)
	var u UserRow
	if err := h.accounts.pool.QueryRow(context.Background(), `INSERT INTO users (username, display_name, password_hash, activated_at)
		VALUES ($1, $1, $2, now()) RETURNING id`, username, hash).Scan(&u.ID); err != nil {
		t.Fatal(err)
	}
	return u
}

func TestUserManagement(t *testing.T) {
	h := newHarness(t)
	c := h.signedIn()
	u := registerUser(t, h, "kim")
	registerUser(t, h, "kimberly_x")
	registerUser(t, h, "lou")
	base := "/api/users/" + u.ID.String()

	var list struct {
		Users []UserRow `json:"users"`
		Total int       `json:"total"`
	}
	c.do("GET", "/api/users?q=kim", nil, 200, &list)
	if list.Total != 2 {
		t.Fatalf("search kim: %+v", list)
	}
	// LIKE wildcards are literal.
	c.do("GET", "/api/users?q="+url.QueryEscape("_x"), nil, 200, &list)
	if list.Total != 1 || list.Users[0].Username != "kimberly_x" {
		t.Fatalf("search _x: %+v", list)
	}

	var row UserRow
	c.do("PUT", base+"/contact/email", map[string]any{"value": "kim@example.org", "verified": true}, 200, &row)
	if row.Email != "kim@example.org" || !row.EmailVerified {
		t.Fatalf("set email: %+v", row)
	}
	c.do("PUT", base+"/contact/phone", map[string]any{"value": "12345", "verified": true}, 400, nil)
	c.do("PATCH", base, map[string]string{"display_name": "Kim K"}, 200, &row)
	if row.DisplayName != "Kim K" {
		t.Fatalf("display name: %+v", row)
	}

	c.do("PUT", base+"/mfa-policy", map[string]string{"policy": "exempt"}, 200, &row)
	if row.MFAPolicy != "exempt" || row.MFARequired {
		t.Fatalf("policy: %+v", row)
	}
	c.do("PUT", "/api/settings/mfa", map[string]bool{"require": false}, 200, nil)
	c.do("PUT", base+"/mfa-policy", map[string]string{"policy": "inherit"}, 200, &row)
	if row.MFARequired {
		t.Fatal("inherit with the default off should not require MFA")
	}

	var sent struct{ Channel string }
	c.do("POST", base+"/password/send-link", nil, 200, &sent)
	if sent.Channel != "email" {
		t.Fatalf("send link: %+v", sent)
	}
	if _, ok := h.email.Last("kim@example.org"); !ok {
		t.Fatal("no reset email")
	}
	var link struct{ URL string }
	c.do("POST", base+"/password/create-link", nil, 200, &link)
	if !strings.Contains(link.URL, "#t=") {
		t.Fatalf("link: %+v", link)
	}

	c.do("POST", base+"/disable", nil, 200, &row)
	if row.DisabledAt == nil {
		t.Fatal("not disabled")
	}
	c.do("POST", base+"/enable", nil, 200, &row)
	c.do("POST", base+"/sessions/revoke", nil, 200, nil)
	c.do("GET", "/api/users/00000000-0000-0000-0000-000000000000", nil, 404, nil)

	// Everything is in the audit log, and the hand-over link is not.
	var audit struct{ Entries []Entry }
	c.do("GET", "/api/audit", nil, 200, &audit)
	actions := map[string]bool{}
	for _, e := range audit.Entries {
		actions[e.Action] = true
		if strings.Contains(string(e.Detail), "#t=") {
			t.Fatalf("link token in the audit log: %s", e.Detail)
		}
	}
	for _, a := range []string{"login", "user.contact.set", "user.display_name", "user.mfa_policy", "settings.mfa",
		"user.reset_link.send", "user.reset_link.create", "user.disable", "user.enable", "user.sessions_revoke"} {
		if !actions[a] {
			t.Errorf("no %q in the audit log", a)
		}
	}
}

func TestSettingsNeverReturnSecrets(t *testing.T) {
	h := newHarness(t)
	c := h.signedIn()
	var out map[string]any
	c.do("PUT", "/api/settings/email", map[string]any{"mode": "smtp", "host": "smtp.example.org", "port": 587, "tls": "starttls",
		"username": "mailer", "password": "s3cret-pass", "from": "no-reply@example.org"}, 200, &out)
	raw, _ := json.Marshal(out)
	if strings.Contains(string(raw), "s3cret") || strings.Contains(string(raw), "password_sealed") {
		t.Fatalf("secret in settings response: %s", raw)
	}
	email := out["email"].(map[string]any)
	if email["source"] != "override" || email["value"].(map[string]any)["has_password"] != true {
		t.Fatalf("email view: %+v", email)
	}
	// Applied to this process right away.
	if smtp, ok := h.ident.Runtime().Recovery.Email.(*delivery.SMTP); !ok || smtp.Host != "smtp.example.org" {
		t.Fatalf("not applied: %+v", h.ident.Runtime().Recovery.Email)
	}
	c.do("PUT", "/api/settings/sessions", map[string]string{"access_ttl": "5s", "refresh_ttl": "720h"}, 400, nil)
	c.do("PUT", "/api/settings/nope", map[string]string{}, 404, nil)
	c.do("DELETE", "/api/settings/email", nil, 200, &out)
	if out["email"].(map[string]any)["source"] != "deployment" {
		t.Fatalf("after revert: %+v", out["email"])
	}

	var audit struct{ Entries []Entry }
	c.do("GET", "/api/audit", nil, 200, &audit)
	for _, e := range audit.Entries {
		if strings.Contains(string(e.Detail), "s3cret") {
			t.Fatalf("secret in the audit log: %s", e.Detail)
		}
	}
}

func TestChangeOwnPassword(t *testing.T) {
	h := newHarness(t)
	c := h.signedIn()
	// Each sign-in or change uses up a code, so later ones need later steps.
	step := totp.Period * time.Second
	other := h.client()
	other.do("POST", "/api/login", map[string]string{"username": "root", "password": h.creds.Password, "code": h.code(time.Now().Add(step))}, 200, nil)
	now := time.Now().Add(2 * step)
	h.accounts.now = func() time.Time { return now }
	c.do("POST", "/api/me/password", map[string]string{"current_password": "wrong", "code": h.code(now), "new_password": "glacier-mosaic-lantern-9"}, 400, nil)
	c.do("POST", "/api/me/password", map[string]string{"current_password": h.creds.Password, "code": h.code(now), "new_password": "glacier-mosaic-lantern-9"}, 204, nil)
	c.do("GET", "/api/me", nil, 200, nil)
	other.do("GET", "/api/me", nil, 401, nil) // other sessions end
}
