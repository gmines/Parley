// Package messaging owns conversations (1:1 DMs and small group DMs) and
// their messages. New messages are announced through events.Publisher; the
// realtime service delivers them to connected clients.
package messaging

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"parley/pkg/db"
	"parley/pkg/events"
	"parley/services/api/internal/social"
)

const (
	MaxMessageRunes = 4000
	MaxGroupMembers = 10 // including the creator
	maxGroupName    = 64
	maxNonce        = 64
	defaultPage     = 50
	maxPage         = 100
	unreadCap       = 100 // unread counts stop at this ("99+" in the UI)
)

var (
	ErrNotFound       = errors.New("conversation not found")
	ErrNotFriends     = errors.New("you can only start conversations with friends")
	ErrCannotMessage  = errors.New("you can't send messages to this user")
	ErrSelf           = errors.New("you can't start a conversation with yourself")
	ErrEmptyMessage   = errors.New("message is empty")
	ErrMessageTooLong = fmt.Errorf("message is longer than %d characters", MaxMessageRunes)
	ErrGroupSize      = fmt.Errorf("a group needs 2 to %d other people", MaxGroupMembers-1)
	ErrGroupName      = fmt.Errorf("group name must be at most %d printable characters", maxGroupName)
	ErrBadMessageID   = errors.New("message not found in this conversation")
)

// Graph is the part of the social module messaging depends on.
type Graph interface {
	AreFriends(ctx context.Context, a, b uuid.UUID) (bool, error)
	CanInteract(ctx context.Context, a, b uuid.UUID) (bool, error)
}

type Message struct {
	ID             uuid.UUID       `json:"id"`
	ConversationID uuid.UUID       `json:"conversation_id"`
	Author         *social.UserRef `json:"author"` // nil if the account was deleted
	Content        string          `json:"content"`
	CreatedAt      time.Time       `json:"created_at"`
	// Nonce echoes the sender's client-generated id so it can match the
	// realtime event to its optimistic copy. Not stored.
	Nonce string `json:"nonce,omitempty"`
}

type Conversation struct {
	ID                uuid.UUID        `json:"id"`
	Kind              string           `json:"kind"` // "dm" or "group"
	Name              string           `json:"name,omitempty"`
	Members           []social.UserRef `json:"members"`
	CreatedAt         time.Time        `json:"created_at"`
	LastMessage       *Message         `json:"last_message"`
	LastReadMessageID *uuid.UUID       `json:"last_read_message_id"`
	UnreadCount       int              `json:"unread_count"`
}

type Service struct {
	pool   *pgxpool.Pool
	graph  Graph
	events events.Publisher
}

func NewService(pool *pgxpool.Pool, graph Graph, pub events.Publisher) *Service {
	return &Service{pool: pool, graph: graph, events: pub}
}

// Conversations -------------------------------------------------------------------

// ListConversations returns the user's conversations, most recently active first.
func (s *Service) ListConversations(ctx context.Context, user uuid.UUID) ([]Conversation, error) {
	return s.conversations(ctx, s.pool, user, nil)
}

func (s *Service) GetConversation(ctx context.Context, user, id uuid.UUID) (Conversation, error) {
	list, err := s.conversations(ctx, s.pool, user, &id)
	if err != nil {
		return Conversation{}, err
	}
	if len(list) == 0 {
		return Conversation{}, ErrNotFound
	}
	return list[0], nil
}

