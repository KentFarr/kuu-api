-- +goose Up
-- +goose StatementBegin
-- Track the egg variant and version chosen at provision time so support
-- can answer "what exactly did this customer get?" without grepping logs.
-- Both columns are nullable because rows created before this migration
-- pre-date variants entirely; new provisions always populate them.
ALTER TABLE servers ADD COLUMN IF NOT EXISTS variant TEXT;
ALTER TABLE servers ADD COLUMN IF NOT EXISTS version TEXT;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE servers DROP COLUMN IF EXISTS version;
ALTER TABLE servers DROP COLUMN IF EXISTS variant;
-- +goose StatementEnd
