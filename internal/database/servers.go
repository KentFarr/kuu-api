package database

import (
	"fmt"
	"time"
)

type Server struct {
	ID                  int       `db:"id"`
	SubscriptionID      int       `db:"subscription_id"`
	CustomerID          int       `db:"customer_id"`
	PterodactylServerID int       `db:"pterodactyl_server_id"`
	ServerName          string    `db:"server_name"`
	NodeID              int       `db:"node_id"`
	Port                int       `db:"port"`
	Game                string    `db:"game"`
	PlanName            string    `db:"plan_name"`
	Status              string    `db:"status"`
	CreatedAt           time.Time `db:"created_at"`
}

func (db *DB) CreateServer(subscriptionID, customerID, pteroServerID, nodeID, port int, serverName, game, planName string) (*Server, error) {
	var server Server
	err := db.conn.QueryRowx(
		`INSERT INTO servers (subscription_id, customer_id, pterodactyl_server_id, server_name, node_id, port, game, plan_name, status)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'active')
		 RETURNING *`,
		subscriptionID, customerID, pteroServerID, serverName, nodeID, port, game, planName,
	).StructScan(&server)

	if err != nil {
		return nil, fmt.Errorf("failed to create server: %w", err)
	}

	return &server, nil
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
