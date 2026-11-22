package admin

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// UserRow is an account as the admin app shows it.
type UserRow struct {
	ID            uuid.UUID  `json:"id"`
	Username      string     `json:"username"`
	DisplayName   string     `json:"display_name"`
	CreatedAt     time.Time  `json:"created_at"`
	Activated     bool       `json:"activated"`
	Email         string     `json:"email"`
	EmailVerified bool       `json:"email_verified"`
	Phone         string     `json:"phone"`
	PhoneVerified bool       `json:"phone_verified"`
	MFAEnabled    bool       `json:"mfa_enabled"`
	MFAPolicy     string     `json:"mfa_policy"`
	MFARequired   bool       `json:"mfa_required"` // policy resolved against the server default
	DisabledAt    *time.Time `json:"disabled_at"`
	LastActive    *time.Time `json:"last_active"` // newest login or token refresh
	Sessions      int        `json:"sessions"`    // signed-in devices
}

const userRowCols = `u.id, u.username, u.display_name, u.created_at, u.activated_at IS NOT NULL,
	coalesce(u.email, ''), u.email_verified_at IS NOT NULL, coalesce(u.phone, ''), u.phone_verified_at IS NOT NULL,
	u.mfa_enabled_at IS NOT NULL, u.mfa_policy,
	(u.mfa_policy = 'required' OR (u.mfa_policy = 'inherit' AND coalesce(
		(SELECT (ss.value->>'require')::boolean FROM server_settings ss WHERE ss.key = 'mfa'), true))),
	u.disabled_at,
	(SELECT max(t.created_at) FROM refresh_tokens t WHERE t.user_id = u.id),
	(SELECT count(DISTINCT t.family_id) FROM refresh_tokens t
		WHERE t.user_id = u.id AND t.revoked_at IS NULL AND t.used_at IS NULL AND t.expires_at > now())::int`

func scanUserRow(r pgx.Row) (UserRow, error) {
	var u UserRow
	err := r.Scan(&u.ID, &u.Username, &u.DisplayName, &u.CreatedAt, &u.Activated,
		&u.Email, &u.EmailVerified, &u.Phone, &u.PhoneVerified,
		&u.MFAEnabled, &u.MFAPolicy, &u.MFARequired, &u.DisabledAt, &u.LastActive, &u.Sessions)
	return u, err
}

var errUserNotFound = errors.New("user not found")

// likePattern matches q anywhere, with LIKE's wildcards taken literally.
func likePattern(q string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return "%" + r.Replace(q) + "%"
}

// searchUsers finds accounts by username, display name, email or phone.
func searchUsers(ctx context.Context, pool *pgxpool.Pool, q string, limit, offset int) ([]UserRow, int, error) {
	q = strings.TrimSpace(q)
	where := `TRUE`
	args := []any{limit, offset}
	if q != "" {
		where = `(u.username ILIKE $3 OR u.display_name ILIKE $3 OR u.email ILIKE $3 OR u.phone LIKE $3)`
		args = append(args, likePattern(q))
	}
	var total int
	countArgs := args[2:]
	countWhere := strings.ReplaceAll(where, "$3", "$1")
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM users u WHERE `+countWhere, countArgs...).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := pool.Query(ctx, `SELECT `+userRowCols+` FROM users u WHERE `+where+`
		ORDER BY u.created_at DESC, u.id LIMIT $1 OFFSET $2`, args...)
	if err != nil {
		return nil, 0, err
	}
	users, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (UserRow, error) { return scanUserRow(r) })
	return users, total, err
}

func getUserRow(ctx context.Context, pool *pgxpool.Pool, id uuid.UUID) (UserRow, error) {
	u, err := scanUserRow(pool.QueryRow(ctx, `SELECT `+userRowCols+` FROM users u WHERE u.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return UserRow{}, errUserNotFound
	}
	return u, err
}