// OpenDM returns the DM between user and other, creating it if needed. Only
// friends can start a DM.
func (s *Service) OpenDM(ctx context.Context, user, other uuid.UUID) (Conversation, error) {
	if user == other {
		return Conversation{}, ErrSelf
	}
	friends, err := s.graph.AreFriends(ctx, user, other)
	if err != nil {
		return Conversation{}, err
	}

	var created bool
	var id uuid.UUID
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		created = false
		key := dmKey(user, other)
		err := tx.QueryRow(ctx, `SELECT id FROM conversations WHERE dm_key = $1`, key).Scan(&id)
		if err == nil {
			return nil // an existing DM stays readable even if no longer friends
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if !friends {
			return ErrNotFriends
		}
		id = uuid.Must(uuid.NewV7())
		tag, err := tx.Exec(ctx, `INSERT INTO conversations (id, kind, dm_key, created_by)
			VALUES ($1, 'dm', $2, $3) ON CONFLICT (dm_key) DO NOTHING`, id, key, user)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 { // lost a race with the other user opening it
			return tx.QueryRow(ctx, `SELECT id FROM conversations WHERE dm_key = $1`, key).Scan(&id)
		}
		created = true
		_, err = tx.Exec(ctx, `INSERT INTO conversation_members (conversation_id, user_id) VALUES ($1, $2), ($1, $3)`,
			id, user, other)
		return err
	})
	if err != nil {
		return Conversation{}, err
	}
	conv, err := s.GetConversation(ctx, user, id)
	if err == nil && created {
		s.announceConversation(ctx, conv)
	}
	return conv, err
}

// CreateGroup starts a group DM between user and 2-9 of their friends.
func (s *Service) CreateGroup(ctx context.Context, user uuid.UUID, memberIDs []uuid.UUID, name string) (Conversation, error) {
	others := slices.DeleteFunc(slices.Compact(slices.SortedFunc(slices.Values(memberIDs), uuidCompare)),
		func(id uuid.UUID) bool { return id == user })
	if len(others) < 2 || len(others) > MaxGroupMembers-1 {
		return Conversation{}, ErrGroupSize
	}
	name = strings.TrimSpace(name)
	if utf8.RuneCountInString(name) > maxGroupName || strings.IndexFunc(name, func(r rune) bool { return !unicode.IsPrint(r) }) >= 0 {
		return Conversation{}, ErrGroupName
	}
	for _, o := range others {
		ok, err := s.graph.AreFriends(ctx, user, o)
		if err != nil {
			return Conversation{}, err
		}
		if !ok {
			return Conversation{}, ErrNotFriends
		}
	}

	id := uuid.Must(uuid.NewV7())
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO conversations (id, kind, name, created_by) VALUES ($1, 'group', $2, $3)`,
			id, nullable(name), user); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO conversation_members (conversation_id, user_id)
			SELECT $1, unnest($2::uuid[])`, id, append([]uuid.UUID{user}, others...))
		return err
	})
	if err != nil {
		return Conversation{}, err
	}
	conv, err := s.GetConversation(ctx, user, id)
	if err == nil {
		s.announceConversation(ctx, conv)
	}
	return conv, err
}

// Messages ------------------------------------------------------------------------

// ListMessages returns up to limit messages in ascending order: the newest
// ones, or those strictly before/after the given message id.
func (s *Service) ListMessages(ctx context.Context, user, conv uuid.UUID, before, after *uuid.UUID, limit int) ([]Message, error) {
	if ok, err := isMember(ctx, s.pool, conv, user); err != nil || !ok {
		return nil, notFoundOr(err)
	}
	if limit <= 0 || limit > maxPage {
		limit = defaultPage
	}
	q := `SELECT m.id, m.conversation_id, m.content, m.created_at, u.id, u.username, u.display_name
		FROM messages m LEFT JOIN users u ON u.id = m.author_id WHERE m.conversation_id = $1`
	args := []any{conv, limit}
	switch {
	case after != nil:
		q += ` AND m.id > $3 ORDER BY m.id ASC LIMIT $2`
		args = append(args, *after)
	case before != nil:
		q += ` AND m.id < $3 ORDER BY m.id DESC LIMIT $2`
		args = append(args, *before)
	default:
		q += ` ORDER BY m.id DESC LIMIT $2`
	}
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	msgs, err := pgx.CollectRows(rows, scanMessage)
	if err != nil {
		return nil, err
	}
	if after == nil {
		slices.Reverse(msgs)
	}
	return msgs, nil
}

