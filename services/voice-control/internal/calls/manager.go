// Package calls runs Parley's voice calls. Every DM and group has one call
// room: an Asterisk mixing bridge. Clients join by dialing the room through
// Kamailio with credentials from Join; Asterisk hands each call to this
// package over ARI (Stasis), which re-checks who's calling, answers, and puts
// the channel into the room's bridge. Rooms disappear when the last person
// leaves. Membership changes are announced to conversation members as
// realtime events.
package calls

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"parley/pkg/events"
	"parley/services/voice-control/internal/ari"
)

const (
	StasisApp     = "parley"
	bridgePrefix  = "parley-"
	userVariable  = "PARLEY_USER" // set on each joined channel, read back on resync
	actionTimeout = 10 * time.Second
)

var (
	ErrNotMember  = errors.New("conversation not found")
	ErrCannotCall = errors.New("you can't call this user")
)

// ARI is the subset of the Asterisk REST Interface the manager uses.
type ARI interface {
	Answer(ctx context.Context, channel string) error
	Hangup(ctx context.Context, channel string) error
	SetVar(ctx context.Context, channel, name, value string) error
	GetVar(ctx context.Context, channel, name string) (string, error)
	CreateBridge(ctx context.Context, id, name string) error
	AddChannel(ctx context.Context, bridge, channel string) error
	DestroyBridge(ctx context.Context, bridge string) error
	ListBridges(ctx context.Context) ([]ari.Bridge, error)
}

// Directory answers who may call whom, from the conversations data.
type Directory interface {
	// Members returns the conversation's kind ("dm" or "group") and members,
	// or ErrNotMember if user doesn't belong to it.
	Members(ctx context.Context, conv, user uuid.UUID) (kind string, members []uuid.UUID, err error)
	// Blocked reports whether either user blocked the other.
	Blocked(ctx context.Context, a, b uuid.UUID) (bool, error)
}

// Call is the public state of a room.
type Call struct {
	ConversationID uuid.UUID   `json:"conversation_id"`
	StartedBy      uuid.UUID   `json:"started_by"`
	StartedAt      time.Time   `json:"started_at"`
	Participants   []uuid.UUID `json:"participants"`
}

type room struct {
	conv      uuid.UUID
	startedBy uuid.UUID
	startedAt time.Time
	channels  map[string]uuid.UUID // ARI channel id -> user
}

func (r *room) call() Call {
	seen := map[uuid.UUID]bool{}
	var ps []uuid.UUID
	for _, u := range r.channels {
		if !seen[u] {
			seen[u] = true
			ps = append(ps, u)
		}
	}
	slices.SortFunc(ps, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
	return Call{ConversationID: r.conv, StartedBy: r.startedBy, StartedAt: r.startedAt, Participants: ps}
}

type Manager struct {
	ari ARI
	dir Directory
	pub events.Publisher
	now func() time.Time

	mu    sync.Mutex
	rooms map[uuid.UUID]*room
}

func NewManager(a ARI, dir Directory, pub events.Publisher) *Manager {
	return &Manager{ari: a, dir: dir, pub: pub, now: time.Now, rooms: map[uuid.UUID]*room{}}
}

// Authorize checks that user may be in conv's call: they're a member and, in
// a DM, neither person has blocked the other. It returns the members.
func (m *Manager) Authorize(ctx context.Context, user, conv uuid.UUID) ([]uuid.UUID, error) {
	kind, members, err := m.dir.Members(ctx, conv, user)
	if err != nil {
		return nil, err
	}
	if kind == "dm" {
		for _, other := range members {
			if other == user {
				continue
			}
			blocked, err := m.dir.Blocked(ctx, user, other)
			if err != nil {
				return nil, err
			}
			if blocked {
				return nil, ErrCannotCall
			}
		}
	}
	return members, nil
}

// Active returns the calls in progress in conversations user belongs to.
func (m *Manager) Active(ctx context.Context, user uuid.UUID) []Call {
	m.mu.Lock()
	var candidates []Call
	for _, r := range m.rooms {
		candidates = append(candidates, r.call())
	}
	m.mu.Unlock()

	out := []Call{}
	for _, c := range candidates {
		if _, _, err := m.dir.Members(ctx, c.ConversationID, user); err == nil {
			out = append(out, c)
		}
	}
	return out
}

// Get returns conv's call, if one is in progress.
func (m *Manager) Get(conv uuid.UUID) (Call, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.rooms[conv]
	if r == nil {
		return Call{}, false
	}
	return r.call(), true
}

// Decline tells the other members that user won't join (their clients can
// stop ringing, and a DM caller sees it was declined).
func (m *Manager) Decline(ctx context.Context, user, conv uuid.UUID) error {
	members, err := m.Authorize(ctx, user, conv)
	if err != nil {
		return err
	}
	m.pub.Publish(ctx, events.Event{
		Type:       events.CallDeclined,
		Recipients: members,
		Data:       map[string]uuid.UUID{"conversation_id": conv, "user_id": user},
	})
	return nil
}

// HandleEvent processes one ARI event.
func (m *Manager) HandleEvent(e ari.Event) {
	if e.Channel == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), actionTimeout)
	defer cancel()
	switch e.Type {
	case "StasisStart":
		m.join(ctx, e.Channel.ID, e.Args)
	case "StasisEnd":
		m.leave(ctx, e.Channel.ID)
	}
}

