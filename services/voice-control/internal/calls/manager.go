// Package calls runs Parley's voice calls. Every DM and group has one call
// room, a conference on the media server (FreeSWITCH). Clients join by
// dialing the room through Kamailio with credentials from the REST API; the
// media server parks each arriving call and reports it, this package
// re-checks who's calling and admits them into the room (or hangs up).
// Rooms disappear when the last person leaves. Membership changes are
// announced to the conversation's members as realtime events.
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
)

const actionTimeout = 10 * time.Second

var (
	ErrNotMember  = errors.New("conversation not found")
	ErrCannotCall = errors.New("you can't call this user")
)

// Member is one call leg in a room.
type Member struct {
	CallID string
	User   uuid.UUID
}

// Media is the media server, as seen by the manager.
type Media interface {
	// Admit moves a waiting (parked) call into the room.
	Admit(ctx context.Context, callID string, room uuid.UUID) error
	// Reject hangs a call up.
	Reject(ctx context.Context, callID string) error
	// Rooms lists the rooms in progress and their members, for resync.
	Rooms(ctx context.Context) (map[uuid.UUID][]Member, error)
}

// EventHandler receives call events from the media server.
type EventHandler interface {
	// CallArrived: a call is waiting to be admitted. room and user are as
	// Kamailio authenticated them (still strings: unverified input).
	CallArrived(callID, room, user string)
	// CallEnded: a call hung up (admitted or not).
	CallEnded(callID string)
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
	legs      map[string]uuid.UUID // call id -> user
}

func (r *room) call() Call {
	seen := map[uuid.UUID]bool{}
	var ps []uuid.UUID
	for _, u := range r.legs {
		if !seen[u] {
			seen[u] = true
			ps = append(ps, u)
		}
	}
	slices.SortFunc(ps, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
	return Call{ConversationID: r.conv, StartedBy: r.startedBy, StartedAt: r.startedAt, Participants: ps}
}

type Manager struct {
	media Media
	dir   Directory
	pub   events.Publisher
	now   func() time.Time

	mu    sync.Mutex
	rooms map[uuid.UUID]*room
}

func NewManager(media Media, dir Directory, pub events.Publisher) *Manager {
	return &Manager{media: media, dir: dir, pub: pub, now: time.Now, rooms: map[uuid.UUID]*room{}}
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

// Decline reasons: the user chose not to join, or tried to and couldn't
// (e.g. their app has no working audio device).
const (
	DeclineDeclined = "declined"
	DeclineFailed   = "failed"
)

// ErrBadReason is returned for a decline reason other than the above.
var ErrBadReason = errors.New("unknown decline reason")

// Decline tells the other members that user won't join (their clients can
// stop ringing, and a DM caller sees it was declined or failed). An empty
// reason means DeclineDeclined.
func (m *Manager) Decline(ctx context.Context, user, conv uuid.UUID, reason string) error {
	switch reason {
	case "":
		reason = DeclineDeclined
	case DeclineDeclined, DeclineFailed:
	default:
		return ErrBadReason
	}
	members, err := m.Authorize(ctx, user, conv)
	if err != nil {
		return err
	}
	m.pub.Publish(ctx, events.Event{
		Type:       events.CallDeclined,
		Recipients: members,
		Data:       map[string]any{"conversation_id": conv, "user_id": user, "reason": reason},
	})
	return nil
}

// CallArrived admits a waiting call into its room after re-checking the
// caller, or hangs it up.
func (m *Manager) CallArrived(callID, roomArg, userArg string) {
	ctx, cancel := context.WithTimeout(context.Background(), actionTimeout)
	defer cancel()
	reject := func(reason string, err error) {
		slog.Warn("rejecting call", "call", callID, "reason", reason, "err", err)
		m.media.Reject(ctx, callID)
	}
	conv, err1 := uuid.Parse(roomArg)
	user, err2 := uuid.Parse(userArg)
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
	if err := m.media.Admit(ctx, callID, conv); err != nil {
		reject("admit failed", err)
		return
	}

	m.mu.Lock()
	r := m.rooms[conv]
	started := r == nil
	if started {
		r = &room{conv: conv, startedBy: user, startedAt: m.now(), legs: map[string]uuid.UUID{}}
		m.rooms[conv] = r
	}
	r.legs[callID] = user
	call := r.call()
	m.mu.Unlock()

	slog.Info("joined call", "conversation", conv, "user", user, "participants", len(call.Participants))
	if started {
		m.pub.Publish(ctx, events.Event{Type: events.CallStarted, Recipients: members, Data: call})
	}
	m.pub.Publish(ctx, events.Event{Type: events.CallUpdated, Recipients: members, Data: call})
}

// CallEnded removes a hung-up call from its room; the room ends when empty.
// (The media server tears the conference down by itself.)
func (m *Manager) CallEnded(callID string) {
	ctx, cancel := context.WithTimeout(context.Background(), actionTimeout)
	defer cancel()
	m.mu.Lock()
	var r *room
	for _, candidate := range m.rooms {
		if _, ok := candidate.legs[callID]; ok {
			r = candidate
			break
		}
	}
	if r == nil {
		m.mu.Unlock()
		return // a rejected or never-admitted call
	}
	user := r.legs[callID]
	delete(r.legs, callID)
	ended := len(r.legs) == 0
	if ended {
		delete(m.rooms, r.conv)
	}
	call := r.call()
	m.mu.Unlock()

	slog.Info("left call", "conversation", r.conv, "user", user, "participants", len(call.Participants))
	recipients := m.recipients(ctx, r.conv, user)
	if ended {
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

// Resync rebuilds room state from the media server after (re)connecting, so
// a voice-control restart doesn't lose track of calls in progress.
func (m *Manager) Resync(ctx context.Context) {
	current, err := m.media.Rooms(ctx)
	if err != nil {
		slog.Error("resync: list rooms", "err", err)
		return
	}
	rooms := map[uuid.UUID]*room{}
	for conv, members := range current {
		if len(members) == 0 {
			continue
		}
		r := &room{conv: conv, startedBy: members[0].User, startedAt: m.now(), legs: map[string]uuid.UUID{}}
		for _, mb := range members {
			r.legs[mb.CallID] = mb.User
		}
		rooms[conv] = r
	}
	m.mu.Lock()
	for conv, r := range rooms { // keep start info we already knew
		if old := m.rooms[conv]; old != nil {
			r.startedAt, r.startedBy = old.startedAt, old.startedBy
		}
	}
	m.rooms = rooms
	m.mu.Unlock()
	slog.Info("resynced calls from media server", "rooms", len(rooms))
}
