package gateway

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/nats-io/nats-server/v2/server"
	natstest "github.com/nats-io/nats-server/v2/test"
	"github.com/nats-io/nats.go"

	"parley/pkg/auth"
	"parley/pkg/events"
)

type harness struct {
	t      *testing.T
	url    string
	signer *auth.Signer
	pub    events.NATS
	gw     *Gateway
}

func setup(t *testing.T) *harness {
	t.Helper()
	opts := natstest.DefaultTestOptions
	opts.Port = server.RANDOM_PORT
	ns := natstest.RunServer(&opts)
	t.Cleanup(ns.Shutdown)

	connect := func() *nats.Conn {
		nc, err := nats.Connect(ns.ClientURL())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(nc.Close)
		return nc
	}
	pubKey, priv, _ := ed25519.GenerateKey(rand.Reader)
	gw := New(connect(), auth.NewVerifier(pubKey))
	srv := httptest.NewServer(gw)
	t.Cleanup(srv.Close)
	return &harness{
		t:      t,
		url:    "ws" + strings.TrimPrefix(srv.URL, "http"),
		signer: auth.NewSigner(priv, time.Minute),
		pub:    events.NATS{Conn: connect()},
		gw:     gw,
	}
}

func (h *harness) dial(user uuid.UUID) *websocket.Conn {
	h.t.Helper()
	tok, _, _ := h.signer.Sign(auth.Claims{UserID: user, Username: "u"})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, h.url, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + tok}},
	})
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { ws.CloseNow() })
	if got := read(h.t, ws); got.Type != "ready" {
		h.t.Fatalf("first message type = %q, want ready", got.Type)
	}
	return ws
}

func read(t *testing.T, ws *websocket.Conn) events.Wire {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, data, err := ws.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var w events.Wire
	if err := json.Unmarshal(data, &w); err != nil {
		t.Fatal(err)
	}
	return w
}

// expectSilence fails if a message arrives within d.
func expectSilence(t *testing.T, ws *websocket.Conn, d time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	if _, data, err := ws.Read(ctx); err == nil {
		t.Fatalf("unexpected message: %s", data)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatal("condition not met in time")
}

func TestRejectsMissingOrBadToken(t *testing.T) {
	h := setup(t)
	for _, hdr := range []string{"", "Bearer nope"} {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, resp, err := websocket.Dial(ctx, h.url, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {hdr}}})
		cancel()
		if err == nil || resp == nil || resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("auth %q: err %v resp %v, want 401", hdr, err, resp)
		}
	}
}

func TestFanOutToUsersConnections(t *testing.T) {
	h := setup(t)
	alice, bob := uuid.New(), uuid.New()
	a1, a2, b := h.dial(alice), h.dial(alice), h.dial(bob)
	waitFor(t, func() bool { u, c := h.gw.Connections(); return u == 2 && c == 3 })

	h.pub.Publish(context.Background(), events.Event{
		Type: events.MessageCreated, Recipients: []uuid.UUID{alice}, Data: map[string]string{"content": "hi"},
	})
	for i, ws := range []*websocket.Conn{a1, a2} {
		got := read(t, ws)
		if got.Type != events.MessageCreated || !strings.Contains(string(got.Data), `"hi"`) {
			t.Fatalf("alice conn %d got %+v", i, got)
		}
	}
	expectSilence(t, b, 200*time.Millisecond)
}

func TestUnsubscribesWhenLastConnectionCloses(t *testing.T) {
	h := setup(t)
	alice := uuid.New()
	ws := h.dial(alice)
	waitFor(t, func() bool { u, _ := h.gw.Connections(); return u == 1 })
	ws.Close(websocket.StatusNormalClosure, "")
	waitFor(t, func() bool { u, c := h.gw.Connections(); return u == 0 && c == 0 })
}

func TestSlowClientIsDisconnected(t *testing.T) {
	h := setup(t)
	alice := uuid.New()
	ws := h.dial(alice) // never reads after "ready"
	waitFor(t, func() bool { u, _ := h.gw.Connections(); return u == 1 })

	big := strings.Repeat("x", 64<<10)
	for i := 0; i < sendBuffer*4; i++ {
		h.pub.Publish(context.Background(), events.Event{Type: "spam", Recipients: []uuid.UUID{alice}, Data: big})
	}
	waitFor(t, func() bool { _, c := h.gw.Connections(); return c == 0 })
	_ = ws
}
