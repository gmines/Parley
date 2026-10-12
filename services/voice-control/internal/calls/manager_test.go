package calls

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"parley/pkg/events"
	"parley/services/voice-control/internal/ari"
)

// fakeARI records actions and models bridges like Asterisk does.
type fakeARI struct {
	mu       sync.Mutex
	answered []string
	hungUp   []string
	vars     map[string]string
	bridges  map[string][]string
	failAdd  bool
}

func newFakeARI() *fakeARI {
	return &fakeARI{vars: map[string]string{}, bridges: map[string][]string{}}
}

func (f *fakeARI) Answer(_ context.Context, ch string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answered = append(f.answered, ch)
	return nil
}
func (f *fakeARI) Hangup(_ context.Context, ch string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hungUp = append(f.hungUp, ch)
	return nil
}
func (f *fakeARI) SetVar(_ context.Context, ch, name, value string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.vars[ch+"/"+name] = value
	return nil
}
func (f *fakeARI) GetVar(_ context.Context, ch, name string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.vars[ch+"/"+name]
	if !ok {
		return "", ari.ErrNotFound
	}
	return v, nil
}
func (f *fakeARI) CreateBridge(_ context.Context, id, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.bridges[id]; ok {
		return ari.ErrConflict // what Asterisk answers; the real client maps it to nil
	}
	f.bridges[id] = nil
	return nil
}
func (f *fakeARI) AddChannel(_ context.Context, b, ch string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failAdd {
		return ari.ErrNotFound
	}
	f.bridges[b] = append(f.bridges[b], ch)
	return nil
}
func (f *fakeARI) DestroyBridge(_ context.Context, b string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.bridges, b)
	return nil
}
func (f *fakeARI) ListBridges(context.Context) ([]ari.Bridge, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []ari.Bridge
	for id, chs := range f.bridges {
		out = append(out, ari.Bridge{ID: id, Channels: append([]string(nil), chs...)})
	}
	return out, nil
}

type fakeDir struct {
	convs map[uuid.UUID]struct {
		kind    string
		members []uuid.UUID
	}
	blocked map[[2]uuid.UUID]bool
}

func (d *fakeDir) Members(_ context.Context, conv, user uuid.UUID) (string, []uuid.UUID, error) {
	c, ok := d.convs[conv]
	if !ok {
		return "", nil, ErrNotMember
	}
	for _, m := range c.members {
		if m == user {
			return c.kind, c.members, nil
		}
	}
	return "", nil, ErrNotMember
}
func (d *fakeDir) Blocked(_ context.Context, a, b uuid.UUID) (bool, error) {
	return d.blocked[[2]uuid.UUID{a, b}] || d.blocked[[2]uuid.UUID{b, a}], nil
}

type fixture struct {
	m                 *Manager
	ari               *fakeARI
	dir               *fakeDir
	rec               *events.Recorder
	alice, bob, carol uuid.UUID
	dm, group         uuid.UUID
}

func setup() *fixture {
	f := &fixture{ari: newFakeARI(), rec: &events.Recorder{},
		alice: uuid.New(), bob: uuid.New(), carol: uuid.New(), dm: uuid.New(), group: uuid.New()}
	f.dir = &fakeDir{
		convs: map[uuid.UUID]struct {
			kind    string
			members []uuid.UUID
		}{
			f.dm:    {"dm", []uuid.UUID{f.alice, f.bob}},
			f.group: {"group", []uuid.UUID{f.alice, f.bob, f.carol}},
		},
		blocked: map[[2]uuid.UUID]bool{},
	}
	f.m = NewManager(f.ari, f.dir, f.rec)
	return f
}

func (f *fixture) start(ch string, conv, user uuid.UUID) {
	f.m.HandleEvent(ari.Event{Type: "StasisStart", Channel: &ari.Channel{ID: ch}, Args: []string{conv.String(), user.String()}})
}

func (f *fixture) end(ch string) {
	f.m.HandleEvent(ari.Event{Type: "StasisEnd", Channel: &ari.Channel{ID: ch}})
}

func (f *fixture) types() []string {
	var out []string
	for _, e := range f.rec.Events() {
		out = append(out, e.Type)
	}
	return out
}

func TestCallLifecycle(t *testing.T) {
	f := setup()
	f.start("ch-a", f.group, f.alice)

	c, ok := f.m.Get(f.group)
	if !ok || c.StartedBy != f.alice || len(c.Participants) != 1 {
		t.Fatalf("after first join: %+v %v", c, ok)
	}
	if got := strings.Join(f.types(), ","); got != "call.started,call.updated" {
		t.Fatalf("events = %s", got)
	}
	started := f.rec.Events()[0]
	if len(started.Recipients) != 3 {
		t.Fatalf("call.started recipients = %v, want all 3 members (they ring)", started.Recipients)
	}
	if f.ari.bridges[bridgePrefix+f.group.String()] == nil || f.ari.vars["ch-a/"+userVariable] != f.alice.String() {
		t.Fatalf("bridge/vars = %v / %v", f.ari.bridges, f.ari.vars)
	}

	f.start("ch-b", f.group, f.bob)
	f.start("ch-b2", f.group, f.bob) // bob's second device
	c, _ = f.m.Get(f.group)
	if len(c.Participants) != 2 {
		t.Fatalf("participants = %v, want alice and bob once each", c.Participants)
	}

	f.end("ch-a")
	f.end("ch-b")
	if _, ok := f.m.Get(f.group); !ok {
		t.Fatal("call ended while bob's second device is still in it")
	}
	f.end("ch-b2")
	if _, ok := f.m.Get(f.group); ok {
		t.Fatal("call still active after everyone left")
	}
	if _, ok := f.ari.bridges[bridgePrefix+f.group.String()]; ok {
		t.Fatal("bridge not destroyed")
	}
	ty := f.types()
	if ty[len(ty)-1] != "call.ended" {
		t.Fatalf("last event = %s, want call.ended", ty[len(ty)-1])
	}
	f.end("ch-unknown") // a channel that was never admitted: ignored
}

