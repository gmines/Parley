package messaging

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"parley/pkg/db/dbtest"
	"parley/pkg/events"
	"parley/services/api/internal/social"
	"parley/services/api/migrations"
)

type fixture struct {
	svc    *Service
	social *social.Service
	rec    *events.Recorder
	users  map[string]uuid.UUID
}

func setup(t *testing.T, names ...string) *fixture {
	t.Helper()
	pool := dbtest.New(t, migrations.FS)
	rec := &events.Recorder{}
	soc := social.NewService(pool, events.Discard{})
	f := &fixture{svc: NewService(pool, soc, rec), social: soc, rec: rec, users: map[string]uuid.UUID{}}
	for _, n := range names {
		var id uuid.UUID
		if err := pool.QueryRow(context.Background(),
			`INSERT INTO users (username, display_name, password_hash) VALUES ($1, $1, 'x') RETURNING id`, n,
		).Scan(&id); err != nil {
			t.Fatal(err)
		}
		f.users[n] = id
	}
	return f
}

func (f *fixture) befriend(t *testing.T, a, b string) {
	t.Helper()
	ctx := context.Background()
	res, err := f.social.SendRequest(ctx, f.users[a], b)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.social.AcceptRequest(ctx, f.users[b], res.Request.ID); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) send(t *testing.T, from string, conv uuid.UUID, text string) Message {
	t.Helper()
	m, err := f.svc.SendMessage(context.Background(), f.users[from], conv, text, "")
	if err != nil {
		t.Fatalf("send %q: %v", text, err)
	}
	return m
}

func TestDMBetweenFriends(t *testing.T) {
	f := setup(t, "alice", "bob", "eve")
	ctx := context.Background()
	alice, bob, eve := f.users["alice"], f.users["bob"], f.users["eve"]

	if _, err := f.svc.OpenDM(ctx, alice, bob); !errors.Is(err, ErrNotFriends) {
		t.Fatalf("DM with non-friend err = %v", err)
	}
	if _, err := f.svc.OpenDM(ctx, alice, alice); !errors.Is(err, ErrSelf) {
		t.Fatalf("DM with self err = %v", err)
	}
	f.befriend(t, "alice", "bob")

	dm, err := f.svc.OpenDM(ctx, alice, bob)
	if err != nil {
		t.Fatal(err)
	}
	if dm.Kind != "dm" || len(dm.Members) != 2 {
		t.Fatalf("dm = %+v", dm)
	}
	again, err := f.svc.OpenDM(ctx, bob, alice)
	if err != nil || again.ID != dm.ID {
		t.Fatalf("reopen from other side: %v id %v, want same conversation", err, again.ID)
	}

	m, err := f.svc.SendMessage(ctx, alice, dm.ID, "  hello bob \n", "nonce-1")
	if err != nil {
		t.Fatal(err)
	}
	if m.Content != "hello bob" || m.Author == nil || m.Author.ID != alice || m.Nonce != "nonce-1" {
		t.Fatalf("message = %+v", m)
	}

	// Outsiders can't read or post.
	if _, err := f.svc.ListMessages(ctx, eve, dm.ID, nil, nil, 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("outsider list err = %v", err)
	}
	if _, err := f.svc.SendMessage(ctx, eve, dm.ID, "hi", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("outsider send err = %v", err)
	}

	// The message event goes to both members, with the nonce for the sender's client.
	var created []events.Event
	for _, e := range f.rec.Events() {
		if e.Type == events.MessageCreated {
			created = append(created, e)
		}
	}
	if len(created) != 1 || len(created[0].Recipients) != 2 {
		t.Fatalf("message.created events = %+v", created)
	}
}

func TestConcurrentOpenDMCreatesOne(t *testing.T) {
	f := setup(t, "alice", "bob")
	f.befriend(t, "alice", "bob")
	ctx := context.Background()

	ids := make([]uuid.UUID, 10)
	var wg sync.WaitGroup
	for i := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a, b := f.users["alice"], f.users["bob"]
			if i%2 == 1 {
				a, b = b, a
			}
			c, err := f.svc.OpenDM(ctx, a, b)
			if err != nil {
				t.Error(err)
				return
			}
			ids[i] = c.ID
		}()
	}
	wg.Wait()
	for _, id := range ids {
		if id != ids[0] {
			t.Fatalf("concurrent opens produced different DMs: %v", ids)
		}
	}
}

