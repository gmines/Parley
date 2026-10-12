// Package ari is a minimal client for the parts of Asterisk's REST Interface
// that voice-control uses: channels, mixing bridges and the event stream.
package ari

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coder/websocket"
)

type Client struct {
	base       *url.URL // e.g. http://127.0.0.1:8088/ari
	user, pass string
	http       *http.Client
}

func New(baseURL, user, pass string) (*Client, error) {
	u, err := url.Parse(strings.TrimSuffix(baseURL, "/"))
	if err != nil {
		return nil, err
	}
	return &Client{base: u, user: user, pass: pass, http: &http.Client{Timeout: 10 * time.Second}}, nil
}

// ErrNotFound means the channel or bridge no longer exists (e.g. the caller
// already hung up). Callers can usually ignore it.
var ErrNotFound = errors.New("ari: not found")

type Channel struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	State string `json:"state"`
}

type Bridge struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Channels []string `json:"channels"`
}

// Event is an ARI event. Only the fields voice-control reads are decoded.
type Event struct {
	Type        string   `json:"type"`
	Application string   `json:"application"`
	Args        []string `json:"args"`
	Channel     *Channel `json:"channel"`
	Bridge      *Bridge  `json:"bridge"`
}

func (c *Client) do(ctx context.Context, method, path string, q url.Values, out any) error {
	u := *c.base
	u.Path += path
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, method, u.String(), nil)
	if err != nil {
		return err
	}
	req.SetBasicAuth(c.user, c.pass)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("ari %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNotFound:
		return ErrNotFound
	case http.StatusConflict:
		return ErrConflict
	}
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<10))
		return fmt.Errorf("ari %s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(body)))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func (c *Client) Answer(ctx context.Context, channel string) error {
	return c.do(ctx, http.MethodPost, "/channels/"+url.PathEscape(channel)+"/answer", nil, nil)
}

func (c *Client) Hangup(ctx context.Context, channel string) error {
	return c.do(ctx, http.MethodDelete, "/channels/"+url.PathEscape(channel), nil, nil)
}

func (c *Client) SetVar(ctx context.Context, channel, name, value string) error {
	return c.do(ctx, http.MethodPost, "/channels/"+url.PathEscape(channel)+"/variable",
		url.Values{"variable": {name}, "value": {value}}, nil)
}

func (c *Client) GetVar(ctx context.Context, channel, name string) (string, error) {
	var out struct {
		Value string `json:"value"`
	}
	err := c.do(ctx, http.MethodGet, "/channels/"+url.PathEscape(channel)+"/variable", url.Values{"variable": {name}}, &out)
	return out.Value, err
}

// ErrConflict is returned when creating something that already exists.
var ErrConflict = errors.New("ari: already exists")

// CreateBridge creates the mixing bridge with this id. An existing bridge
// with the same id is fine (Asterisk answers 409 Conflict for it).
func (c *Client) CreateBridge(ctx context.Context, id, name string) error {
	err := c.do(ctx, http.MethodPost, "/bridges/"+url.PathEscape(id), url.Values{"type": {"mixing"}, "name": {name}}, nil)
	if errors.Is(err, ErrConflict) {
		return nil
	}
	return err
}

func (c *Client) AddChannel(ctx context.Context, bridge, channel string) error {
	return c.do(ctx, http.MethodPost, "/bridges/"+url.PathEscape(bridge)+"/addChannel", url.Values{"channel": {channel}}, nil)
}

func (c *Client) DestroyBridge(ctx context.Context, bridge string) error {
	return c.do(ctx, http.MethodDelete, "/bridges/"+url.PathEscape(bridge), nil, nil)
}

func (c *Client) ListBridges(ctx context.Context) ([]Bridge, error) {
	var out []Bridge
	err := c.do(ctx, http.MethodGet, "/bridges", nil, &out)
	return out, err
}

// Run subscribes to app's events and calls handle for each one until ctx is
// cancelled, reconnecting with backoff. onConnect runs after every
// (re)connect, before any events of that connection are handled, so the
// caller can resync state it may have missed.
func (c *Client) Run(ctx context.Context, app string, onConnect func(context.Context), handle func(Event)) {
	backoff := time.Second
	for ctx.Err() == nil {
		connected, err := c.stream(ctx, app, onConnect, handle)
		if ctx.Err() != nil {
			return
		}
		if connected {
			backoff = time.Second
		}
		slog.Warn("ari event stream lost; reconnecting", "err", err, "in", backoff)
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

func (c *Client) stream(ctx context.Context, app string, onConnect func(context.Context), handle func(Event)) (bool, error) {
	u := *c.base
	u.Scheme = map[string]string{"https": "wss"}[u.Scheme]
	if u.Scheme == "" {
		u.Scheme = "ws"
	}
	u.Path += "/events"
	u.RawQuery = url.Values{"app": {app}, "subscribeAll": {"false"}}.Encode()
	hdr := http.Header{"Authorization": {"Basic " + base64.StdEncoding.EncodeToString([]byte(c.user+":"+c.pass))}}

	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	ws, _, err := websocket.Dial(dialCtx, u.String(), &websocket.DialOptions{HTTPHeader: hdr})
	cancel()
	if err != nil {
		return false, err
	}
	defer ws.CloseNow()
	ws.SetReadLimit(1 << 20)
	slog.Info("ari event stream connected", "app", app)
	onConnect(ctx)

	for {
		_, data, err := ws.Read(ctx)
		if err != nil {
			return true, err
		}
		var e Event
		if err := json.Unmarshal(data, &e); err != nil {
			slog.Warn("bad ari event", "err", err)
			continue
		}
		handle(e)
	}
}
