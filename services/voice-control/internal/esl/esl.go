// Package esl talks to FreeSWITCH over its Event Socket (inbound mode): one
// connection for commands, one for the event stream. It implements the
// calls.Media interface: admit a parked call into a room's conference, reject
// a call, and list rooms for resync.
package esl

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"parley/services/voice-control/internal/calls"
)

// ConferenceProfile is the conference.conf profile used for call rooms.
const ConferenceProfile = "parley"

// ErrNotFound means the call is already gone (e.g. the caller hung up).
var ErrNotFound = errors.New("esl: no such channel")

type Client struct {
	addr, password string

	mu   sync.Mutex // serializes commands on cmd
	cmd  *conn
	dial func(ctx context.Context) (*conn, error)
}

func New(addr, password string) *Client {
	c := &Client{addr: addr, password: password}
	c.dial = func(ctx context.Context) (*conn, error) { return dial(ctx, addr, password) }
	return c
}

// conn is one authenticated Event Socket connection.
type conn struct {
	nc net.Conn
	r  *textproto.Reader
}

// message is one framed ESL message: outer headers plus an optional body.
type message struct {
	header textproto.MIMEHeader
	body   []byte
}

func dial(ctx context.Context, addr, password string) (*conn, error) {
	var d net.Dialer
	nc, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	c := &conn{nc: nc, r: textproto.NewReader(bufio.NewReader(nc))}
	m, err := c.read()
	if err != nil || m.header.Get("Content-Type") != "auth/request" {
		nc.Close()
		return nil, fmt.Errorf("esl: unexpected greeting: %v", err)
	}
	reply, err := c.send("auth " + password)
	if err != nil {
		nc.Close()
		return nil, err
	}
	if !strings.HasPrefix(reply.header.Get("Reply-Text"), "+OK") {
		nc.Close()
		return nil, fmt.Errorf("esl: auth failed: %s", reply.header.Get("Reply-Text"))
	}
	return c, nil
}

func (c *conn) read() (message, error) {
	h, err := c.r.ReadMIMEHeader()
	if err != nil {
		return message{}, err
	}
	m := message{header: h}
	if n, _ := strconv.Atoi(h.Get("Content-Length")); n > 0 {
		m.body = make([]byte, n)
		if _, err := io.ReadFull(c.r.R, m.body); err != nil {
			return message{}, err
		}
	}
	return m, nil
}

// send writes a command and returns its reply. Only for connections that
// aren't receiving events (replies and events would interleave).
func (c *conn) send(cmd string) (message, error) {
	if strings.ContainsAny(cmd, "\r\n") {
		return message{}, errors.New("esl: line break in command")
	}
	c.nc.SetDeadline(time.Now().Add(10 * time.Second))
	defer c.nc.SetDeadline(time.Time{})
	if _, err := io.WriteString(c.nc, cmd+"\n\n"); err != nil {
		return message{}, err
	}
	return c.read()
}

// API runs a FreeSWITCH API command and returns its output. Output starting
// with "-ERR" becomes an error.
func (c *Client) API(ctx context.Context, cmd string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for attempt := 0; attempt < 2; attempt++ {
		if c.cmd == nil {
			cn, err := c.dial(ctx)
			if err != nil {
				return "", err
			}
			c.cmd = cn
		}
		m, err := c.cmd.send("api " + cmd)
		if err != nil {
			// Stale connection (FreeSWITCH restarted): reconnect once.
			c.cmd.nc.Close()
			c.cmd = nil
			continue
		}
		out := strings.TrimSpace(string(m.body))
		if strings.HasPrefix(out, "-ERR") {
			if strings.Contains(out, "No such channel") || strings.Contains(out, "INVALID SESSION") {
				return "", ErrNotFound
			}
			return "", fmt.Errorf("esl: %s: %s", cmd, out)
		}
		return out, nil
	}
	return "", errors.New("esl: command connection lost")
}

// Admit moves a parked call into the room's conference.
func (c *Client) Admit(ctx context.Context, callID string, room uuid.UUID) error {
	if _, err := uuid.Parse(callID); err != nil {
		return fmt.Errorf("esl: bad call id %q", callID)
	}
	_, err := c.API(ctx, fmt.Sprintf("uuid_transfer %s conference:%s@%s inline", callID, room, ConferenceProfile))
	return err
}

