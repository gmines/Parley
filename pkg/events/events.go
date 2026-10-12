// Package events defines the domain events services publish for realtime
// fan-out. Milestone 2 adds a NATS JetStream publisher; until then services use
// Discard or Recorder.
package events

import (
	"context"
	"sync"

	"github.com/google/uuid"
)

// Event is delivered to every user in Recipients over their realtime connection.
type Event struct {
	Type       string      `json:"type"`
	Recipients []uuid.UUID `json:"-"`
	Data       any         `json:"data"`
}

// Event types. Keep in sync with api/events.md.
const (
	FriendRequestCreated = "friend_request.created"
	FriendRequestRemoved = "friend_request.removed"
	FriendshipCreated    = "friendship.created"
	FriendshipRemoved    = "friendship.removed"
	ConversationCreated  = "conversation.created"
	ConversationRead     = "conversation.read"
	MessageCreated       = "message.created"
)

type Publisher interface {
	// Publish must not fail the caller's operation: the state change has already
	// been committed. Implementations log and retry internally.
	Publish(ctx context.Context, e Event)
}

// Discard drops every event.
type Discard struct{}

func (Discard) Publish(context.Context, Event) {}

// Recorder keeps published events in memory, for tests.
type Recorder struct {
	mu     sync.Mutex
	events []Event
}

func (r *Recorder) Publish(_ context.Context, e Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

func (r *Recorder) Events() []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Event(nil), r.events...)
}
