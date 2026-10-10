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

Compares `servers` rows against the panel's actual state across every
node listed in `NODE_IDS`:

```sh
curl -s -H "Authorization: Bearer $ADMIN_BEARER_TOKEN" \
     https://api.kuu.gg/admin/reconciliation | jq
```

Returns three buckets:

- **tracked** — IDs present in both. Healthy.
- **orphaned** — DB row exists, panel doesn't. Likely a panel-side delete
  outside kuu-api, or the daily reaper deleting the panel side then
  failing to delete the DB row (rare). Decide whether to remove the DB
  row or recreate the panel server.
- **dangling** — Panel has a server kuu-api doesn't know about. Either a
  manual creation on the panel or a provisioner crash between
  `CreateServer` and the DB insert. Investigate before deleting.

If you've added a node to `NODE_IDS` recently, expect every server still
on the old single node to show up as tracked — reconciliation iterates
the configured pool, so a node missing from `NODE_IDS` is invisible to
this report. Keep the env in sync with the panel.

## Subscription lifecycle and the reaper

Cancellation, payment failure, and hard-deletion all run through the
same wiring. Walking the timeline for a single customer:

0. A renewal payment fails: Stripe sends `invoice.payment_failed`. The
   subscription becomes `past_due` and the customer is emailed a link
   to pay the invoice with another card. **The server keeps running**
   while Stripe retries. If a retry succeeds (`invoice.paid`), the
   subscription goes back to `active` and nothing else happens.
1. Stripe gives up (`customer.subscription.updated` with status
   `unpaid` or `canceled`) or the customer cancels
   (`customer.subscription.deleted`). The webhook handler calls
   `Provisioner.SuspendServer`, which suspends the Pterodactyl server,
   sets `subscriptions.status='suspended'`, records
   `suspended_reason` (`non_payment` or `cancelled`), sets
   `cancelled_at` if it isn't already set, and sets
   `servers.status='suspended'`. If an `unpaid` subscription's invoice
   is paid later, `invoice.paid` unsuspends it automatically (only for
   `non_payment`, and only until the reaper has run). The customer can no longer
   start their server but their world files are intact on the panel
   node.
2. The daily `Reaper` (in `internal/provisioner/reaper.go`) wakes up
   24h later. For every server whose subscription was cancelled more
   than 14 days ago, it calls Pterodactyl `DeleteServer` and then
   removes the `servers` row.
3. `subscriptions`, `customers`, and the Pterodactyl user account are
   never touched by the reaper — billing history is preserved, and a
   returning customer keeps their login on the panel.

What this means for ops:

- **Restoring a recently-cancelled customer (within 14d):** unsuspend
  in the panel UI and re-activate the subscription in Stripe; the
  webhook fires and the system catches up. There is no admin endpoint
  for this yet.
- **After 14d:** the panel server is gone. A new checkout for the same
  email reuses the existing Pterodactyl user (they keep their login)
  and creates a fresh server with a new world.
- **Changing the grace period:** edit `defaultReapGracePeriod` in
  `internal/provisioner/reaper.go` and redeploy. Deliberately a code
  constant rather than an env knob so the policy lives in code review.
- **Watching the reaper:** filter logs for `msg=~"reaper:"`. A run
  that finds nothing logs nothing, so silence is the steady state.

```sh
docker compose logs --since 48h kuu-api | jq -r 'select(.msg | startswith("reaper:") // false)'
```

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

## Backups

Off-host Postgres backups run as a separate `backup` service in
docker-compose, gated behind the `backup` profile. The service shells
out to `pg_dump`, gzips, uploads to an S3-compatible bucket, and prunes
objects older than the retention window each cycle.

### One-time setup

1. Sign up for an S3-compatible provider. Recommended:
   - **Backblaze B2** — cheapest storage, 10 GB free
     (`https://s3.us-west-002.backblazeb2.com` or your bucket region)
   - **Cloudflare R2** — free egress, 10 GB free
     (`https://<accountid>.r2.cloudflarestorage.com`)
2. Create a bucket, e.g. `kuu-backups`.
3. Create an application key scoped to that bucket. Save the access key
   and secret.
