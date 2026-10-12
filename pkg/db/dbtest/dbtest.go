// Package dbtest gives each test an isolated, migrated Postgres schema.
//
// Tests are skipped unless PARLEY_TEST_DATABASE_URL is set (see `make test-db`).
package dbtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io/fs"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"parley/pkg/db"
)

const envURL = "PARLEY_TEST_DATABASE_URL"

// New creates a fresh schema, runs migrations into it and returns a pool whose
// search_path points at it. The schema is dropped when the test finishes.
func New(t testing.TB, migrations fs.FS) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv(envURL)
	if url == "" {
		t.Skipf("%s not set; skipping database test", envURL)
	}
	ctx := context.Background()

	b := make([]byte, 6)
	rand.Read(b)
	schema := "test_" + hex.EncodeToString(b)

	admin, err := db.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		admin.Close()
	})

	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := db.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	if err := db.Migrate(ctx, pool, migrations); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return pool
}
