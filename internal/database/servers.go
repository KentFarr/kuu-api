package database

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

type Server struct {
	ID                  int            `db:"id"`
	SubscriptionID      int            `db:"subscription_id"`
	CustomerID          int            `db:"customer_id"`
	PterodactylServerID int            `db:"pterodactyl_server_id"`
	ServerName          string         `db:"server_name"`
	NodeID              int            `db:"node_id"`
	Port                int            `db:"port"`
	Game                string         `db:"game"`
	Variant             sql.NullString `db:"variant"`
	Version             sql.NullString `db:"version"`
	Subdomain           sql.NullString `db:"subdomain"`
	PlanName            string         `db:"plan_name"`
	Status              string         `db:"status"`
	CreatedAt           time.Time      `db:"created_at"`
}

// CreateServerParams carries everything needed to insert a server row.
// Using a struct instead of a positional arg list keeps call sites readable
// as the schema grows (variant and version landed in migration 004).
type CreateServerParams struct {
	SubscriptionID      int
	CustomerID          int
	PterodactylServerID int
	NodeID              int
	Port                int
	ServerName          string
	Game                string
	Variant             string
	Version             string
	Subdomain           string
	PlanName            string
}

func (db *DB) CreateServer(p CreateServerParams) (*Server, error) {
	var server Server
	err := db.conn.QueryRowx(
		`INSERT INTO servers (subscription_id, customer_id, pterodactyl_server_id, server_name, node_id, port, game, variant, version, subdomain, plan_name, status)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, 'active')
		 RETURNING *`,
		p.SubscriptionID, p.CustomerID, p.PterodactylServerID, p.ServerName,
		p.NodeID, p.Port, p.Game, nullableStr(p.Variant), nullableStr(p.Version), nullableStr(p.Subdomain), p.PlanName,
	).StructScan(&server)

	if err != nil {
		return nil, fmt.Errorf("failed to create server: %w", err)
	}

	return &server, nil
}

func nullableStr(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

func (db *DB) GetServerBySubscription(subscriptionID int) (*Server, error) {
	var server Server
	err := db.conn.Get(&server, "SELECT * FROM servers WHERE subscription_id = $1", subscriptionID)
	if err != nil {
		return nil, fmt.Errorf("server not found: %w", err)
	}
	return &server, nil
}

func (db *DB) SuspendServer(subscriptionID int) error {
	_, err := db.conn.Exec(
		"UPDATE servers SET status = 'suspended' WHERE subscription_id = $1",
		subscriptionID,
	)
	return err
}

func (db *DB) UnsuspendServer(subscriptionID int) error {
	_, err := db.conn.Exec(
		"UPDATE servers SET status = 'active' WHERE subscription_id = $1 AND status = 'suspended'",
		subscriptionID,
	)
	return err
}

// ListServerPteroIDs returns every pterodactyl_server_id we have on record
// for the given node. Used by the reconciliation endpoint to diff our DB
// against what Pterodactyl actually reports.
func (db *DB) ListServerPteroIDs(nodeID int) ([]int, error) {
	var ids []int
	err := db.conn.Select(&ids,
		"SELECT pterodactyl_server_id FROM servers WHERE node_id = $1 AND pterodactyl_server_id IS NOT NULL",
		nodeID,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to list server pterodactyl ids: %w", err)
	}
	return ids, nil
}

// ListReapableServers returns suspended servers whose subscription was
// cancelled before cutoff — i.e. eligible for hard deletion under the
// grace-period policy. Subscription and customer rows are intentionally
// not joined in: the reaper only deletes the server resource, leaving
// billing/audit history intact.
func (db *DB) ListReapableServers(cutoff time.Time) ([]Server, error) {
	var servers []Server
	err := db.conn.Select(&servers,
		`SELECT s.* FROM servers s
		 JOIN subscriptions sub ON sub.id = s.subscription_id
		 WHERE s.status = 'suspended'
		   AND sub.status = 'suspended'
		   AND sub.cancelled_at IS NOT NULL
		   AND sub.cancelled_at < $1`,
		cutoff,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to list reapable servers: %w", err)
	}
	return servers, nil
}

func (db *DB) DeleteServerByID(id int) error {
	_, err := db.conn.Exec("DELETE FROM servers WHERE id = $1", id)
	return err
}

func (db *DB) GetLatestActiveServerByCustomerID(customerID int) (*Server, error) {
	var server Server
	err := db.conn.Get(&server,
		"SELECT * FROM servers WHERE customer_id = $1 AND status = 'active' ORDER BY created_at DESC LIMIT 1",
		customerID,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to look up server: %w", err)
	}
	return &server, nil
}