// join admits a channel that entered the Stasis app. Args are the room
// (conversation id) and the user Kamailio authenticated.
func (m *Manager) join(ctx context.Context, channel string, args []string) {
	reject := func(reason string, err error) {
		slog.Warn("rejecting call", "channel", channel, "reason", reason, "err", err)
		m.ari.Hangup(ctx, channel)
	}
	if len(args) < 2 {
		reject("missing room or user", nil)
		return
	}
	conv, err1 := uuid.Parse(args[0])
	user, err2 := uuid.Parse(args[1])
	if err1 != nil || err2 != nil {
		reject("bad room or user", errors.Join(err1, err2))
		return
	}
	// Credentials were only issued to authorized users, but membership or a
	// block may have changed since: check again at the door.
	members, err := m.Authorize(ctx, user, conv)
	if err != nil {
		reject("not allowed", err)
		return
	}

	bridge := bridgePrefix + conv.String()
	if err := m.ari.Answer(ctx, channel); err != nil {
		reject("answer failed", err)
		return
	}
	if err := m.ari.SetVar(ctx, channel, userVariable, user.String()); err != nil {
		reject("set user variable failed", err)
		return
	}
	// The bridge already exists once the first person is in: that's fine.
	if err := m.ari.CreateBridge(ctx, bridge, "call "+conv.String()); err != nil && !errors.Is(err, ari.ErrConflict) {
		reject("create bridge failed", err)
		return
	}
	if err := m.ari.AddChannel(ctx, bridge, channel); err != nil {
		reject("add to bridge failed", err)
		return
	}

	m.mu.Lock()
	r := m.rooms[conv]
	started := r == nil
	if started {
		r = &room{conv: conv, startedBy: user, startedAt: m.now(), channels: map[string]uuid.UUID{}}
		m.rooms[conv] = r
	}
	r.channels[channel] = user
	call := r.call()
	m.mu.Unlock()

	slog.Info("joined call", "conversation", conv, "user", user, "participants", len(call.Participants))
	if started {
		m.pub.Publish(ctx, events.Event{Type: events.CallStarted, Recipients: members, Data: call})
	}
	m.pub.Publish(ctx, events.Event{Type: events.CallUpdated, Recipients: members, Data: call})
}

func (m *Manager) leave(ctx context.Context, channel string) {
	m.mu.Lock()
	var r *room
	for _, candidate := range m.rooms {
		if _, ok := candidate.channels[channel]; ok {
			r = candidate
			break
		}
	}
	if r == nil {
		m.mu.Unlock()
		return // a rejected or never-admitted channel
	}
	user := r.channels[channel]
	delete(r.channels, channel)
	ended := len(r.channels) == 0
	if ended {
		delete(m.rooms, r.conv)
	}
	call := r.call()
	m.mu.Unlock()

	slog.Info("left call", "conversation", r.conv, "user", user, "participants", len(call.Participants))
	recipients := m.recipients(ctx, r.conv, user)
	if ended {
		if err := m.ari.DestroyBridge(ctx, bridgePrefix+r.conv.String()); err != nil && !errors.Is(err, ari.ErrNotFound) {
			slog.Warn("destroy bridge", "conversation", r.conv, "err", err)
		}
		m.pub.Publish(ctx, events.Event{Type: events.CallEnded, Recipients: recipients, Data: map[string]uuid.UUID{"conversation_id": r.conv}})
		return
	}
	m.pub.Publish(ctx, events.Event{Type: events.CallUpdated, Recipients: recipients, Data: call})
}

// recipients returns conv's members for an announcement on behalf of user
// (whose membership may be gone by now, e.g. they left a group).
func (m *Manager) recipients(ctx context.Context, conv, user uuid.UUID) []uuid.UUID {
	if _, members, err := m.dir.Members(ctx, conv, user); err == nil {
		return members
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if r := m.rooms[conv]; r != nil {
		return r.call().Participants
	}
	return []uuid.UUID{user}
}

// Resync rebuilds room state from Asterisk after (re)connecting to ARI, so a
// voice-control restart doesn't drop calls in progress. Empty leftover
// bridges are destroyed.
func (m *Manager) Resync(ctx context.Context) {
	bridges, err := m.ari.ListBridges(ctx)
	if err != nil {
		slog.Error("resync: list bridges", "err", err)
		return
	}
	rooms := map[uuid.UUID]*room{}
	for _, b := range bridges {
		conv, err := uuid.Parse(strings.TrimPrefix(b.ID, bridgePrefix))
		if !strings.HasPrefix(b.ID, bridgePrefix) || err != nil {
			continue
		}
		r := &room{conv: conv, startedAt: m.now(), channels: map[string]uuid.UUID{}}
		for _, ch := range b.Channels {
			v, err := m.ari.GetVar(ctx, ch, userVariable)
			if u, perr := uuid.Parse(v); err == nil && perr == nil {
				r.channels[ch] = u
				if r.startedBy == uuid.Nil {
					r.startedBy = u
				}
			}
		}
		if len(r.channels) == 0 {
			m.ari.DestroyBridge(ctx, b.ID)
			continue
		}
		rooms[conv] = r
	}
	m.mu.Lock()
	// Keep start times we already knew.
	for conv, r := range rooms {
		if old := m.rooms[conv]; old != nil {
			r.startedAt, r.startedBy = old.startedAt, old.startedBy
		}
	}
	m.rooms = rooms
	m.mu.Unlock()
	slog.Info("resynced calls from asterisk", "rooms", len(rooms))
}
