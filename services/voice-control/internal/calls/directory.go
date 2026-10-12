package calls

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PGDirectory reads conversations and blocks from the api's database
// (read-only). The api owns that schema; these queries depend on the
// conversations, conversation_members and blocks tables.
type PGDirectory struct {
	Pool *pgxpool.Pool
}

func (d PGDirectory) Members(ctx context.Context, conv, user uuid.UUID) (string, []uuid.UUID, error) {
	var (
		kind    string
		members []uuid.UUID
	)
	err := d.Pool.QueryRow(ctx, `
		SELECT c.kind, array_agg(all_m.user_id ORDER BY all_m.user_id)
		FROM conversations c
		JOIN conversation_members me ON me.conversation_id = c.id AND me.user_id = $2
		JOIN conversation_members all_m ON all_m.conversation_id = c.id
		WHERE c.id = $1
		GROUP BY c.kind`, conv, user).Scan(&kind, &members)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil, ErrNotMember
	}
	return kind, members, err
}

func (d PGDirectory) Blocked(ctx context.Context, a, b uuid.UUID) (bool, error) {
	var blocked bool
	err := d.Pool.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM blocks WHERE (blocker = $1 AND blocked = $2) OR (blocker = $2 AND blocked = $1))`, a, b).Scan(&blocked)
	return blocked, err
}
