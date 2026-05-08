-- +goose Up
-- +goose StatementBegin
CREATE TABLE IF NOT EXISTS customers (
    id              SERIAL PRIMARY KEY,
    email           TEXT UNIQUE NOT NULL,
    stripe_customer_id TEXT,
    pterodactyl_user_id INTEGER,
    pterodactyl_username TEXT,
    created_at      TIMESTAMP DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS subscriptions (
    id              SERIAL PRIMARY KEY,
    customer_id     INTEGER REFERENCES customers(id),
    stripe_subscription_id TEXT UNIQUE,
    stripe_price_id TEXT NOT NULL,
    game            TEXT NOT NULL,
    status          TEXT NOT NULL DEFAULT 'active',
    created_at      TIMESTAMP DEFAULT NOW(),
    cancelled_at    TIMESTAMP
);

CREATE TABLE IF NOT EXISTS servers (
    id              SERIAL PRIMARY KEY,
    subscription_id INTEGER REFERENCES subscriptions(id),
    customer_id     INTEGER REFERENCES customers(id),
    pterodactyl_server_id INTEGER,
    server_name     TEXT,
    node_id         INTEGER NOT NULL,
    port            INTEGER,
    game            TEXT NOT NULL,
    plan_name       TEXT NOT NULL,
    status          TEXT NOT NULL DEFAULT 'active',
    created_at      TIMESTAMP DEFAULT NOW()
);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS servers;
DROP TABLE IF EXISTS subscriptions;
DROP TABLE IF EXISTS customers;
-- +goose StatementEnd
