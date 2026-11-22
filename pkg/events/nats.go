package events

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
)

// UserSubject is the NATS subject carrying events for one user. The realtime
// service subscribes to it while that user has a connection open.
func UserSubject(user uuid.UUID) string { return "parley.user." + user.String() }

// Wire is an event as delivered to clients.
type Wire struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

// NATS publishes each event to every recipient's subject. Core NATS (not
// JetStream) is enough: clients that miss events while disconnected catch up
// from the REST API when they reconnect.
type NATS struct {
	Conn *nats.Conn
}

func (n NATS) Publish(ctx context.Context, e Event) {
	data, err := json.Marshal(e.Data)
	if err != nil {
		slog.ErrorContext(ctx, "encode event", "type", e.Type, "err", err)
		return
	}
	payload, _ := json.Marshal(Wire{Type: e.Type, Data: data})
	for _, r := range e.Recipients {
		if err := n.Conn.Publish(UserSubject(r), payload); err != nil {
			slog.ErrorContext(ctx, "publish event", "type", e.Type, "err", err)
		}
	}
}
