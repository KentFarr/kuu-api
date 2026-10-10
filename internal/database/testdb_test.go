package database

import (
	"context"
	"os"
	"testing"
)

// newTestDB connects to TEST_DATABASE_URL, applies the migrations and
// empties every table, so each test starts from a clean schema. Tests are
// skipped when the variable is unset, so `go test ./...` works without a
// database.
//
// Point it at a throwaway database: everything in it is deleted. For example:
//
//	docker run --rm -d -p 5433:5432 -e POSTGRES_PASSWORD=test postgres:16-alpine
//	TEST_DATABASE_URL='postgres://postgres:test@localhost:5433/postgres?sslmode=disable' go test ./internal/database/
//
// Tests that use it must not call t.Parallel: they share one database.
func newTestDB(t *testing.T) *DB {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping database integration test")
	}

	if err := MigrateUp(context.Background(), url); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	db, err := New(url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	_, err = db.conn.Exec(`TRUNCATE customers, subscriptions, servers, webhook_events, pending_emails RESTART IDENTITY CASCADE`)
	if err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return db
}
