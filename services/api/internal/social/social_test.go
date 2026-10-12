package social

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"parley/pkg/db/dbtest"
	"parley/pkg/events"
	"parley/services/api/migrations"
)

type fixture struct {
	svc   *Service
	rec   *events.Recorder
	pool  *pgxpool.Pool
	users map[string]uuid.UUID
}

func setup(t *testing.T, names ...string) *fixture {
	t.Helper()
	pool := dbtest.New(t, migrations.FS)
	rec := &events.Recorder{}
	f := &fixture{svc: NewService(pool, rec), rec: rec, pool: pool, users: map[string]uuid.UUID{}}
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

func (f *fixture) friends(t *testing.T, a, b string) bool {
	t.Helper()
	ok, err := f.svc.AreFriends(context.Background(), f.users[a], f.users[b])
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

func (f *fixture) befriend(t *testing.T, a, b string) {
	t.Helper()
	ctx := context.Background()
	res, err := f.svc.SendRequest(ctx, f.users[a], b)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.AcceptRequest(ctx, f.users[b], res.Request.ID); err != nil {
		t.Fatal(err)
	}
}

func TestRequestAcceptRemove(t *testing.T) {
	f := setup(t, "alice", "bob")
	ctx := context.Background()
	alice, bob := f.users["alice"], f.users["bob"]

	res, err := f.svc.SendRequest(ctx, alice, "BOB")
	if err != nil {
		t.Fatal(err)
	}
	if res.Request == nil || res.Request.To.ID != bob {
		t.Fatalf("result = %+v, want pending request to bob", res)
	}
	again, err := f.svc.SendRequest(ctx, alice, "bob")
	if err != nil || again.Request.ID != res.Request.ID {
		t.Fatalf("re-send: id %v err %v, want idempotent", again.Request, err)
	}

	in, out, err := f.svc.ListRequests(ctx, bob)
	if err != nil || len(in) != 1 || len(out) != 0 {
		t.Fatalf("bob's requests: in=%d out=%d err=%v", len(in), len(out), err)
	}

	if _, err := f.svc.AcceptRequest(ctx, alice, res.Request.ID); !errors.Is(err, ErrRequestNotFound) {
		t.Fatalf("sender accepting own request err = %v", err)
	}
	fr, err := f.svc.AcceptRequest(ctx, bob, res.Request.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fr.User.ID != alice {
		t.Fatalf("accepted friend = %v, want alice", fr.User)
	}
	if !f.friends(t, "alice", "bob") {
		t.Fatal("not friends after accept")
	}
	if _, err := f.svc.SendRequest(ctx, alice, "bob"); !errors.Is(err, ErrAlreadyFriends) {
		t.Fatalf("request to friend err = %v", err)
	}

	list, err := f.svc.ListFriends(ctx, bob)
	if err != nil || len(list) != 1 || list[0].User.ID != alice {
		t.Fatalf("bob's friends = %+v err %v", list, err)
	}

	if err := f.svc.RemoveFriend(ctx, bob, alice); err != nil {
		t.Fatal(err)
	}
	if f.friends(t, "alice", "bob") {
		t.Fatal("still friends after remove")
	}
	if err := f.svc.RemoveFriend(ctx, bob, alice); !errors.Is(err, ErrNotFriends) {
		t.Fatalf("second remove err = %v", err)
	}

	types := map[string]int{}
	for _, e := range f.rec.Events() {
		types[e.Type]++
	}
	want := map[string]int{
		events.FriendRequestCreated: 1, // the idempotent re-send doesn't re-announce
		events.FriendRequestRemoved: 1,
		events.FriendshipCreated:    2, // one per side
		events.FriendshipRemoved:    2,
	}
	for k, v := range want {
		if types[k] != v {
			t.Errorf("%s events = %d, want %d (all: %v)", k, types[k], v, types)
		}
	}
}

func TestMutualRequestsBecomeFriends(t *testing.T) {
	f := setup(t, "alice", "bob")
	ctx := context.Background()
	if _, err := f.svc.SendRequest(ctx, f.users["alice"], "bob"); err != nil {
		t.Fatal(err)
	}
	res, err := f.svc.SendRequest(ctx, f.users["bob"], "alice")
	if err != nil {
		t.Fatal(err)
	}
	if res.Friendship == nil {
		t.Fatalf("result = %+v, want friendship", res)
	}
	in, out, _ := f.svc.ListRequests(ctx, f.users["alice"])
	if len(in)+len(out) != 0 {
		t.Fatal("pending requests remain after mutual add")
	}
}

func TestConcurrentMutualRequests(t *testing.T) {
	f := setup(t, "alice", "bob")
	ctx := context.Background()
	for i := 0; i < 10; i++ {
		var wg sync.WaitGroup
		errs := make([]error, 2)
		wg.Add(2)
		go func() { defer wg.Done(); _, errs[0] = f.svc.SendRequest(ctx, f.users["alice"], "bob") }()
		go func() { defer wg.Done(); _, errs[1] = f.svc.SendRequest(ctx, f.users["bob"], "alice") }()
		wg.Wait()
		for _, err := range errs {
			if err != nil {
				t.Fatal(err)
			}
		}
		if !f.friends(t, "alice", "bob") {
			t.Fatalf("round %d: simultaneous requests did not produce a friendship", i)
		}
		if err := f.svc.RemoveFriend(ctx, f.users["alice"], f.users["bob"]); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDeclineAndCancel(t *testing.T) {
	f := setup(t, "alice", "bob", "eve")
	ctx := context.Background()
	r1, _ := f.svc.SendRequest(ctx, f.users["alice"], "bob")
	r2, _ := f.svc.SendRequest(ctx, f.users["alice"], "eve")

	if err := f.svc.DeleteRequest(ctx, f.users["eve"], r1.Request.ID); !errors.Is(err, ErrRequestNotFound) {
		t.Fatalf("third party deleting request err = %v", err)
	}
	if err := f.svc.DeleteRequest(ctx, f.users["bob"], r1.Request.ID); err != nil {
		t.Fatalf("decline: %v", err)
	}
	if err := f.svc.DeleteRequest(ctx, f.users["alice"], r2.Request.ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	_, out, _ := f.svc.ListRequests(ctx, f.users["alice"])
	if len(out) != 0 {
		t.Fatalf("outgoing = %d, want 0", len(out))
	}
}

func TestBlock(t *testing.T) {
	f := setup(t, "alice", "bob", "carol")
	ctx := context.Background()
	alice, bob, carol := f.users["alice"], f.users["bob"], f.users["carol"]

	f.befriend(t, "alice", "bob")
	if _, err := f.svc.SendRequest(ctx, carol, "alice"); err != nil {
		t.Fatal(err)
	}

	// Blocking ends the friendship and is idempotent.
	if err := f.svc.Block(ctx, alice, bob); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.Block(ctx, alice, bob); err != nil {
		t.Fatalf("second block: %v", err)
	}
	if f.friends(t, "alice", "bob") {
		t.Fatal("friendship survived block")
	}
	if ok, _ := f.svc.CanInteract(ctx, bob, alice); ok {
		t.Fatal("CanInteract true after block")
	}

	// The blocked user gets the same error as for a nonexistent username.
	_, errBlocked := f.svc.SendRequest(ctx, bob, "alice")
	_, errMissing := f.svc.SendRequest(ctx, bob, "nobody")
	if !errors.Is(errBlocked, ErrCannotSend) || !errors.Is(errMissing, ErrCannotSend) {
		t.Fatalf("blocked err = %v, missing err = %v; both want ErrCannotSend", errBlocked, errMissing)
	}
	// The blocker is told why.
	if _, err := f.svc.SendRequest(ctx, alice, "bob"); !errors.Is(err, ErrYouBlocked) {
		t.Fatalf("blocker sending err = %v", err)
	}

	// Blocking clears pending requests in both directions.
	in, _, _ := f.svc.ListRequests(ctx, alice)
	if len(in) != 1 {
		t.Fatalf("alice incoming = %d, want carol's request", len(in))
	}
	if err := f.svc.Block(ctx, alice, carol); err != nil {
		t.Fatal(err)
	}
	in, _, _ = f.svc.ListRequests(ctx, alice)
	if len(in) != 0 {
		t.Fatal("pending request survived block")
	}

	blocked, err := f.svc.ListBlocked(ctx, alice)
	if err != nil || len(blocked) != 2 {
		t.Fatalf("blocked list = %+v err %v", blocked, err)
	}

	if err := f.svc.Unblock(ctx, alice, bob); err != nil {
		t.Fatal(err)
	}
	if ok, _ := f.svc.CanInteract(ctx, alice, bob); !ok {
		t.Fatal("CanInteract false after unblock")
	}
	if f.friends(t, "alice", "bob") {
		t.Fatal("unblock must not restore the friendship")
	}

	if err := f.svc.Block(ctx, alice, alice); !errors.Is(err, ErrSelf) {
		t.Fatalf("self block err = %v", err)
	}
	if err := f.svc.Block(ctx, alice, uuid.New()); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("block unknown err = %v", err)
	}
}
