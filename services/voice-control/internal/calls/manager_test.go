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
)

// fakeMedia records admits and rejects and models rooms like FreeSWITCH.
type fakeMedia struct {
	mu        sync.Mutex
	admitted  []string
	rejected  []string
	rooms     map[uuid.UUID][]Member
	failAdmit bool
}

func newFakeMedia() *fakeMedia { return &fakeMedia{rooms: map[uuid.UUID][]Member{}} }

func (f *fakeMedia) Admit(_ context.Context, callID string, room uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failAdmit {
		return errors.New("no such channel")
	}
	f.admitted = append(f.admitted, callID)
	return nil
}

func (f *fakeMedia) Reject(_ context.Context, callID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rejected = append(f.rejected, callID)
	return nil
}

func (f *fakeMedia) Rooms(context.Context) (map[uuid.UUID][]Member, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[uuid.UUID][]Member{}
	for k, v := range f.rooms {
		out[k] = append([]Member(nil), v...)
	}
	return out, nil
}

type conv struct {
	kind    string
	members []uuid.UUID
}

type fakeDir struct {
	convs   map[uuid.UUID]conv
	blocked map[[2]uuid.UUID]bool
}

func (d *fakeDir) Members(_ context.Context, id, user uuid.UUID) (string, []uuid.UUID, error) {
	c, ok := d.convs[id]
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
	media             *fakeMedia
	dir               *fakeDir
	rec               *events.Recorder
	alice, bob, carol uuid.UUID
	dm, group         uuid.UUID
}

func setup() *fixture {
	f := &fixture{media: newFakeMedia(), rec: &events.Recorder{},
		alice: uuid.New(), bob: uuid.New(), carol: uuid.New(), dm: uuid.New(), group: uuid.New()}
	f.dir = &fakeDir{
		convs: map[uuid.UUID]conv{
			f.dm:    {"dm", []uuid.UUID{f.alice, f.bob}},
			f.group: {"group", []uuid.UUID{f.alice, f.bob, f.carol}},
		},
		blocked: map[[2]uuid.UUID]bool{},
	}
	f.m = NewManager(f.media, f.dir, f.rec)
	return f
}

func (f *fixture) arrive(call string, room, user uuid.UUID) {
	f.m.CallArrived(call, room.String(), user.String())
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
	f.arrive("call-a", f.group, f.alice)

	c, ok := f.m.Get(f.group)
	if !ok || c.StartedBy != f.alice || len(c.Participants) != 1 {
		t.Fatalf("after first join: %+v %v", c, ok)
	}
	if got := strings.Join(f.types(), ","); got != "call.started,call.updated" {
		t.Fatalf("events = %s", got)
	}
	if started := f.rec.Events()[0]; len(started.Recipients) != 3 {
		t.Fatalf("call.started recipients = %v, want all 3 members (they ring)", started.Recipients)
	}

	f.arrive("call-b", f.group, f.bob)
	f.arrive("call-b2", f.group, f.bob) // bob's second device
	if c, _ = f.m.Get(f.group); len(c.Participants) != 2 {
		t.Fatalf("participants = %v, want alice and bob once each", c.Participants)
	}
	if got := strings.Join(f.media.admitted, ","); got != "call-a,call-b,call-b2" {
		t.Fatalf("admitted = %s", got)
	}

	f.m.CallEnded("call-a")
	f.m.CallEnded("call-b")
	if _, ok := f.m.Get(f.group); !ok {
		t.Fatal("call ended while bob's second device is still in it")
	}
	f.m.CallEnded("call-b2")
	if _, ok := f.m.Get(f.group); ok {
		t.Fatal("call still active after everyone left")
	}
	ty := f.types()
	if ty[len(ty)-1] != "call.ended" {
		t.Fatalf("last event = %s, want call.ended", ty[len(ty)-1])
	}
	f.m.CallEnded("never-admitted") // ignored
}