func TestBlockStopsDMs(t *testing.T) {
	f := setup(t, "alice", "bob")
	ctx := context.Background()
	f.befriend(t, "alice", "bob")
	dm, _ := f.svc.OpenDM(ctx, f.users["alice"], f.users["bob"])
	f.send(t, "alice", dm.ID, "before the block")

	if err := f.social.Block(ctx, f.users["bob"], f.users["alice"]); err != nil {
		t.Fatal(err)
	}
	for _, who := range []string{"alice", "bob"} {
		if _, err := f.svc.SendMessage(ctx, f.users[who], dm.ID, "hello?", ""); !errors.Is(err, ErrCannotMessage) {
			t.Fatalf("%s sending after block err = %v", who, err)
		}
	}
	// History stays readable, and the DM can still be opened, but not used.
	msgs, err := f.svc.ListMessages(ctx, f.users["alice"], dm.ID, nil, nil, 0)
	if err != nil || len(msgs) != 1 {
		t.Fatalf("history after block: %d msgs, err %v", len(msgs), err)
	}
	if _, err := f.svc.OpenDM(ctx, f.users["alice"], f.users["bob"]); err != nil {
		t.Fatalf("reopen existing DM after block: %v", err)
	}
}

func TestPagingAndOrder(t *testing.T) {
	f := setup(t, "alice", "bob")
	ctx := context.Background()
	f.befriend(t, "alice", "bob")
	dm, _ := f.svc.OpenDM(ctx, f.users["alice"], f.users["bob"])
	var sent []Message
	for i := 0; i < 120; i++ {
		sent = append(sent, f.send(t, []string{"alice", "bob"}[i%2], dm.ID, fmt.Sprintf("m%03d", i)))
	}
	alice := f.users["alice"]

	latest, _ := f.svc.ListMessages(ctx, alice, dm.ID, nil, nil, 0)
	if len(latest) != 50 || latest[0].Content != "m070" || latest[49].Content != "m119" {
		t.Fatalf("latest page: %d msgs, %s..%s", len(latest), latest[0].Content, latest[len(latest)-1].Content)
	}
	older, _ := f.svc.ListMessages(ctx, alice, dm.ID, &latest[0].ID, nil, 100)
	if len(older) != 70 || older[0].Content != "m000" || older[69].Content != "m069" {
		t.Fatalf("older page: %d msgs", len(older))
	}
	newer, _ := f.svc.ListMessages(ctx, alice, dm.ID, nil, &sent[115].ID, 0)
	if len(newer) != 4 || newer[0].Content != "m116" {
		t.Fatalf("catch-up after m115: %+v", newer)
	}
}

func TestUnreadAndMarkRead(t *testing.T) {
	f := setup(t, "alice", "bob")
	ctx := context.Background()
	f.befriend(t, "alice", "bob")
	alice, bob := f.users["alice"], f.users["bob"]
	dm, _ := f.svc.OpenDM(ctx, alice, bob)

	f.send(t, "alice", dm.ID, "one")
	f.send(t, "alice", dm.ID, "two")
	last := f.send(t, "alice", dm.ID, "three")

	unread := func(user uuid.UUID) int {
		c, err := f.svc.GetConversation(ctx, user, dm.ID)
		if err != nil {
			t.Fatal(err)
		}
		return c.UnreadCount
	}
	if unread(bob) != 3 || unread(alice) != 0 {
		t.Fatalf("unread bob=%d alice=%d, want 3 and 0 (own messages count as read)", unread(bob), unread(alice))
	}
	if err := f.svc.MarkRead(ctx, bob, dm.ID, last.ID); err != nil {
		t.Fatal(err)
	}
	if unread(bob) != 0 {
		t.Fatalf("unread after mark read = %d", unread(bob))
	}
	// Marking an older message doesn't move the marker backwards.
	msgs, _ := f.svc.ListMessages(ctx, bob, dm.ID, nil, nil, 0)
	if err := f.svc.MarkRead(ctx, bob, dm.ID, msgs[0].ID); err != nil {
		t.Fatal(err)
	}
	if unread(bob) != 0 {
		t.Fatal("read marker moved backwards")
	}
	if err := f.svc.MarkRead(ctx, bob, dm.ID, uuid.New()); !errors.Is(err, ErrBadMessageID) {
		t.Fatalf("mark unknown message err = %v", err)
	}

	list, _ := f.svc.ListConversations(ctx, bob)
	if len(list) != 1 || list[0].LastMessage == nil || list[0].LastMessage.Content != "three" {
		t.Fatalf("conversation list = %+v", list)
	}
}

