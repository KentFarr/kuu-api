-- +goose Up
-- +goose StatementBegin
-- Why a subscription was suspended, so a recovered payment only restores
-- servers we suspended for non-payment, never ones the customer cancelled.
-- Values: 'non_payment', 'cancelled'. NULL while active or past_due.
ALTER TABLE subscriptions ADD COLUMN IF NOT EXISTS suspended_reason TEXT;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE subscriptions DROP COLUMN IF EXISTS suspended_reason;
-- +goose StatementEnd
