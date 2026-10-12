// Package social owns the friend graph: friend requests, friendships and blocks.
//
// Blocks are enforced here, server-side. Other modules (messaging, voice) must
// call CanInteract before letting two users reach each other.
package social

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"parley/pkg/events"
)

var (
	// ErrCannotSend is deliberately vague: it covers both "no such user" and
	// "that user has blocked you", so a block can't be detected by probing.
	ErrCannotSend      = errors.New("could not send friend request; check the username")
	ErrSelf            = errors.New("you cannot do that to yourself")
	ErrAlreadyFriends  = errors.New("you are already friends")
	ErrYouBlocked      = errors.New("you have blocked this user; unblock them first")
	ErrRequestNotFound = errors.New("friend request not found")
	ErrNotFriends      = errors.New("not friends with this user")
	ErrUserNotFound    = errors.New("user not found")
)

type UserRef struct {
	ID          uuid.UUID `json:"id"`
	Username    string    `json:"username"`
	DisplayName string    `json:"display_name"`
}

type Request struct {
	ID        uuid.UUID `json:"id"`
	From      UserRef   `json:"from"`
	To        UserRef   `json:"to"`
	CreatedAt time.Time `json:"created_at"`
}

type Friend struct {
	User  UserRef   `json:"user"`
	Since time.Time `json:"since"`
}

type Blocked struct {
	User      UserRef   `json:"user"`
	BlockedAt time.Time `json:"blocked_at"`
}

// SendResult is either a pending Request or, if the other user had already
// sent us one, the new Friendship.
type SendResult struct {
	Request    *Request `json:"request,omitempty"`
	Friendship *Friend  `json:"friendship,omitempty"`
}

type Service struct {
	pool   *pgxpool.Pool
	events events.Publisher
}

func NewService(pool *pgxpool.Pool, pub events.Publisher) *Service {
	return &Service{pool: pool, events: pub}
}

// SendRequest sends a friend request from one user to the user named toUsername.
// If toUsername already has a pending request to from, the two become friends.
func (s *Service) SendRequest(ctx context.Context, from uuid.UUID, toUsername string) (SendResult, error) {
	var (
		res SendResult
		out []events.Event
	)
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		out = nil
		to, err := userByUsername(ctx, tx, toUsername)
		if errors.Is(err, ErrUserNotFound) {
			return ErrCannotSend
		}
		if err != nil {
			return err
		}
		if to.ID == from {
			return ErrSelf
		}
		if err := lockPair(ctx, tx, from, to.ID); err != nil {
			return err
		}
		youBlocked, theyBlocked, err := blockState(ctx, tx, from, to.ID)
		if err != nil {
			return err
		}
		if youBlocked {
			return ErrYouBlocked
		}
		if theyBlocked {
			return ErrCannotSend
		}
		friends, err := areFriends(ctx, tx, from, to.ID)
		if err != nil {
			return err
		}
		if friends {
			return ErrAlreadyFriends
		}
		me, err := userByID(ctx, tx, from)
		if err != nil {
			return err
		}

		// They already asked us: accept instead of creating a mirror request.
		var reverseID uuid.UUID
		err = tx.QueryRow(ctx, `DELETE FROM friend_requests WHERE from_user = $1 AND to_user = $2 RETURNING id`,
			to.ID, from).Scan(&reverseID)
		if err == nil {
			since, err := insertFriendship(ctx, tx, from, to.ID)
			if err != nil {
				return err
			}
			res = SendResult{Friendship: &Friend{User: to, Since: since}}
			out = append(out, requestRemoved(reverseID, from, to.ID))
			out = append(out, friendshipCreated(me, to, since)...)
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}

		// Re-sending a pending request is a no-op that returns the existing one.
		req := Request{From: me, To: to}
		var inserted bool
		err = tx.QueryRow(ctx, `
			INSERT INTO friend_requests (from_user, to_user) VALUES ($1, $2)
			ON CONFLICT (from_user, to_user) DO UPDATE SET from_user = EXCLUDED.from_user
			RETURNING id, created_at, (xmax = 0)`, from, to.ID,
		).Scan(&req.ID, &req.CreatedAt, &inserted)
		if err != nil {
			return fmt.Errorf("insert friend request: %w", err)
		}
		res = SendResult{Request: &req}
		if inserted {
			out = append(out, events.Event{Type: events.FriendRequestCreated, Recipients: []uuid.UUID{from, to.ID}, Data: req})
		}
		return nil
	})
	if err != nil {
		return SendResult{}, err
	}
	s.publish(ctx, out)
	return res, nil
}

// AcceptRequest accepts an incoming request addressed to user.
func (s *Service) AcceptRequest(ctx context.Context, user, requestID uuid.UUID) (Friend, error) {
	var (
		f   Friend
		out []events.Event
	)
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		out = nil
		var fromID uuid.UUID
		err := tx.QueryRow(ctx, `SELECT from_user FROM friend_requests WHERE id = $1 AND to_user = $2`,
			requestID, user).Scan(&fromID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrRequestNotFound
		}
		if err != nil {
			return err
		}
		// Always take the pair lock before row locks, matching SendRequest and Block.
		if err := lockPair(ctx, tx, user, fromID); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `DELETE FROM friend_requests WHERE id = $1 AND to_user = $2`, requestID, user)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrRequestNotFound // raced with cancel/block
		}
		since, err := insertFriendship(ctx, tx, user, fromID)
		if err != nil {
			return err
		}
		me, err := userByID(ctx, tx, user)
		if err != nil {
			return err
		}
		them, err := userByID(ctx, tx, fromID)
		if err != nil {
			return err
		}
		f = Friend{User: them, Since: since}
		out = append(out, requestRemoved(requestID, user, fromID))
		out = append(out, friendshipCreated(me, them, since)...)
		return nil
	})
	if err != nil {
		return Friend{}, err
	}
	s.publish(ctx, out)
	return f, nil
}

