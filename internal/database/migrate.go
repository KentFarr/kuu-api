package database

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"

	"github.com/pressly/goose/v3"

	"github.com/KentFarr/kuu-api/migrations"
)

func newGooseProvider(db *sql.DB) (*goose.Provider, error) {
	subFS, err := fs.Sub(migrations.FS, ".")
	if err != nil {
		return nil, fmt.Errorf("failed to scope migrations FS: %w", err)
	}
	return goose.NewProvider(goose.DialectPostgres, db, subFS)
}

func MigrateUp(ctx context.Context, databaseURL string) error {
	conn, err := sql.Open("postgres", databaseURL)
	if err != nil {
		return fmt.Errorf("failed to open database for migration: %w", err)
	}
	defer conn.Close()

	provider, err := newGooseProvider(conn)
	if err != nil {
		return err
	}

	results, err := provider.Up(ctx)
	if err != nil {
		return fmt.Errorf("migration up failed: %w", err)
	}
	for _, r := range results {
		if r.Empty {
			continue
		}
		fmt.Printf("applied %s in %s\n", r.Source.Path, r.Duration)
	}
	return nil
}

func MigrateDown(ctx context.Context, databaseURL string) error {
	conn, err := sql.Open("postgres", databaseURL)
	if err != nil {
		return fmt.Errorf("failed to open database for migration: %w", err)
	}
	defer conn.Close()

	provider, err := newGooseProvider(conn)
	if err != nil {
		return err
	}

	result, err := provider.Down(ctx)
	if err != nil {
		return fmt.Errorf("migration down failed: %w", err)
	}
	if result != nil {
		fmt.Printf("rolled back %s\n", result.Source.Path)
	}
	return nil
}

func MigrateStatus(ctx context.Context, databaseURL string) error {
	conn, err := sql.Open("postgres", databaseURL)
	if err != nil {
		return fmt.Errorf("failed to open database for migration: %w", err)
	}
	defer conn.Close()

	provider, err := newGooseProvider(conn)
	if err != nil {
		return err
	}

	statuses, err := provider.Status(ctx)
	if err != nil {
		return fmt.Errorf("migration status failed: %w", err)
	}
	for _, s := range statuses {
		state := "pending"
		if s.State == "applied" {
			state = fmt.Sprintf("applied at %s", s.AppliedAt.Format("2006-01-02 15:04:05"))
		}
		fmt.Printf("%-50s %s\n", s.Source.Path, state)
	}
	return nil
}
