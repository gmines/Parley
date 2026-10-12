// Package delivery sends short transactional messages (verification and
// password-reset codes) by email or SMS.
package delivery

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
)

// Message is a plain-text notification. Subject is ignored for SMS.
type Message struct {
	To      string
	Subject string
	Body    string
}

type Sender interface {
	Send(ctx context.Context, m Message) error
}

var errHeaderInjection = errors.New("delivery: line break in address or subject")

func checkHeaders(m Message) error {
	if strings.ContainsAny(m.To, "\r\n") || strings.ContainsAny(m.Subject, "\r\n") {
		return errHeaderInjection
	}
	return nil
}

// Log writes messages, including their codes, to the server log instead of
// sending them. For development and testing only: anyone who can read the
// logs can take over any account.
type Log struct {
	Channel string
}

func (l Log) Send(ctx context.Context, m Message) error {
	slog.WarnContext(ctx, "DEV DELIVERY (not sent)", "channel", l.Channel, "to", m.To, "subject", m.Subject, "body", m.Body)
	return nil
}

// Recorder keeps messages in memory, for tests.
type Recorder struct {
	mu       sync.Mutex
	messages []Message
}

func (r *Recorder) Send(_ context.Context, m Message) error {
	if err := checkHeaders(m); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.messages = append(r.messages, m)
	return nil
}

func (r *Recorder) Messages() []Message {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Message(nil), r.messages...)
}

// Last returns the most recent message sent to `to`.
func (r *Recorder) Last(to string) (Message, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := len(r.messages) - 1; i >= 0; i-- {
		if r.messages[i].To == to {
			return r.messages[i], true
		}
	}
	return Message{}, false
}
