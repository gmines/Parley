package social

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"parley/pkg/db"
	"parley/pkg/events"
)

func ordered(a, b uuid.UUID) (uuid.UUID, uuid.UUID) {
	if a.String() < b.String() {
		return a, b
	}
	return b, a
}

// lockPair serializes all graph changes between two users for the rest of the
// transaction. Callers must take it before any row locks to avoid deadlocks.
func lockPair(ctx context.Context, tx pgx.Tx, a, b uuid.UUID) error {
	x, y := ordered(a, b)
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, x.String()+":"+y.String())
	return err
}

func userByUsername(ctx context.Context, q db.Querier, username string) (UserRef, error) {
	return scanUser(q.QueryRow(ctx,
		`SELECT id, username, display_name FROM users WHERE username = lower(trim($1))`, username))
}

func userByID(ctx context.Context, q db.Querier, id uuid.UUID) (UserRef, error) {
	return scanUser(q.QueryRow(ctx, `SELECT id, username, display_name FROM users WHERE id = $1`, id))
}

func scanUser(row pgx.Row) (UserRef, error) {
	var u UserRef
	err := row.Scan(&u.ID, &u.Username, &u.DisplayName)
	if errors.Is(err, pgx.ErrNoRows) {
		return UserRef{}, ErrUserNotFound
	}
	return u, err
}

// blockState reports whether a blocked b and whether b blocked a.
func blockState(ctx context.Context, q db.Querier, a, b uuid.UUID) (aBlockedB, bBlockedA bool, err error) {
	err = q.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM blocks WHERE blocker = $1 AND blocked = $2),
		       EXISTS (SELECT 1 FROM blocks WHERE blocker = $2 AND blocked = $1)`, a, b,
	).Scan(&aBlockedB, &bBlockedA)
	return
}

func areFriends(ctx context.Context, q db.Querier, a, b uuid.UUID) (bool, error) {
	x, y := ordered(a, b)
	var ok bool
	err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM friendships WHERE user_a = $1 AND user_b = $2)`, x, y).Scan(&ok)
	return ok, err
}

func insertFriendship(ctx context.Context, tx pgx.Tx, a, b uuid.UUID) (time.Time, error) {
	x, y := ordered(a, b)
	var since time.Time
	err := tx.QueryRow(ctx, `
		INSERT INTO friendships (user_a, user_b) VALUES ($1, $2)
		ON CONFLICT (user_a, user_b) DO UPDATE SET user_a = EXCLUDED.user_a
		RETURNING created_at`, x, y).Scan(&since)
	return since, err
}

// Event constructors. Friendship events are per-recipient because each side's
// payload names the other user.

func requestRemoved(id, a, b uuid.UUID) events.Event {
	return events.Event{
		Type:       events.FriendRequestRemoved,
		Recipients: []uuid.UUID{a, b},
		Data:       map[string]uuid.UUID{"id": id},
	}
}

func friendshipCreated(a, b UserRef, since time.Time) []events.Event {
	return []events.Event{
		{Type: events.FriendshipCreated, Recipients: []uuid.UUID{a.ID}, Data: Friend{User: b, Since: since}},
		{Type: events.FriendshipCreated, Recipients: []uuid.UUID{b.ID}, Data: Friend{User: a, Since: since}},
	}
}

func friendshipRemoved(a, b uuid.UUID) []events.Event {
	return []events.Event{
		{Type: events.FriendshipRemoved, Recipients: []uuid.UUID{a}, Data: map[string]uuid.UUID{"user_id": b}},
		{Type: events.FriendshipRemoved, Recipients: []uuid.UUID{b}, Data: map[string]uuid.UUID{"user_id": a}},
	}
}
