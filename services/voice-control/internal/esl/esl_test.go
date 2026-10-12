package esl

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/textproto"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// fakeFS speaks enough of the Event Socket protocol for the client: auth,
// "api" commands answered from a table, and an event subscription.
type fakeFS struct {
	t        *testing.T
	ln       net.Listener
	password string

	mu       sync.Mutex
	commands []string
	answers  map[string]string // command prefix -> api body
	evConn   net.Conn
	subbed   chan struct{}
}

func newFakeFS(t *testing.T) *fakeFS {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeFS{t: t, ln: ln, password: "pw", answers: map[string]string{}, subbed: make(chan struct{}, 4)}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(c)
		}
	}()
	return f
}

func (f *fakeFS) serve(c net.Conn) {
	defer c.Close()
	r := textproto.NewReader(bufio.NewReader(c))
	fmt.Fprint(c, "Content-Type: auth/request\n\n")
	for {
		line, err := r.ReadLine()
		if err != nil {
			return
		}
		if line == "" {
			continue
		}
		switch {
		case strings.HasPrefix(line, "auth "):
			if strings.TrimPrefix(line, "auth ") == f.password {
				fmt.Fprint(c, "Content-Type: command/reply\nReply-Text: +OK accepted\n\n")
			} else {
				fmt.Fprint(c, "Content-Type: command/reply\nReply-Text: -ERR invalid\n\n")
				return
			}
		case strings.HasPrefix(line, "event plain "):
			fmt.Fprint(c, "Content-Type: command/reply\nReply-Text: +OK event listener enabled plain\n\n")
			f.mu.Lock()
			f.evConn = c
			f.mu.Unlock()
			f.subbed <- struct{}{}
		case strings.HasPrefix(line, "api "):
			cmd := strings.TrimPrefix(line, "api ")
			f.mu.Lock()
			f.commands = append(f.commands, cmd)
			body := "+OK"
			for prefix, ans := range f.answers {
				if strings.HasPrefix(cmd, prefix) {
					body = ans
				}
			}
			f.mu.Unlock()
			fmt.Fprintf(c, "Content-Type: api/response\nContent-Length: %d\n\n%s", len(body), body)
		}
	}
}

// sendEvent pushes a text/event-plain event with url-encoded values.
func (f *fakeFS) sendEvent(headers map[string]string) {
	var b strings.Builder
	for k, v := range headers {
		fmt.Fprintf(&b, "%s: %s\n", k, strings.ReplaceAll(v, " ", "%20"))
	}
	b.WriteString("\n")
	f.mu.Lock()
	defer f.mu.Unlock()
	fmt.Fprintf(f.evConn, "Content-Length: %d\nContent-Type: text/event-plain\n\n%s", b.Len(), b.String())
}

type handler struct {
	mu      sync.Mutex
	arrived [][3]string
	ended   []string
}

func (h *handler) CallArrived(callID, room, user string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.arrived = append(h.arrived, [3]string{callID, room, user})
}

func (h *handler) CallEnded(callID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ended = append(h.ended, callID)
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

func TestCommands(t *testing.T) {
	f := newFakeFS(t)
	c := New(f.ln.Addr().String(), "pw")
	ctx := context.Background()
	call, room := uuid.NewString(), uuid.New()

	if err := c.Admit(ctx, call, room); err != nil {
		t.Fatal(err)
	}
	if err := c.Reject(ctx, call); err != nil {
		t.Fatal(err)
	}
	f.answers["uuid_kill"] = "-ERR No such channel!"
	if err := c.Reject(ctx, call); err != nil {
		t.Fatalf("rejecting a vanished call must succeed, got %v", err)
	}
	f.answers["uuid_transfer"] = "-ERR something broke"
	if err := c.Admit(ctx, call, room); err == nil {
		t.Fatal("admit error not reported")
	}
	if err := c.Admit(ctx, "x; uuid_kill all", room); err == nil {
		t.Fatal("non-uuid call id accepted")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	want := fmt.Sprintf("uuid_transfer %s conference:%s@parley inline", call, room)
	if f.commands[0] != want {
		t.Fatalf("command = %q, want %q", f.commands[0], want)
	}
}

func TestWrongPassword(t *testing.T) {
	f := newFakeFS(t)
	c := New(f.ln.Addr().String(), "nope")
	if _, err := c.API(context.Background(), "status"); err == nil || !strings.Contains(err.Error(), "auth") {
		t.Fatalf("err = %v, want auth failure", err)
	}
}

func TestRooms(t *testing.T) {
	f := newFakeFS(t)
	room, a, b := uuid.New(), uuid.New(), uuid.New()
	callA, callB := uuid.NewString(), uuid.NewString()
	f.answers["conference json_list"] = fmt.Sprintf(`[{"conference_name":"%s","members":[{"uuid":"%s"},{"uuid":"%s"}]},{"conference_name":"not-ours","members":[]}]`, room, callA, callB)
	f.answers["uuid_getvar "+callA] = a.String()
	f.answers["uuid_getvar "+callB] = b.String()
	rooms, err := New(f.ln.Addr().String(), "pw").Rooms(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rooms) != 1 || len(rooms[room]) != 2 || rooms[room][0].User != a {
		t.Fatalf("rooms = %+v", rooms)
	}

	f.answers["conference json_list"] = "No active conferences."
	rooms, err = New(f.ln.Addr().String(), "pw").Rooms(context.Background())
	if err != nil || len(rooms) != 0 {
		t.Fatalf("no conferences: %v %v", rooms, err)
	}
}

func TestEventStream(t *testing.T) {
	f := newFakeFS(t)
	c := New(f.ln.Addr().String(), "pw")
	h := &handler{}
	resyncs := 0
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx, func(context.Context) { resyncs++ }, h)
	<-f.subbed

	room, user, call := uuid.NewString(), uuid.NewString(), uuid.NewString()
	f.sendEvent(map[string]string{"Event-Name": "CHANNEL_PARK", "Unique-ID": call,
		"variable_parley_room": room, "variable_parley_user": user, "Caller-Caller-ID-Name": "has spaces"})
	f.sendEvent(map[string]string{"Event-Name": "CHANNEL_PARK", "Unique-ID": uuid.NewString()}) // not a Parley room: ignored
	f.sendEvent(map[string]string{"Event-Name": "CHANNEL_HANGUP_COMPLETE", "Unique-ID": call})

	waitFor(t, func() bool { h.mu.Lock(); defer h.mu.Unlock(); return len(h.ended) == 1 })
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.arrived) != 1 || h.arrived[0] != [3]string{call, room, user} {
		t.Fatalf("arrived = %v", h.arrived)
	}
	if resyncs != 1 {
		t.Fatalf("resyncs = %d, want 1 (on connect)", resyncs)
	}
}
