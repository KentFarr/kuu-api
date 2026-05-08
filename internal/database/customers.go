package database

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

type Customer struct {
	ID                  int       `db:"id"`
	Email               string    `db:"email"`
	StripeCustomerID    *string   `db:"stripe_customer_id"`
	PterodactylUserID   *int      `db:"pterodactyl_user_id"`
	PterodactylUsername *string   `db:"pterodactyl_username"`
	CreatedAt           time.Time `db:"created_at"`
}

func (db *DB) CreateCustomer(email string, pteroUserID int, pteroUsername string) (*Customer, error) {
	var customer Customer
	err := db.conn.QueryRowx(
		`INSERT INTO customers (email, pterodactyl_user_id, pterodactyl_username)
		 VALUES ($1, $2, $3)
		 RETURNING *`,
		email, pteroUserID, pteroUsername,
	).StructScan(&customer)

	if err != nil {
		return nil, fmt.Errorf("failed to create customer: %w", err)
	}

	return &customer, nil
}

func (db *DB) GetCustomerByEmail(email string) (*Customer, error) {
	var customer Customer
	err := db.conn.Get(&customer, "SELECT * FROM customers WHERE email = $1", email)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to look up customer: %w", err)
	}
	return &customer, nil
}

func (db *DB) UpdateStripeCustomerID(customerID int, stripeCustomerID string) error {
	_, err := db.conn.Exec(
		"UPDATE customers SET stripe_customer_id = $1 WHERE id = $2",
		stripeCustomerID, customerID,
	)
	return err
}

func (db *DB) DeleteCustomer(customerID int) error {
	_, err := db.conn.Exec("DELETE FROM customers WHERE id = $1", customerID)
	return err
}