func TestGroups(t *testing.T) {
	f := setup(t, "alice", "bob", "carol", "dave", "stranger")
	ctx := context.Background()
	for _, n := range []string{"bob", "carol", "dave"} {
		f.befriend(t, "alice", n)
	}
	alice := f.users["alice"]
	ids := func(names ...string) []uuid.UUID {
		var out []uuid.UUID
		for _, n := range names {
			out = append(out, f.users[n])
		}
		return out
	}

	if _, err := f.svc.CreateGroup(ctx, alice, ids("bob"), ""); !errors.Is(err, ErrGroupSize) {
		t.Fatalf("group of 2 err = %v", err)
	}
	if _, err := f.svc.CreateGroup(ctx, alice, ids("bob", "alice", "bob"), ""); !errors.Is(err, ErrGroupSize) {
		t.Fatalf("duplicates and self must not count, err = %v", err)
	}
	if _, err := f.svc.CreateGroup(ctx, alice, ids("bob", "stranger"), ""); !errors.Is(err, ErrNotFriends) {
		t.Fatalf("group with non-friend err = %v", err)
	}
	if _, err := f.svc.CreateGroup(ctx, alice, ids("bob", "carol"), strings.Repeat("x", 65)); !errors.Is(err, ErrGroupName) {
		t.Fatalf("long name err = %v", err)
	}
	g, err := f.svc.CreateGroup(ctx, alice, ids("bob", "carol", "dave"), " Weekend plans ")
	if err != nil {
		t.Fatal(err)
	}
	if g.Kind != "group" || g.Name != "Weekend plans" || len(g.Members) != 4 {
		t.Fatalf("group = %+v", g)
	}
	// Members don't have to be friends with each other.
	f.send(t, "carol", g.ID, "hi all")
	if c, _ := f.svc.GetConversation(ctx, f.users["dave"], g.ID); c.UnreadCount != 1 {
		t.Fatalf("dave unread = %d", c.UnreadCount)
	}
	if _, err := f.svc.GetConversation(ctx, f.users["stranger"], g.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("non-member get err = %v", err)
	}
}

func TestMessageValidation(t *testing.T) {
	f := setup(t, "alice", "bob")
	ctx := context.Background()
	f.befriend(t, "alice", "bob")
	dm, _ := f.svc.OpenDM(ctx, f.users["alice"], f.users["bob"])
	if _, err := f.svc.SendMessage(ctx, f.users["alice"], dm.ID, " \n\t ", ""); !errors.Is(err, ErrEmptyMessage) {
		t.Fatalf("blank err = %v", err)
	}
	if _, err := f.svc.SendMessage(ctx, f.users["alice"], dm.ID, strings.Repeat("é", MaxMessageRunes+1), ""); !errors.Is(err, ErrMessageTooLong) {
		t.Fatalf("too long err = %v", err)
	}
	if _, err := f.svc.SendMessage(ctx, f.users["alice"], dm.ID, strings.Repeat("é", MaxMessageRunes), ""); err != nil {
		t.Fatalf("max length (multi-byte) rejected: %v", err)
	}
}

