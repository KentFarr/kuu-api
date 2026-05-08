# Operating kuu-api

Day-2 runbooks. Pairs with [DEPLOYMENT.md](DEPLOYMENT.md). All commands
assume Docker Compose deploy on the VPS unless noted; for systemd, swap
`docker compose logs -f kuu-api` → `journalctl -u kuu-api -f` and similar.

## Tail logs

```sh
docker compose logs -f kuu-api
```

Logs are JSON-structured slog output. Pipe through `jq` to filter:

```sh
docker compose logs --since 10m kuu-api | jq -r 'select(.level=="ERROR")'
docker compose logs --since 1h  kuu-api | jq -r 'select(.request_id=="abc123…")'
```

The `request_id` field threads through every log line for a given inbound
request, including rollback lines emitted from cleanup defers.

## Run a migration

```sh
docker compose run --rm migrate up        # apply all
docker compose run --rm migrate status    # show current version
docker compose run --rm migrate down      # roll back one (dangerous)
```

`migrate up` is idempotent. Run it before starting `kuu-api` after any
release that ships migrations.

## Inspect the email queue

List the pending rows (most recent first, capped at 50):

```sh
curl -s -H "Authorization: Bearer $ADMIN_BEARER_TOKEN" \
     https://api.kuu.gg/admin/emails | jq
```