func TestRejections(t *testing.T) {
	f := setup()
	f.arrive("c1", f.group, uuid.New()) // not a member
	f.arrive("c2", uuid.New(), f.alice) // no such conversation
	f.m.CallArrived("c3", "not-a-uuid", f.alice.String())
	f.m.CallArrived("c4", f.group.String(), "") // no verified user header

	f.dir.blocked[[2]uuid.UUID{f.bob, f.alice}] = true
	f.arrive("c5", f.dm, f.alice)    // blocked DM
	f.arrive("c6", f.group, f.alice) // a block between two members doesn't affect groups

	if got := strings.Join(f.media.rejected, ","); got != "c1,c2,c3,c4,c5" {
		t.Fatalf("rejected = %s", got)
	}
	if got := strings.Join(f.media.admitted, ","); got != "c6" {
		t.Fatalf("admitted = %s", got)
	}
	if _, err := f.m.Authorize(context.Background(), f.alice, f.dm); !errors.Is(err, ErrCannotCall) {
		t.Fatalf("authorize blocked DM err = %v", err)
	}
}

func TestAdmitFailureRejects(t *testing.T) {
	f := setup()
	f.media.failAdmit = true
	f.arrive("call-a", f.dm, f.alice)
	if _, ok := f.m.Get(f.dm); ok || len(f.media.rejected) != 1 {
		t.Fatalf("failed admit left state: rejected=%v", f.media.rejected)
	}
	if len(f.rec.Events()) != 0 {
		t.Fatal("events published for a failed join")
	}
}

func TestResyncAfterRestart(t *testing.T) {
	f := setup()
	f.media.rooms[f.group] = []Member{{"call-a", f.alice}, {"call-b", f.bob}}
	f.media.rooms[uuid.New()] = nil // empty: ignored

	fresh := NewManager(f.media, f.dir, f.rec) // voice-control restarted
	fresh.Resync(context.Background())
	c, ok := fresh.Get(f.group)
	if !ok || len(c.Participants) != 2 {
		t.Fatalf("resynced call = %+v %v", c, ok)
	}
	if active := fresh.Active(context.Background(), f.carol); len(active) != 1 {
		t.Fatalf("carol sees %d active calls, want 1", len(active))
	}
	if active := fresh.Active(context.Background(), uuid.New()); len(active) != 0 {
		t.Fatal("a stranger sees the call")
	}
	fresh.CallEnded("call-a")
	fresh.CallEnded("call-b")
	if _, ok := fresh.Get(f.group); ok {
		t.Fatal("resynced call didn't end")
	}
}

func TestDecline(t *testing.T) {
	f := setup()
	if err := f.m.Decline(context.Background(), f.bob, f.dm); err != nil {
		t.Fatal(err)
	}
	if e := f.rec.Events()[0]; e.Type != events.CallDeclined || len(e.Recipients) != 2 {
		t.Fatalf("decline event = %+v", e)
	}
	if err := f.m.Decline(context.Background(), uuid.New(), f.dm); !errors.Is(err, ErrNotMember) {
		t.Fatalf("stranger decline err = %v", err)
	}
}

func TestMintMatchesKamailioFormat(t *testing.T) {
	secret := []byte("s3cret")
	user, room := uuid.New(), uuid.New()
	now := time.Unix(1_700_000_000, 0)
	c := Mint(secret, user, room, "sip:192.0.2.1:5060;transport=tcp", "192.0.2.1", 5*time.Minute, now)

	wantUser := fmt.Sprintf("%d:%s.%s", now.Add(5*time.Minute).Unix(), user, room)
	if c.Username != wantUser {
		t.Fatalf("username = %q, want %q", c.Username, wantUser)
	}
	mac := hmac.New(sha1.New, secret)
	mac.Write([]byte(wantUser))
	if c.Password != base64.StdEncoding.EncodeToString(mac.Sum(nil)) {
		t.Fatal("password is not base64(HMAC-SHA1(secret, username))")
	}
	if c.URI != "sip:"+room.String()+"@192.0.2.1" || c.Realm != "parley" {
		t.Fatalf("uri/realm = %s / %s", c.URI, c.Realm)
	}
}