// DeleteRequest declines an incoming request or cancels an outgoing one.
func (s *Service) DeleteRequest(ctx context.Context, user, requestID uuid.UUID) error {
	var from, to uuid.UUID
	err := s.pool.QueryRow(ctx, `
		DELETE FROM friend_requests WHERE id = $1 AND (from_user = $2 OR to_user = $2)
		RETURNING from_user, to_user`, requestID, user).Scan(&from, &to)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrRequestNotFound
	}
	if err != nil {
		return err
	}
	s.publish(ctx, []events.Event{requestRemoved(requestID, from, to)})
	return nil
}

func (s *Service) RemoveFriend(ctx context.Context, user, friend uuid.UUID) error {
	a, b := ordered(user, friend)
	tag, err := s.pool.Exec(ctx, `DELETE FROM friendships WHERE user_a = $1 AND user_b = $2`, a, b)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFriends
	}
	s.publish(ctx, friendshipRemoved(user, friend))
	return nil
}

// Block blocks target, ending any friendship and pending requests between the
// two users. Blocking is idempotent.
func (s *Service) Block(ctx context.Context, user, target uuid.UUID) error {
	if user == target {
		return ErrSelf
	}
	var out []events.Event
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		out = nil
		if _, err := userByID(ctx, tx, target); err != nil {
			return err
		}
		if err := lockPair(ctx, tx, user, target); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO blocks (blocker, blocked) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
			user, target); err != nil {
			return err
		}
		a, b := ordered(user, target)
		tag, err := tx.Exec(ctx, `DELETE FROM friendships WHERE user_a = $1 AND user_b = $2`, a, b)
		if err != nil {
			return err
		}
		if tag.RowsAffected() > 0 {
			out = append(out, friendshipRemoved(user, target)...)
		}
		rows, err := tx.Query(ctx, `
			DELETE FROM friend_requests
			WHERE (from_user = $1 AND to_user = $2) OR (from_user = $2 AND to_user = $1)
			RETURNING id`, user, target)
		if err != nil {
			return err
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
		if err != nil {
			return err
		}
		for _, id := range ids {
			out = append(out, requestRemoved(id, user, target))
		}
		return nil
	})
	if err != nil {
		return err
	}
	s.publish(ctx, out)
	return nil
}

// Unblock is idempotent.
func (s *Service) Unblock(ctx context.Context, user, target uuid.UUID) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM blocks WHERE blocker = $1 AND blocked = $2`, user, target)
	return err
}

func (s *Service) ListFriends(ctx context.Context, user uuid.UUID) ([]Friend, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT u.id, u.username, u.display_name, f.created_at
		FROM friendships f
		JOIN users u ON u.id = CASE WHEN f.user_a = $1 THEN f.user_b ELSE f.user_a END
		WHERE f.user_a = $1 OR f.user_b = $1
		ORDER BY u.username`, user)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Friend, error) {
		var f Friend
		err := r.Scan(&f.User.ID, &f.User.Username, &f.User.DisplayName, &f.Since)
		return f, err
	})
}

// ListRequests returns pending requests addressed to user and sent by user.
func (s *Service) ListRequests(ctx context.Context, user uuid.UUID) (incoming, outgoing []Request, err error) {
	rows, err := s.pool.Query(ctx, `
		SELECT r.id, r.created_at,
		       f.id, f.username, f.display_name,
		       t.id, t.username, t.display_name
		FROM friend_requests r
		JOIN users f ON f.id = r.from_user
		JOIN users t ON t.id = r.to_user
		WHERE r.from_user = $1 OR r.to_user = $1
		ORDER BY r.created_at DESC`, user)
	if err != nil {
		return nil, nil, err
	}
	all, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Request, error) {
		var q Request
		err := r.Scan(&q.ID, &q.CreatedAt,
			&q.From.ID, &q.From.Username, &q.From.DisplayName,
			&q.To.ID, &q.To.Username, &q.To.DisplayName)
		return q, err
	})
	if err != nil {
		return nil, nil, err
	}
	incoming, outgoing = []Request{}, []Request{}
	for _, q := range all {
		if q.To.ID == user {
			incoming = append(incoming, q)
		} else {
			outgoing = append(outgoing, q)
		}
	}
	return incoming, outgoing, nil
}

func (s *Service) ListBlocked(ctx context.Context, user uuid.UUID) ([]Blocked, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT u.id, u.username, u.display_name, b.created_at
		FROM blocks b JOIN users u ON u.id = b.blocked
		WHERE b.blocker = $1
		ORDER BY u.username`, user)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Blocked, error) {
		var b Blocked
		err := r.Scan(&b.User.ID, &b.User.Username, &b.User.DisplayName, &b.BlockedAt)
		return b, err
	})
}

// CanInteract reports whether neither user has blocked the other. Messaging and
// voice must check this before delivering anything between two users.
func (s *Service) CanInteract(ctx context.Context, a, b uuid.UUID) (bool, error) {
	x, y, err := blockState(ctx, s.pool, a, b)
	return !x && !y, err
}

func (s *Service) AreFriends(ctx context.Context, a, b uuid.UUID) (bool, error) {
	return areFriends(ctx, s.pool, a, b)
}

func (s *Service) publish(ctx context.Context, evs []events.Event) {
	for _, e := range evs {
		s.events.Publish(ctx, e)
	}
}
