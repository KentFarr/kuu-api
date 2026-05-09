-- Migration 005: Add subdomain to servers table
-- Each server can optionally have a custom subdomain (e.g., "my-epic-server" for "my-epic-server.kuu.gg")
-- This is provisioned for Pro plans and above.

ALTER TABLE servers ADD COLUMN subdomain VARCHAR(64);

-- Add an index for lookups by subdomain, as we'll likely need to verify uniqueness
-- or resolve subdomains to server IDs in the future.
CREATE INDEX idx_servers_subdomain ON servers(subdomain);
