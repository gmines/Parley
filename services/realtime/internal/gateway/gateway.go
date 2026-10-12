// Package gateway delivers events to connected clients over WebSocket.
//
// Each user's events arrive on their own NATS subject (events.UserSubject).
// The gateway subscribes to it while the user has at least one connection
// open and fans each event out to all of that user's connections (several
// devices, or several windows).
package gateway

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"

	"parley/pkg/auth"
	"parley/pkg/events"
)

const (
	sendBuffer   = 256 // events queued per connection before it counts as stuck
	writeTimeout = 10 * time.Second
)

type Gateway struct {
	nc       *nats.Conn
	verifier *auth.Verifier
	// PingInterval keeps idle connections (and the NAT mappings along their
	// path) alive and detects dead peers.
	PingInterval time.Duration

	mu    sync.Mutex
	users map[uuid.UUID]*userConns
}

type userConns struct {
	conns map[*client]struct{}
	sub   *nats.Subscription
}

type client struct {
	send chan []byte
	// kick drops a client that fell too far behind. It cancels the
	// connection's context, which also aborts a write blocked on a peer that
	// stopped reading.
	kick context.CancelFunc
}

func New(nc *nats.Conn, verifier *auth.Verifier) *Gateway {
	return &Gateway{nc: nc, verifier: verifier, PingInterval: 25 * time.Second, users: map[uuid.UUID]*userConns{}}
}

// ServeHTTP upgrades an authenticated request ("Authorization: Bearer
// <access token>") to a WebSocket and streams the user's events until the
// connection ends. The client sends nothing; the first message is
// {"type":"ready","data":{"user_id":...}}.
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	claims, err := g.verifier.Verify(strings.TrimSpace(raw))
	if !ok || err != nil {
		http.Error(w, `{"code":"unauthorized","message":"missing or invalid credentials"}`, http.StatusUnauthorized)
		return
	}
	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		return // Accept already wrote the HTTP error
	}
	defer ws.CloseNow()

	// CloseRead handles pings/close frames; its context ends when the peer goes away.
	ctx, kick := context.WithCancel(ws.CloseRead(r.Context()))
	defer kick()
	c := &client{send: make(chan []byte, sendBuffer), kick: kick}
	if err := g.add(claims.UserID, c); err != nil {
		slog.Error("subscribe", "user_id", claims.UserID, "err", err)
		ws.Close(websocket.StatusInternalError, "subscribe failed")
		return
	}
	defer g.remove(claims.UserID, c)

	ready, _ := json.Marshal(map[string]any{"type": "ready", "data": map[string]uuid.UUID{"user_id": claims.UserID}})
	c.send <- ready

	ping := time.NewTicker(g.PingInterval)
	defer ping.Stop()
	for {
		select {
		case msg := <-c.send:
			wctx, cancel := context.WithTimeout(ctx, writeTimeout)
			err := ws.Write(wctx, websocket.MessageText, msg)
			cancel()
			if err != nil {
				return
			}
		case <-ping.C:
			pctx, cancel := context.WithTimeout(ctx, writeTimeout)
			err := ws.Ping(pctx)
			cancel()
			if err != nil {
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

func (g *Gateway) add(user uuid.UUID, c *client) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	uc := g.users[user]
	if uc == nil {
		uc = &userConns{conns: map[*client]struct{}{}}
		sub, err := g.nc.Subscribe(events.UserSubject(user), func(m *nats.Msg) { g.deliver(user, m.Data) })
		if err != nil {
			return err
		}
		uc.sub = sub
		g.users[user] = uc
	}
	uc.conns[c] = struct{}{}
	return nil
}

func (g *Gateway) remove(user uuid.UUID, c *client) {
	g.mu.Lock()
	defer g.mu.Unlock()
	uc := g.users[user]
	if uc == nil {
		return
	}
	delete(uc.conns, c)
	if len(uc.conns) == 0 {
		uc.sub.Unsubscribe()
		delete(g.users, user)
	}
}

func (g *Gateway) deliver(user uuid.UUID, msg []byte) {
	g.mu.Lock()
	defer g.mu.Unlock()
	uc := g.users[user]
	if uc == nil {
		return
	}
	for c := range uc.conns {
		select {
		case c.send <- msg:
		default:
			// Never block the NATS callback on one stuck client. Dropping the
			// connection makes it reconnect and resync from the REST API.
			c.kick()
		}
	}
}

// Connections returns how many users and connections are currently open.
func (g *Gateway) Connections() (users, conns int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, uc := range g.users {
		conns += len(uc.conns)
	}
	return len(g.users), conns
}