4. Fill in the `BACKUP_S3_*` block in `.env`. See `.env.example` for the
   full set; the required fields are endpoint, bucket, access key, and
   secret key.
5. Bring the service up:

   ```sh
   docker compose --profile backup up -d backup
   ```

Logs (`docker compose logs -f backup`) will show a successful
bucket-existence check at startup, then a `backup uploaded key=...`
line on each cycle. If `BACKUP_RUN_ON_START=true` (the default) you'll
see the first dump within seconds.

### Restore from backup

Listing backups in your bucket:

```sh
# Backblaze: install b2 CLI, or use rclone, or the web console.
# Quick check via mc (minio client):
mc alias set kuubackups $BACKUP_S3_ENDPOINT $BACKUP_S3_ACCESS_KEY $BACKUP_S3_SECRET_KEY
mc ls kuubackups/$BACKUP_S3_BUCKET/kuu/
```

Pulling and restoring a specific dump:

```sh
# Download (replace timestamp with the object you want).
mc cp kuubackups/$BACKUP_S3_BUCKET/kuu/kuu-2026-05-08T030000Z.sql.gz .

# Spin up a fresh stack (perhaps on a scratch host) without applying any
# data:
docker compose up -d postgres

# Pipe the dump in. The image's psql is reachable via `exec`:
gunzip -c kuu-2026-05-08T030000Z.sql.gz | \
  docker compose exec -T postgres psql -U kuu -d kuu

# Sanity check schema and goose version:
docker compose run --rm migrate status

# Then start the API:
docker compose up -d kuu-api
```

If you're restoring **over** an existing live DB, stop the API first
(`docker compose stop kuu-api`) so concurrent writes don't fight the
restore, drop and recreate the database, then load the dump.

### Drill it before you need it

Once a quarter, restore the most recent backup into a scratch Postgres
container and run `docker compose run --rm migrate status` to confirm
the dump is loadable. Backups that have never been tested are not
backups.

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

# Per-node memory + free allocations (refreshed every 30s)
curl -s -H "$TOK" https://api.kuu.gg/metrics | grep '^kuu_node_'

# How traffic is distributing across the node pool
curl -s -H "$TOK" https://api.kuu.gg/metrics | grep '^kuu_provision_node_chosen_total'
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

### "subscription not found" on customer.subscription.deleted

Benign log noise that fires when Stripe sends
`customer.subscription.deleted` for a subscription the DB no longer
knows about. Two ways this happens, both expected:

- A provisioning attempt failed and the rollback chain deleted the
  subscription row before the deletion webhook arrived. The customer
  was never charged successfully, so there is nothing to suspend.
- A subscription that was cancelled long enough ago to have been
  reaped is being deleted (or re-deleted) on the Stripe side.

Today this surfaces as an `ERROR`-level log; treat it as informational
unless it pairs with a customer complaint about a server that should
have been suspended. A future polish item is to demote this specific
case to `INFO` so it stops triggering alerting hooks.

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
5. **The welcome email holds no password**, so a lost email is only an
   inconvenience: the customer (or the frontend, via
   `POST /api/support/resend-welcome`) can get the server details
   re-sent. To log in, they set or reset their password themselves at
   `panel.kuu.gg/auth/password`. If they never received the panel's
   "Account Created" setup email, that page sends a fresh link. This
   needs the panel's own mail settings to work; check the panel's
   mail logs if reset emails aren't arriving.

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
DB stats collector, the per-node metrics sampler, the daily cleaner,
and the suspended-server reaper to exit before the process returns.
Postgres and Caddy keep running.

`docker compose down` stops everything in dependency order. Add `-v` only
if you genuinely want to wipe the database volume.

## What lives where on the host

```text
/srv/kuu-api/                 # the repo + .env
/var/lib/docker/volumes/      # postgres-data, caddy-data, caddy-config volumes
/etc/kuu-api/secrets          # systemd-only: the secrets file
```

Off-host backups (object storage) are configured via `BACKUP_S3_*` and
are inspected with the provider's own tooling — they don't live on this
host. See [Backups](#backups).
