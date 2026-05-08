-- +goose Up
-- +goose StatementBegin
CREATE TABLE IF NOT EXISTS pending_emails (
    id              SERIAL PRIMARY KEY,
    to_addr         TEXT NOT NULL,
    subject         TEXT NOT NULL,
    body            TEXT NOT NULL,
    attempts        INT NOT NULL DEFAULT 0,
    max_attempts    INT NOT NULL DEFAULT 5,
    last_error      TEXT,
    next_attempt_at TIMESTAMP NOT NULL DEFAULT NOW(),
    created_at      TIMESTAMP NOT NULL DEFAULT NOW(),
    sent_at         TIMESTAMP
);

CREATE INDEX IF NOT EXISTS pending_emails_due_idx
    ON pending_emails (next_attempt_at)
    WHERE sent_at IS NULL;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS pending_emails_due_idx;
DROP TABLE IF EXISTS pending_emails;
-- +goose StatementEnd