// SendMessage posts content to a conversation the user belongs to. In a DM,
// a block in either direction stops new messages.
func (s *Service) SendMessage(ctx context.Context, user, conv uuid.UUID, content, nonce string) (Message, error) {
	content = strings.TrimSpace(content)
	if content == "" {
		return Message{}, ErrEmptyMessage
	}
	if utf8.RuneCountInString(content) > MaxMessageRunes {
		return Message{}, ErrMessageTooLong
	}
	if len(nonce) > maxNonce {
		nonce = ""
	}

	var (
		msg     Message
		members []uuid.UUID
	)
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var kind string
		err := tx.QueryRow(ctx, `SELECT c.kind FROM conversations c
			JOIN conversation_members m ON m.conversation_id = c.id AND m.user_id = $2
			WHERE c.id = $1`, conv, user).Scan(&kind)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT user_id FROM conversation_members WHERE conversation_id = $1`, conv)
		if err != nil {
			return err
		}
		if members, err = pgx.CollectRows(rows, pgx.RowTo[uuid.UUID]); err != nil {
			return err
		}
		if kind == "dm" {
			for _, m := range members {
				if m == user {
					continue
				}
				ok, err := s.graph.CanInteract(ctx, user, m)
				if err != nil {
					return err
				}
				if !ok {
					return ErrCannotMessage
				}
			}
		}

		id := uuid.Must(uuid.NewV7())
		var row messageRow
		err = tx.QueryRow(ctx, `
			WITH ins AS (
				INSERT INTO messages (id, conversation_id, author_id, content) VALUES ($1, $2, $3, $4)
				RETURNING id, conversation_id, content, created_at, author_id
			)
			SELECT ins.id, ins.conversation_id, ins.content, ins.created_at, u.id, u.username, u.display_name
			FROM ins JOIN users u ON u.id = ins.author_id`, id, conv, user, content,
		).Scan(row.dest()...)
		if err != nil {
			return err
		}
		msg = row.message()
		if _, err := tx.Exec(ctx, `UPDATE conversations SET last_message_at = $2 WHERE id = $1`, conv, msg.CreatedAt); err != nil {
			return err
		}
		// Your own message counts as read.
		_, err = tx.Exec(ctx, `UPDATE conversation_members SET last_read_message_id = $3
			WHERE conversation_id = $1 AND user_id = $2`, conv, user, msg.ID)
		return err
	})
	if err != nil {
		return Message{}, err
	}
	msg.Nonce = nonce
	s.events.Publish(ctx, events.Event{Type: events.MessageCreated, Recipients: members, Data: msg})
	return msg, nil
}

// MarkRead moves the user's read marker forward to messageID (never backwards).
func (s *Service) MarkRead(ctx context.Context, user, conv, messageID uuid.UUID) error {
	if ok, err := isMember(ctx, s.pool, conv, user); err != nil || !ok {
		return notFoundOr(err)
	}
	var exists bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM messages WHERE id = $1 AND conversation_id = $2)`,
		messageID, conv).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return ErrBadMessageID
	}
	tag, err := s.pool.Exec(ctx, `UPDATE conversation_members SET last_read_message_id = $3
		WHERE conversation_id = $1 AND user_id = $2
		  AND (last_read_message_id IS NULL OR last_read_message_id < $3)`, conv, user, messageID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() > 0 {
		// Lets the user's other devices clear their unread badge.
		s.events.Publish(ctx, events.Event{
			Type:       events.ConversationRead,
			Recipients: []uuid.UUID{user},
			Data:       map[string]uuid.UUID{"conversation_id": conv, "last_read_message_id": messageID},
		})
	}
	return nil
}

// Helpers -------------------------------------------------------------------------

