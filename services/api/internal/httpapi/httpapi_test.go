package httpapi

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sync"
	"testing"
	"time"

	"parley/pkg/auth"
	"parley/pkg/db/dbtest"
	"parley/pkg/events"
	"parley/services/api/internal/delivery"
	"parley/services/api/internal/identity"
	"parley/services/api/internal/social"
	"parley/services/api/migrations"
)

type client struct {
	t     *testing.T
	base  string
	token string
}

func (c *client) do(method, path string, body any, wantStatus int, out any) {
	c.t.Helper()
	var buf bytes.Buffer
	if body != nil {
		json.NewEncoder(&buf).Encode(body)
	}
	req, _ := http.NewRequest(method, c.base+path, &buf)
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != wantStatus {
		var e map[string]any
		json.NewDecoder(resp.Body).Decode(&e)
		c.t.Fatalf("%s %s: status %d, want %d (body %v)", method, path, resp.StatusCode, wantStatus, e)
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			c.t.Fatalf("%s %s: decode: %v", method, path, err)
		}
	}
}

func newTestServer(t *testing.T) string {
	return newTestServerWith(t, nil)
}

// newTestServerWith lets a test adjust the identity config (lockout, recovery).
func newTestServerWith(t *testing.T, configure func(*identity.Config)) string {
	pool := dbtest.New(t, migrations.FS)
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	email := &delivery.Recorder{}
	cfg := identity.Config{
		Signer:   auth.NewSigner(priv, time.Minute),
		Verifier: auth.NewVerifier(pub),
		MFAKey:   make([]byte, 32),
		Recovery: identity.RecoveryConfig{
			EmailEnabled: true, Email: email, SynchronousDelivery: true, AccountURL: "https://account.example.org",
		},
		RefreshTTL: time.Hour,
		Hash:       identity.HashParams{Memory: 1024, Time: 1, Threads: 1},
	}
	if configure != nil {
		configure(&cfg)
	}
	ident, err := identity.NewService(pool, cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{Identity: ident, Social: social.NewService(pool, events.Discard{}), Verifier: auth.NewVerifier(pub), Pool: pool}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	internal := httptest.NewServer(srv.InternalHandler())
	t.Cleanup(internal.Close)
	testServers.Store(ts.URL, testServer{email: email, internal: internal.URL})
	return ts.URL
}

// testServer is what the register helper needs besides a server's public URL.
type testServer struct {
	email    *delivery.Recorder
	internal string
}

var testServers sync.Map

const testPassword = "plum-orbit-candle-42"

var linkRe = regexp.MustCompile(`#t=([A-Za-z0-9_-]{43})`)

func lastLink(t *testing.T, r *delivery.Recorder, to string) string {
	t.Helper()
	m, ok := r.Last(to)
	if !ok {
		t.Fatalf("no message to %s", to)
	}
	return linkRe.FindStringSubmatch(m.Body)[1]
}

// signup takes a new account from sign-up through its activation link.
func signup(t *testing.T, base, name string) {
	t.Helper()
	v, _ := testServers.Load(base)
	ts := v.(testServer)
	addr := name + "@example.org"
	(&client{t: t, base: base}).do("POST", "/v1/auth/signup", map[string]string{"username": name, "email": addr}, 202, nil)
	(&client{t: t, base: ts.internal}).do("POST", "/internal/v1/links/complete",
		map[string]string{"token": lastLink(t, ts.email, addr), "new_password": testPassword}, 204, nil)
}

// enroll sets up MFA using an enrollment token and returns the secret.
func enroll(t *testing.T, base, token string) (string, identity.Enrolled) {
	t.Helper()
	c := &client{t: t, base: base, token: token}
	var e identity.MFAEnrollment
	c.do("POST", "/v1/auth/mfa/enroll/start", nil, 200, &e)
	code, err := identity.GenerateTOTP(e.Secret, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var out identity.Enrolled
	c.do("POST", "/v1/auth/mfa/enroll/confirm", map[string]string{"code": code}, 200, &out)
	return e.Secret, out
}

// register signs up, activates and enrolls a user, returning a full session.
func register(t *testing.T, base, name string) (*client, identity.Session) {
	t.Helper()
	signup(t, base, name)
	var first identity.Session
	(&client{t: t, base: base}).do("POST", "/v1/auth/login", map[string]string{"username": name, "password": testPassword}, 200, &first)
	_, out := enroll(t, base, first.AccessToken)
	return &client{t: t, base: base, token: out.AccessToken}, out.Session
}

// The flow from the plan's verification section: register two users → request →
// accept → block → assert interaction is rejected.
func TestFriendFlow(t *testing.T) {
	base := newTestServer(t)
	alice, aliceSess := register(t, base, "alice")
	bob, bobSess := register(t, base, "bob")

	anon := &client{t: t, base: base}
	anon.do("GET", "/v1/friends", nil, 401, nil)
	(&client{t: t, base: base, token: "garbage"}).do("GET", "/v1/friends", nil, 401, nil)

	var me identity.User
	alice.do("GET", "/v1/users/me", nil, 200, &me)
	if me.ID != aliceSess.User.ID {
		t.Fatalf("me = %v", me)
	}

	var sent social.SendResult
	alice.do("POST", "/v1/friends/requests", map[string]string{"username": "bob"}, 200, &sent)

	var reqs struct{ Incoming, Outgoing []social.Request }
	bob.do("GET", "/v1/friends/requests", nil, 200, &reqs)
	if len(reqs.Incoming) != 1 || reqs.Incoming[0].ID != sent.Request.ID {
		t.Fatalf("bob incoming = %+v", reqs.Incoming)
	}
	bob.do("POST", "/v1/friends/requests/"+sent.Request.ID.String()+"/accept", nil, 200, nil)

	var friends struct{ Friends []social.Friend }
	alice.do("GET", "/v1/friends", nil, 200, &friends)
	if len(friends.Friends) != 1 || friends.Friends[0].User.ID != bobSess.User.ID {
		t.Fatalf("alice friends = %+v", friends.Friends)
	}

	bob.do("PUT", "/v1/blocks/"+aliceSess.User.ID.String(), nil, 204, nil)
	alice.do("GET", "/v1/friends", nil, 200, &friends)
	if len(friends.Friends) != 0 {
		t.Fatal("friendship survived block")
	}
	alice.do("POST", "/v1/friends/requests", map[string]string{"username": "bob"}, 400, nil)
	bob.do("POST", "/v1/friends/requests", map[string]string{"username": "alice"}, 409, nil)

	var blocked struct{ Blocked []social.Blocked }
	bob.do("GET", "/v1/blocks", nil, 200, &blocked)
	if len(blocked.Blocked) != 1 {
		t.Fatalf("blocked = %+v", blocked)
	}
	bob.do("DELETE", "/v1/blocks/"+aliceSess.User.ID.String(), nil, 204, nil)
	alice.do("POST", "/v1/friends/requests", map[string]string{"username": "bob"}, 200, nil)

	bob.do("PUT", "/v1/blocks/not-a-uuid", nil, 404, nil)
}

func TestAuthEndpoints(t *testing.T) {
	base := newTestServer(t)
	c, sess := register(t, base, "dana")
	anon := &client{t: t, base: base}

	anon.do("POST", "/v1/auth/signup", map[string]string{"username": "dana", "email": "d2@example.org"}, 409, nil)
	anon.do("POST", "/v1/auth/signup", map[string]string{"username": "x", "email": "x@example.org"}, 400, nil)
	anon.do("POST", "/v1/auth/signup", map[string]string{"username": "nocontact"}, 400, nil)
	anon.do("POST", "/v1/auth/signup", map[string]any{"username": "erin", "email": "e@example.org", "admin": true}, 400, nil)
	anon.do("POST", "/v1/auth/login", map[string]string{"username": "dana", "password": "nope-nope"}, 401, nil)
	var challenge map[string]any
	anon.do("POST", "/v1/auth/login", map[string]string{"username": "dana", "password": testPassword}, 200, &challenge)
	if challenge["mfa_required"] != true || challenge["access_token"] != nil || challenge["refresh_token"] != nil {
		t.Fatalf("login with MFA returned %v", challenge)
	}

	var next identity.Session
	anon.do("POST", "/v1/auth/refresh", map[string]string{"refresh_token": sess.RefreshToken}, 200, &next)
	// A retry (the response was lost) gets a new session while the first replacement is unused.
	anon.do("POST", "/v1/auth/refresh", map[string]string{"refresh_token": sess.RefreshToken}, 200, &next)
	anon.do("POST", "/v1/auth/logout", map[string]string{"refresh_token": next.RefreshToken}, 204, nil)
	anon.do("POST", "/v1/auth/refresh", map[string]string{"refresh_token": next.RefreshToken}, 401, nil)
	anon.do("POST", "/v1/auth/refresh", map[string]string{"refresh_token": sess.RefreshToken}, 401, nil)

	// Access tokens are stateless and stay valid until expiry.
	c.do("GET", "/v1/users/me", nil, 200, nil)
}
