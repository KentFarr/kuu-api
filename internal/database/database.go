package database

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"
	_ "github.com/lib/pq" // registers the "postgres" driver with database/sql
)

type DB struct {
	conn *sqlx.DB
}

func New(databaseURL string) (*DB, error) {
	conn, err := sqlx.Connect("postgres", databaseURL)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to database: %w", err)
	}

	conn.SetMaxOpenConns(50)
	conn.SetMaxIdleConns(10)
	conn.SetConnMaxLifetime(5 * time.Minute)

	return &DB{conn: conn}, nil
}

func (db *DB) Close() error {
	return db.conn.Close()
}

func (db *DB) Ping(ctx context.Context) error {
	return db.conn.PingContext(ctx)
}

// SQL exposes the underlying *sql.DB so callers like the metrics collector
// can read pool stats without us reimplementing every accessor.
func (db *DB) SQL() *sql.DB {
	return db.conn.DB
}
