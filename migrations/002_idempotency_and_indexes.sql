-- +goose Up
-- +goose StatementBegin
CREATE TABLE IF NOT EXISTS webhook_events (
    event_id     TEXT PRIMARY KEY,
    processed_at TIMESTAMP NOT NULL DEFAULT NOW()
);

ALTER TABLE subscriptions DROP CONSTRAINT IF EXISTS subscriptions_customer_id_fkey;
ALTER TABLE subscriptions
    ADD CONSTRAINT subscriptions_customer_id_fkey
    FOREIGN KEY (customer_id) REFERENCES customers(id) ON DELETE RESTRICT;

ALTER TABLE servers DROP CONSTRAINT IF EXISTS servers_subscription_id_fkey;
ALTER TABLE servers
    ADD CONSTRAINT servers_subscription_id_fkey
    FOREIGN KEY (subscription_id) REFERENCES subscriptions(id) ON DELETE RESTRICT;

ALTER TABLE servers DROP CONSTRAINT IF EXISTS servers_customer_id_fkey;
ALTER TABLE servers
    ADD CONSTRAINT servers_customer_id_fkey
    FOREIGN KEY (customer_id) REFERENCES customers(id) ON DELETE RESTRICT;

CREATE UNIQUE INDEX IF NOT EXISTS servers_pterodactyl_server_id_key
    ON servers (pterodactyl_server_id)
    WHERE pterodactyl_server_id IS NOT NULL;

CREATE INDEX IF NOT EXISTS subscriptions_customer_id_idx ON subscriptions (customer_id);
CREATE INDEX IF NOT EXISTS servers_subscription_id_idx ON servers (subscription_id);
CREATE INDEX IF NOT EXISTS servers_customer_id_idx ON servers (customer_id);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS servers_customer_id_idx;
DROP INDEX IF EXISTS servers_subscription_id_idx;
DROP INDEX IF EXISTS subscriptions_customer_id_idx;
DROP INDEX IF EXISTS servers_pterodactyl_server_id_key;
ALTER TABLE servers DROP CONSTRAINT IF EXISTS servers_customer_id_fkey;
ALTER TABLE servers DROP CONSTRAINT IF EXISTS servers_subscription_id_fkey;
ALTER TABLE subscriptions DROP CONSTRAINT IF EXISTS subscriptions_customer_id_fkey;
DROP TABLE IF EXISTS webhook_events;
-- +goose StatementEnd