func TestGroupMembership(t *testing.T) {
	f := setup(t, "alice", "bob", "carol", "dave", "erin", "stranger")
	ctx := context.Background()
	for _, n := range []string{"bob", "carol", "dave", "erin"} {
		f.befriend(t, "alice", n)
	}
	f.befriend(t, "bob", "erin")
	u := f.users
	g, err := f.svc.CreateGroup(ctx, u["alice"], []uuid.UUID{u["bob"], u["carol"]}, "")
	if err != nil {
		t.Fatal(err)
	}
	last := f.send(t, "bob", g.ID, "before dave")

	// Only friends of the adder; DMs can't change members; outsiders can't add.
	if _, err := f.svc.AddMembers(ctx, u["alice"], g.ID, []uuid.UUID{u["stranger"]}); !errors.Is(err, ErrNotFriends) {
		t.Fatalf("add non-friend err = %v", err)
	}
	if _, err := f.svc.AddMembers(ctx, u["alice"], g.ID, nil); !errors.Is(err, ErrNoOneToAdd) {
		t.Fatalf("add nobody err = %v", err)
	}
	if _, err := f.svc.AddMembers(ctx, u["dave"], g.ID, []uuid.UUID{u["alice"]}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("non-member add err = %v", err)
	}
	dm, err := f.svc.OpenDM(ctx, u["alice"], u["bob"])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.AddMembers(ctx, u["alice"], dm.ID, []uuid.UUID{u["carol"]}); !errors.Is(err, ErrNotGroup) {
		t.Fatalf("add to DM err = %v", err)
	}

	before := len(f.rec.Events())
	c, err := f.svc.AddMembers(ctx, u["alice"], g.ID, []uuid.UUID{u["dave"], u["bob"]}) // bob is already in
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Members) != 4 {
		t.Fatalf("members = %d, want 4", len(c.Members))
	}
	evs := f.rec.Events()[before:]
	if len(evs) != 2 || evs[0].Type != events.ConversationUpdated || len(evs[0].Recipients) != 3 ||
		evs[1].Type != events.ConversationCreated || len(evs[1].Recipients) != 1 || evs[1].Recipients[0] != u["dave"] {
		t.Fatalf("events = %+v", evs)
	}
	// Dave sees the history but nothing unread; then new messages count.
	dc, err := f.svc.GetConversation(ctx, u["dave"], g.ID)
	if err != nil || dc.UnreadCount != 0 || dc.LastReadMessageID == nil || *dc.LastReadMessageID != last.ID {
		t.Fatalf("dave's view = %+v, %v", dc, err)
	}
	if msgs, err := f.svc.ListMessages(ctx, u["dave"], g.ID, nil, nil, 10); err != nil || len(msgs) != 1 {
		t.Fatalf("dave's history = %d, %v", len(msgs), err)
	}
	f.send(t, "carol", g.ID, "hi dave")
	if dc, _ := f.svc.GetConversation(ctx, u["dave"], g.ID); dc.UnreadCount != 1 {
		t.Fatalf("dave unread = %d", dc.UnreadCount)
	}
	// Any member can add their own friends.
	if _, err := f.svc.AddMembers(ctx, u["bob"], g.ID, []uuid.UUID{u["erin"]}); err != nil {
		t.Fatal(err)
	}

	// Leaving: DMs can't be left; the leaver loses access; the last one out deletes it.
	if err := f.svc.LeaveGroup(ctx, u["alice"], dm.ID); !errors.Is(err, ErrNotGroup) {
		t.Fatalf("leave DM err = %v", err)
	}
	before = len(f.rec.Events())
	if err := f.svc.LeaveGroup(ctx, u["dave"], g.ID); err != nil {
		t.Fatal(err)
	}
	evs = f.rec.Events()[before:]
	if len(evs) != 2 || evs[0].Type != events.ConversationRemoved || evs[0].Recipients[0] != u["dave"] ||
		evs[1].Type != events.ConversationUpdated || len(evs[1].Recipients) != 4 {
		t.Fatalf("leave events = %+v", evs)
	}
	if _, err := f.svc.GetConversation(ctx, u["dave"], g.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("dave after leaving err = %v", err)
	}
	if err := f.svc.LeaveGroup(ctx, u["dave"], g.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("leave twice err = %v", err)
	}
	for _, n := range []string{"alice", "bob", "carol", "erin"} {
		if err := f.svc.LeaveGroup(ctx, u[n], g.ID); err != nil {
			t.Fatalf("%s leave: %v", n, err)
		}
	}
	if _, err := f.svc.ListMessages(ctx, u["alice"], g.ID, nil, nil, 10); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted group err = %v", err)
	}
}

func TestGroupSizeLimitOnAdd(t *testing.T) {
	names := []string{"owner"}
	for i := range MaxGroupMembers + 1 {
		names = append(names, fmt.Sprintf("friend%d", i))
	}
	f := setup(t, names...)
	ctx := context.Background()
	var ids []uuid.UUID
	for _, n := range names[1:] {
		f.befriend(t, "owner", n)
		ids = append(ids, f.users[n])
	}
	g, err := f.svc.CreateGroup(ctx, f.users["owner"], ids[:MaxGroupMembers-2], "") // one slot left
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.AddMembers(ctx, f.users["owner"], g.ID, ids[MaxGroupMembers-2:]); !errors.Is(err, ErrGroupFull) {
		t.Fatalf("overfill err = %v", err)
	}
	// Concurrent adds of the last slot: exactly one wins.
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = f.svc.AddMembers(ctx, f.users["owner"], g.ID, []uuid.UUID{ids[MaxGroupMembers-2+i]})
		}()
	}
	wg.Wait()
	if (errs[0] == nil) == (errs[1] == nil) {
		t.Fatalf("concurrent adds: %v / %v, want exactly one success", errs[0], errs[1])
	}
	if c, _ := f.svc.GetConversation(ctx, f.users["owner"], g.ID); len(c.Members) != MaxGroupMembers {
		t.Fatalf("members = %d, want %d", len(c.Members), MaxGroupMembers)
	}
}