func TestRejections(t *testing.T) {
	f := setup()
	stranger := uuid.New()
	f.start("ch-1", f.group, stranger)   // not a member
	f.start("ch-2", uuid.New(), f.alice) // no such conversation
	f.m.HandleEvent(ari.Event{Type: "StasisStart", Channel: &ari.Channel{ID: "ch-3"}, Args: []string{"not-a-uuid", f.alice.String()}})
	f.m.HandleEvent(ari.Event{Type: "StasisStart", Channel: &ari.Channel{ID: "ch-4"}}) // no args

	f.dir.blocked[[2]uuid.UUID{f.bob, f.alice}] = true
	f.start("ch-5", f.dm, f.alice)    // blocked DM
	f.start("ch-6", f.group, f.alice) // groups aren't affected by a block between two members

	if got := strings.Join(f.ari.hungUp, ","); got != "ch-1,ch-2,ch-3,ch-4,ch-5" {
		t.Fatalf("hung up = %s", got)
	}
	if len(f.ari.answered) != 1 || f.ari.answered[0] != "ch-6" {
		t.Fatalf("answered = %v", f.ari.answered)
	}
	if _, err := f.m.Authorize(context.Background(), f.alice, f.dm); !errors.Is(err, ErrCannotCall) {
		t.Fatalf("authorize blocked DM err = %v", err)
	}
}

func TestBridgeFailureHangsUp(t *testing.T) {
	f := setup()
	f.ari.failAdd = true
	f.start("ch-a", f.dm, f.alice)
	if _, ok := f.m.Get(f.dm); ok || len(f.ari.hungUp) != 1 {
		t.Fatalf("failed join left state: hungUp=%v", f.ari.hungUp)
	}
	if len(f.rec.Events()) != 0 {
		t.Fatal("events published for a failed join")
	}
}

func TestResyncAfterRestart(t *testing.T) {
	f := setup()
	f.start("ch-a", f.group, f.alice)
	f.start("ch-b", f.group, f.bob)
	f.ari.bridges[bridgePrefix+uuid.New().String()] = nil // leftover empty room
	f.ari.bridges["someone-elses-bridge"] = []string{"x"} // not ours: untouched

	fresh := NewManager(f.ari, f.dir, f.rec) // voice-control restarted
	fresh.Resync(context.Background())
	c, ok := fresh.Get(f.group)
	if !ok || len(c.Participants) != 2 {
		t.Fatalf("resynced call = %+v %v", c, ok)
	}
	if len(f.ari.bridges) != 2 {
		t.Fatalf("bridges after resync = %v, want ours + the foreign one", f.ari.bridges)
	}
	if active := fresh.Active(context.Background(), f.carol); len(active) != 1 {
		t.Fatalf("carol sees %d active calls, want 1", len(active))
	}
	if active := fresh.Active(context.Background(), uuid.New()); len(active) != 0 {
		t.Fatal("a stranger sees the call")
	}
}

func TestDecline(t *testing.T) {
	f := setup()
	if err := f.m.Decline(context.Background(), f.bob, f.dm); err != nil {
		t.Fatal(err)
	}
	e := f.rec.Events()[0]
	if e.Type != events.CallDeclined || len(e.Recipients) != 2 {
		t.Fatalf("decline event = %+v", e)
	}
	if err := f.m.Decline(context.Background(), uuid.New(), f.dm); !errors.Is(err, ErrNotMember) {
		t.Fatalf("stranger decline err = %v", err)
	}
}

func TestMintMatchesKamailioFormat(t *testing.T) {
	secret := []byte("s3cret")
	user, conv := uuid.New(), uuid.New()
	now := time.Unix(1_700_000_000, 0)
	c := Mint(secret, user, conv, "sip:192.0.2.1:5060;transport=tcp", "192.0.2.1", 5*time.Minute, now)

	wantUser := fmt.Sprintf("%d:%s.%s", now.Add(5*time.Minute).Unix(), user, conv)
	if c.Username != wantUser {
		t.Fatalf("username = %q, want %q", c.Username, wantUser)
	}
	mac := hmac.New(sha1.New, secret)
	mac.Write([]byte(wantUser))
	if c.Password != base64.StdEncoding.EncodeToString(mac.Sum(nil)) {
		t.Fatal("password is not base64(HMAC-SHA1(secret, username))")
	}
	if c.URI != "sip:"+conv.String()+"@192.0.2.1" || c.Realm != "parley" {
		t.Fatalf("uri/realm = %s / %s", c.URI, c.Realm)
	}
}