// Reject hangs a call up.
func (c *Client) Reject(ctx context.Context, callID string) error {
	if _, err := uuid.Parse(callID); err != nil {
		return fmt.Errorf("esl: bad call id %q", callID)
	}
	_, err := c.API(ctx, "uuid_kill "+callID+" CALL_REJECTED")
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

// Rooms lists the call rooms FreeSWITCH is running and who is in them
// (from each member's parley_user variable), for resync after a restart.
func (c *Client) Rooms(ctx context.Context) (map[uuid.UUID][]calls.Member, error) {
	out, err := c.API(ctx, "conference json_list")
	if err != nil {
		return nil, err
	}
	rooms := map[uuid.UUID][]calls.Member{}
	if !strings.HasPrefix(out, "[") { // "No active conferences."
		return rooms, nil
	}
	var confs []struct {
		Name    string `json:"conference_name"`
		Members []struct {
			UUID string `json:"uuid"`
		} `json:"members"`
	}
	if err := json.Unmarshal([]byte(out), &confs); err != nil {
		return nil, fmt.Errorf("esl: conference json_list: %w", err)
	}
	for _, cf := range confs {
		room, err := uuid.Parse(cf.Name)
		if err != nil {
			continue // not one of ours
		}
		for _, m := range cf.Members {
			v, err := c.API(ctx, "uuid_getvar "+m.UUID+" parley_user")
			if err != nil {
				continue
			}
			if user, err := uuid.Parse(v); err == nil {
				rooms[room] = append(rooms[room], calls.Member{CallID: m.UUID, User: user})
			}
		}
	}
	return rooms, nil
}

// Run follows the event stream until ctx ends, reconnecting with backoff.
// onConnect runs after every (re)connect, before that connection's events,
// so the caller can resync.
func (c *Client) Run(ctx context.Context, onConnect func(context.Context), h calls.EventHandler) {
	backoff := time.Second
	for ctx.Err() == nil {
		connected, err := c.stream(ctx, onConnect, h)
		if ctx.Err() != nil {
			return
		}
		if connected {
			backoff = time.Second
		}
		slog.Warn("freeswitch event stream lost; reconnecting", "err", err, "in", backoff)
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

func (c *Client) stream(ctx context.Context, onConnect func(context.Context), h calls.EventHandler) (bool, error) {
	dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	ev, err := c.dial(dctx)
	cancel()
	if err != nil {
		return false, err
	}
	defer ev.nc.Close()
	stop := context.AfterFunc(ctx, func() { ev.nc.Close() })
	defer stop()

	reply, err := ev.send("event plain CHANNEL_PARK CHANNEL_HANGUP_COMPLETE")
	if err != nil {
		return false, err
	}
	if !strings.HasPrefix(reply.header.Get("Reply-Text"), "+OK") {
		return false, fmt.Errorf("esl: subscribe: %s", reply.header.Get("Reply-Text"))
	}
	slog.Info("freeswitch event stream connected")
	onConnect(ctx)

	for {
		m, err := ev.read()
		if err != nil {
			return true, err
		}
		if m.header.Get("Content-Type") != "text/event-plain" {
			continue
		}
		e := parseEvent(m.body)
		callID := e["Unique-ID"]
		switch e["Event-Name"] {
		case "CHANNEL_PARK":
			if room := e["variable_parley_room"]; room != "" {
				h.CallArrived(callID, room, e["variable_parley_user"])
			}
		case "CHANNEL_HANGUP_COMPLETE":
			h.CallEnded(callID)
		}
	}
}

// parseEvent decodes a text/event-plain body: "Key: url-encoded value" lines.
func parseEvent(body []byte) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(string(body), "\n") {
		k, v, ok := strings.Cut(line, ": ")
		if !ok {
			continue
		}
		if dv, err := url.PathUnescape(v); err == nil { // not QueryUnescape: "+" is literal
			v = dv
		}
		out[k] = v
	}
	return out
}