// conversations loads the user's conversations (or just `only`) with members,
// last message and unread count.
func (s *Service) conversations(ctx context.Context, q db.Querier, user uuid.UUID, only *uuid.UUID) ([]Conversation, error) {
	rows, err := q.Query(ctx, `
		SELECT c.id, c.kind, coalesce(c.name, ''), c.created_at, m.last_read_message_id,
		       (SELECT count(*) FROM (
		            SELECT 1 FROM messages x
		            WHERE x.conversation_id = c.id
		              AND (m.last_read_message_id IS NULL OR x.id > m.last_read_message_id)
		              AND x.author_id IS DISTINCT FROM $1
		            LIMIT $3) unread),
		       lm.id, lm.content, lm.created_at, au.id, au.username, au.display_name
		FROM conversation_members m
		JOIN conversations c ON c.id = m.conversation_id
		LEFT JOIN LATERAL (
		    SELECT id, content, created_at, author_id FROM messages
		    WHERE conversation_id = c.id ORDER BY id DESC LIMIT 1
		) lm ON true
		LEFT JOIN users au ON au.id = lm.author_id
		WHERE m.user_id = $1 AND ($2::uuid IS NULL OR c.id = $2)
		ORDER BY coalesce(c.last_message_at, c.created_at) DESC`, user, only, unreadCap)
	if err != nil {
		return nil, err
	}
	convs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Conversation, error) {
		var (
			c                 Conversation
			lmID              *uuid.UUID
			lmContent         *string
			lmAt              *time.Time
			auID              *uuid.UUID
			auName, auDisplay *string
		)
		err := r.Scan(&c.ID, &c.Kind, &c.Name, &c.CreatedAt, &c.LastReadMessageID, &c.UnreadCount,
			&lmID, &lmContent, &lmAt, &auID, &auName, &auDisplay)
		if lmID != nil {
			c.LastMessage = &Message{ID: *lmID, ConversationID: c.ID, Content: *lmContent, CreatedAt: *lmAt}
			if auID != nil {
				c.LastMessage.Author = &social.UserRef{ID: *auID, Username: *auName, DisplayName: *auDisplay}
			}
		}
		c.Members = []social.UserRef{}
		return c, err
	})
	if err != nil || len(convs) == 0 {
		return convs, err
	}

	ids := make([]uuid.UUID, len(convs))
	index := make(map[uuid.UUID]*Conversation, len(convs))
	for i := range convs {
		ids[i] = convs[i].ID
		index[convs[i].ID] = &convs[i]
	}
	rows, err = q.Query(ctx, `
		SELECT cm.conversation_id, u.id, u.username, u.display_name
		FROM conversation_members cm JOIN users u ON u.id = cm.user_id
		WHERE cm.conversation_id = ANY($1)
		ORDER BY u.username`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			cid uuid.UUID
			u   social.UserRef
		)
		if err := rows.Scan(&cid, &u.ID, &u.Username, &u.DisplayName); err != nil {
			return nil, err
		}
		index[cid].Members = append(index[cid].Members, u)
	}
	return convs, rows.Err()
}

func (s *Service) announceConversation(ctx context.Context, c Conversation) {
	recipients := make([]uuid.UUID, len(c.Members))
	for i, m := range c.Members {
		recipients[i] = m.ID
	}
	// Per-user fields (unread, read marker) start empty for everyone.
	c.UnreadCount, c.LastReadMessageID = 0, nil
	s.events.Publish(ctx, events.Event{Type: events.ConversationCreated, Recipients: recipients, Data: c})
}

func isMember(ctx context.Context, q db.Querier, conv, user uuid.UUID) (bool, error) {
	var ok bool
	err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM conversation_members WHERE conversation_id = $1 AND user_id = $2)`,
		conv, user).Scan(&ok)
	return ok, err
}

func notFoundOr(err error) error {
	if err != nil {
		return err
	}
	return ErrNotFound
}

// messageRow scans a message joined with its (possibly deleted) author:
// id, conversation_id, content, created_at, author id, username, display name.
type messageRow struct {
	m                 Message
	authorID          *uuid.UUID
	username, display *string
}

func (r *messageRow) dest() []any {
	return []any{&r.m.ID, &r.m.ConversationID, &r.m.Content, &r.m.CreatedAt, &r.authorID, &r.username, &r.display}
}

func (r *messageRow) message() Message {
	if r.authorID != nil {
		r.m.Author = &social.UserRef{ID: *r.authorID, Username: *r.username, DisplayName: *r.display}
	}
	return r.m
}

func scanMessage(r pgx.CollectableRow) (Message, error) {
	var row messageRow
	err := r.Scan(row.dest()...)
	return row.message(), err
}

func dmKey(a, b uuid.UUID) string {
	if a.String() > b.String() {
		a, b = b, a
	}
	return a.String() + ":" + b.String()
}

func uuidCompare(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) }

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