Each row has `id`, `to_addr`, `attempts`, `next_attempt_at`, `last_error`.
A row with `attempts >= max_attempts` and no `sent_at` is stuck — see
[Customer didn't get welcome email](#customer-didnt-get-welcome-email).

## Force-retry a stuck email

```sh
curl -s -X POST \
     -H "Authorization: Bearer $ADMIN_BEARER_TOKEN" \
     https://api.kuu.gg/admin/emails/<id>/retry
```

This sets `next_attempt_at = NOW()` so the worker picks it up on its next
tick (within 30s). It does **not** reset `attempts`, so if `max_attempts`
is already reached the worker still passes — fix the root cause first
(SMTP creds, recipient address) then bump the row in SQL or extend
`max_attempts` if you want to keep trying.

## Run reconciliation

Compares `servers` rows against the panel's actual state on the configured
node:

```sh
curl -s -H "Authorization: Bearer $ADMIN_BEARER_TOKEN" \
     https://api.kuu.gg/admin/reconciliation | jq
```

Returns three buckets:

- **tracked** — IDs present in both. Healthy.
- **orphaned** — DB row exists, panel doesn't. Likely a panel-side delete
  outside kuu-api. Decide whether to remove the DB row or recreate the
  panel server.
- **dangling** — Panel has a server kuu-api doesn't know about. Either a
  manual creation on the panel or a provisioner crash between
  `CreateServer` and the DB insert. Investigate before deleting.

## Rotate the admin bearer token

```sh
NEW=$(openssl rand -hex 32)
sed -i "s|^ADMIN_BEARER_TOKEN=.*|ADMIN_BEARER_TOKEN=$NEW|" /srv/kuu-api/.env
docker compose up -d kuu-api    # picks up new env on restart
```

Update any monitoring jobs that scrape `/metrics` or hit `/admin/*` to use
the new token. Rotation requires a process restart — no SIGHUP.

## Restart the service

```sh
docker compose restart kuu-api          # API only
docker compose down && docker compose up -d   # full stack
```

`docker compose down` does **not** remove volumes — Postgres data
survives. Only `docker compose down -v` wipes data.

## Restore from backup

Given a `pg_dump` gzip from `/var/backups/kuu-api/`:

```sh
# Spin up a fresh stack (perhaps on a scratch host) without applying any
# data:
docker compose up -d postgres

# Pipe the dump in. The image's psql is reachable via `exec`:
gunzip -c /var/backups/kuu-api/kuu-2026-05-08.sql.gz | \
  docker compose exec -T postgres psql -U kuu -d kuu

# Sanity check schema and goose version:
docker compose run --rm migrate status

# Then start the API:
docker compose up -d kuu-api
```

If you're restoring **over** an existing live DB, stop the API first
(`docker compose stop kuu-api`) so concurrent writes don't fight the
restore, drop and recreate the database, then load the dump.

## Metrics cheatsheet

Useful one-liners against `/metrics`:

```sh
TOK="Authorization: Bearer $ADMIN_BEARER_TOKEN"

# Webhook success/error split (last value across the lifetime of the process)
curl -s -H "$TOK" https://api.kuu.gg/metrics | grep '^kuu_webhook_events_total'

# p95 provision time
curl -s -H "$TOK" https://api.kuu.gg/metrics | grep 'kuu_provision_duration_seconds_bucket'

# DB pool saturation
curl -s -H "$TOK" https://api.kuu.gg/metrics | grep '^kuu_db_'

# Pending email queue depth (point-in-time, from the worker)
curl -s -H "$TOK" https://api.kuu.gg/metrics | grep '^kuu_email_pending'
```

The full instrument list is in
[internal/metrics/metrics.go](../internal/metrics/metrics.go).

## Common error patterns

### "duplicate key value violates unique constraint"

Stripe redelivered a webhook (this is expected). The handler is
idempotent via `webhook_events.event_id` — these errors are turned into
`{ outcome: "skipped" }` and a 200 response. If you see one returned to
Stripe as a 500, check whether two distinct events hashed to the same
constraint somehow.

### Customer didn't get welcome email

1. `GET /admin/emails` and find their row by `to_addr`.
2. Read `last_error`. Common causes:
   - **SMTP auth failed** — provider rotated creds, fix `.env`, restart.
   - **Connection refused** — SMTP host wrong, network egress blocked.
   - **Recipient rejected** — the customer's email address is bad. We
     can't fix this from the API.
3. If the cause is fixed, `POST /admin/emails/{id}/retry`.
4. If `attempts >= max_attempts`, the worker has given up. Bump the row:

   ```sql
   UPDATE pending_emails
      SET attempts = 0, next_attempt_at = NOW(), last_error = ''
    WHERE id = <id>;
   ```

   Then call retry. (A first-class admin endpoint for this is on the
   roadmap — see Tier 2 in the production plan.)
5. **The customer's panel password is generated once and only stored in
   the email body**. If the email is permanently lost, we need to reset
   the Pterodactyl user's password and send a fresh welcome — there is
   no admin endpoint for this yet. Drop into the panel UI and reset
   manually, then send the customer their new credentials by hand.

### Provisioner rolled back

Search logs for the `request_id`:

```sh
docker compose logs --since 1h kuu-api | \
  jq -r 'select(.request_id=="<id>") | "\(.level)\t\(.msg)\t\(.error // "")"'
```

The rollback chain unwinds in reverse order. The first ERROR line is the
root cause; subsequent `rolled back …` lines describe the cleanup. If you
see `failed to roll back …`, you have an orphan — run reconciliation.

### Pterodactyl client errors

Look for `pterodactyl API error (status N)` in logs. The full response
body is included for non-empty bodies. 422 typically means the request
shape is wrong (egg/nest IDs, allocation pool exhausted); 401/403 means
the API key was rotated or lost permissions.

### Database connection exhaustion

`kuu_db_in_use_connections` near `kuu_db_open_connections` for sustained
periods means we're saturating the pool. Either traffic spiked, or a
query is hanging. `MaxOpen` is 50 — if you genuinely need more, tune in
[internal/database/database.go](../internal/database/database.go) and
deploy.

## Health checks

- `/livez` — the process is up. Always 200. Wire this to your load
  balancer / OVH monitoring.
- `/readyz` — DB, Pterodactyl, SMTP all reachable. 503 with details when
  any probe fails. Use for "should this instance receive traffic" checks
  in orchestrators that distinguish liveness from readiness.
- `/health` — alias for `/livez`, kept for backwards compatibility.

## Shutting down cleanly

```sh
docker compose stop kuu-api
```

The container catches SIGTERM, drains in-flight HTTP requests for up to
30s (`http.Server.Shutdown`), then waits for the email queue worker, the
DB stats collector, and the daily cleaner to exit before the process
returns. Postgres and Caddy keep running.

`docker compose down` stops everything in dependency order. Add `-v` only
if you genuinely want to wipe the database volume.

## What lives where on the host

```
/srv/kuu-api/                 # the repo + .env
/var/lib/docker/volumes/      # postgres-data, caddy-data, caddy-config volumes
/var/backups/kuu-api/         # pg_dump archives
/etc/kuu-api/secrets          # systemd-only: the secrets file
```
