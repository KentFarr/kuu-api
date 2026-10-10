# kuu-api

Go backend that turns a Stripe Checkout payment into a running game server on a Pterodactyl panel, with no human in the loop.

[![CI](https://github.com/KentFarr/kuu-api/actions/workflows/ci.yml/badge.svg)](https://github.com/KentFarr/kuu-api/actions/workflows/ci.yml)
![Go version](https://img.shields.io/github/go-mod/go-version/KentFarr/kuu-api)

**Status:** kuu.gg ran in production in 2026 and is currently offline; this repo is the complete backend.

## What happens when someone pays

1. **Checkout.** The frontend posts the chosen game, variant, version, plan and optional subdomain to `POST /api/checkout` (rate-limited per IP). kuu-api validates them against the game catalog and creates a Stripe Checkout Session in subscription mode, with the choices stored in the session metadata.
2. **Webhook.** After payment, Stripe sends `checkout.session.completed` to `POST /api/webhook`.
3. **Signature check.** The body is capped at 1 MiB and verified against `STRIPE_WEBHOOK_SECRET`. A bad or missing signature gets a 401 and nothing else happens.
4. **Idempotency claim.** The event ID is inserted into `webhook_events` with `ON CONFLICT DO NOTHING`. If the row already existed, this delivery is a duplicate: return 200 and stop.
5. **Pterodactyl user.** A returning customer (same email) reuses their existing panel account. Otherwise a new one is created with the username they typed at checkout, or a generated one.
6. **Database rows.** The customer and subscription rows are written.
7. **Node and port.** The least-loaded node that can fit the plan's RAM is picked, then the first free allocation in that game's port range.
8. **Server.** The server is created on the panel with the variant's egg, Docker image, environment and the plan's limits. If the allocation was taken in the meantime (HTTP 422), it re-picks a node and retries, up to 3 attempts.
9. **Server row.** The server is recorded in Postgres, linked to the customer and subscription.
10. **DNS.** If a subdomain was chosen and Cloudflare is configured, a CNAME `<subdomain>.kuu.gg → node` is created.
11. **Welcome email.** The server details are queued in `pending_emails`, and a background worker sends them. For a new account, Pterodactyl separately emails a one-time link to set a password, so kuu-api never handles one.

If any of steps 5–9 fails, everything created so far is undone, the event claim is released, and the webhook returns 500 so Stripe's retry starts clean.

Cancellation (`customer.subscription.deleted`) and failed payments (`invoice.payment_failed`) suspend the server. A daily reaper deletes servers that have been suspended for more than 14 days.

## How it fits together

```mermaid
flowchart LR
    Browser["Frontend<br/>(kuu.gg)"] -->|POST /api/checkout| API
    Browser -->|pays| Stripe["Stripe Checkout"]
    Stripe -->|"POST /api/webhook<br/>(signed)"| API

    subgraph API["kuu-api (Go)"]
        H["HTTP handlers"] --> P["Provisioner"]
        Q["Email queue worker"]
        R["Reaper (daily)"]
    end

    P -->|"users, nodes,<br/>allocations, servers"| Ptero["Pterodactyl<br/>Application API"]
    P -->|CNAME| CF["Cloudflare DNS"]
    P -->|"lib/pq + sqlx"| PG[("Postgres")]
    Q <-->|pending_emails| PG
    Q -->|SMTP| Mail["Welcome email"]
    R --> Ptero
    R --> PG
```

What runs in the box:

- **kuu-api** — the HTTP server (handlers, provisioner, email queue worker,
  daily idempotency cleaner, daily suspended-server reaper, Prometheus
  metrics).
- **kuu-migrate** — a one-shot binary that runs the embedded migrations.
  Bundled in the same Docker image; in production you run it before
  starting (or restarting) `kuu-api`.
- **kuu-backup** — optional off-host Postgres dump+upload daemon. Built
  from `cmd/backup` into a separate image (`Dockerfile.backup`) and gated
  behind the `backup` docker-compose profile. See
  [docs/OPERATIONS.md](docs/OPERATIONS.md#backups).

## Design decisions

**Idempotent webhooks.** Stripe delivers events at least once: it retries on timeouts and errors, and can send the same event twice. Provisioning twice would mean two servers for one payment. So before doing any work, the handler claims the event ID with an `INSERT … ON CONFLICT DO NOTHING` into `webhook_events`. The primary key makes Postgres the arbiter, so exactly one concurrent delivery wins (a database test checks this with 20 parallel claims). If processing fails, the claim is deleted so Stripe's next retry can claim it and try again. A daily cleaner prunes claims older than 30 days, long after Stripe stops retrying.

**Compensating rollback instead of a transaction.** Provisioning touches three systems: Pterodactyl, Postgres and Cloudflare. A database transaction can't roll back a user created through someone else's API. Each step that creates something registers a deferred cleanup (delete the Pterodactyl user, the customer row, the subscription row, the server), and the cleanups are disarmed only once every step has succeeded. Cleanups run on a fresh context with its own 30-second timeout, because the request's context may already be cancelled by then. Pre-existing records, such as a returning customer's account, are never touched. DNS is deliberately outside the rollback: if the CNAME fails, the server still works by node address, so the failure is logged rather than undoing a working server.

**Least-loaded node selection and per-game ports.** Each provision queries every configured node in parallel (cached for 10 seconds to protect the panel under bursts). Nodes without enough free memory for the plan, or with no free allocations, are skipped; of the rest, the one with the most free memory wins, with free allocations as the tie-break. Each game has its own port range (Minecraft 25565–25700, Rust 28015–28200, FiveM 30120–30200, GMod 27015–27200), so players see familiar ports. The first free allocation in range is used. Two concurrent provisions can race for the same allocation; Pterodactyl rejects the loser with a 422, which is retried on a freshly picked node.

**Email outbox.** The welcome email is how the customer learns their server is ready. Sending inline from the webhook would tie provisioning success to SMTP availability. Instead the rendered email is written to `pending_emails`, and a worker polls the table, claims due rows with `FOR UPDATE SKIP LOCKED` (so several instances can run safely), and retries failures with quadratic backoff capped at an hour. Operators can list stuck emails and force a retry through `/admin/emails`.

**No passwords in email.** kuu-api used to generate the panel password and send it in the welcome email, which meant it sat in plaintext in `pending_emails` and in the customer's inbox. Now the panel account is created without a password, and Pterodactyl's own flow emails the customer a 60-minute "Setup Your Account" link; its "Forgot password" page covers expired links and later resets. kuu-api never sees a password, so the welcome email only carries server details, and returning customers get one for each new server too. The cost is a second email with panel branding, and if provisioning rolls back after the account was created, the customer may receive a setup link for an account that no longer exists (the retry sends a working one).

**Suspend, then reap.** A cancellation or failed payment suspends the server instead of deleting it. That gives a customer who cancelled by mistake, or whose card failed, two weeks to resubscribe with their world intact, or to ask support for their files (a suspended server is locked in the panel). Fourteen days is mid-range for game hosts (7–30 days is typical) and keeps node disks from filling with abandoned servers. After 14 days a daily reaper deletes the server from Pterodactyl, its DNS record and its database row. Customer and subscription rows and the panel account are kept, so billing history survives and a returning customer keeps their login.

**Trade-offs and what I'd change.**

- *Provisioning runs inside the webhook request.* It's simple and the result is visible in one log trace, but a slow panel makes Stripe wait. Stripe's retries plus the idempotency claim keep it correct. At larger scale I'd acknowledge the webhook immediately and provision from a job queue.
- *The first failed payment suspends the server.* Stripe retries failed payments for days, so a temporarily declined card shouldn't cost the customer their server. The fix is to suspend only when Stripe gives up (subscription `unpaid` or `canceled`) and unsuspend on recovery.
- *Configuration is code.* Games, variants and plans live in `internal/plans/plans.go`. That keeps the catalog type-checked and reviewed, but every new game needs a deploy.

## Games and variants

Each game has one or more variants the buyer picks at checkout. A variant
binds a Pterodactyl egg, docker image, and per-server env contract.
Currently exposed:

- **Minecraft** — `paper` (default), `forge`. Both accept a `version` (e.g.
  `1.21.1`); the env-var name differs per variant (`MINECRAFT_VERSION` vs
  `MC_VERSION`) and is held inside the variant config. Vanilla and
  Bungeecord eggs exist on the panel but are intentionally not exposed.
- **Rust** — `vanilla` (default), `oxide`, `carbon`. Selected via the
  `FRAMEWORK` env var. There is no user-selectable version. RCON password
  and `QUERY_PORT/RCON_PORT/APP_PORT` are derived per server at provision.
- **FiveM** — single `vanilla` variant. Accepts an optional `version`
  (e.g. `recommended` or a build like `6013-d8ae399d`) bound to
  `FIVEM_VERSION`. **Customer must replace the `FIVEM_LICENSE` placeholder
  in the panel before first start** — the egg requires a per-customer
  key from the cfx.re portal that we can't generate. txAdmin is disabled
  by default; flip `TXADMIN_ENABLE=1` in the panel to turn it on.
- **Garry's Mod** — single `vanilla` variant. Defaults to the `sandbox`
  gamemode on `gm_flatgrass`. No version field. The optional
  `STEAM_TOKEN` (32-char alphanumeric) is left empty so the server is
  unlisted by default — customer fills it in panel to appear in the
  public browser. Other gamemodes (TTT, DarkRP, etc.) are a one-line
  `GAMEMODE` change in the panel; promote them to first-class variants
  if there's demand.

Adding a new game or variant is a code change in
[internal/plans/plans.go](internal/plans/plans.go) — the table is the
source of truth for everything checkout, the webhook, and the
provisioner know about games.

## Repository layout

```text
cmd/
  server/         main.go for kuu-api
  migrate/        main.go for kuu-migrate
  backup/         main.go for kuu-backup (pg_dump + S3 upload daemon)
internal/
  config/         env parsing + validation
  database/       lib/pq + sqlx wrapper, models, migrations runner glue, daily cleaner
  dns/            Cloudflare client for per-server CNAME records
  email/          SMTP send + persistent retry queue + templating
  handlers/       HTTP handlers, middleware, rate limit, admin surface, health
  logging/        slog JSON setup + request-ID context plumbing
  metrics/        Prometheus instruments + DB pool + per-node samplers
  plans/          static price-id ↔ plan table; game + variant catalog
  provisioner/    Stripe → Pterodactyl → DB → DNS → email orchestration; reaper
  pterodactyl/    HTTP client for the panel application API
migrations/       embedded *.sql + embed.go (goose)
deploy/
  Caddyfile       reverse-proxy config used by docker-compose
  systemd/        unit files for the non-Docker deploy path
docs/
  API.md          frontend integration guide (request shapes, catalog, error model)
  DEPLOYMENT.md   OVH VPS step-by-step
  OPERATIONS.md   day-2 runbooks
docker-compose.yml   production compose stack (postgres + api + caddy)
Dockerfile           multi-stage; produces kuu-api + kuu-migrate on distroless
Dockerfile.backup    postgres:16-alpine base for kuu-backup (needs pg_dump)
Makefile             build, test, vet, fmt, lint, check
```

## Running locally

Requires Go 1.26+ and Docker. Provisioning also needs a Pterodactyl panel you can reach and Stripe test-mode keys.

```sh
# Postgres
docker run --rm -d --name kuu-pg -p 5432:5432 \
  -e POSTGRES_USER=kuu -e POSTGRES_PASSWORD=kuu -e POSTGRES_DB=kuu postgres:16-alpine

cp .env.example .env
$EDITOR .env             # DATABASE_URL=postgres://kuu:kuu@localhost:5432/kuu?sslmode=disable,
                         # Stripe test keys + price IDs, panel URL + key, NODE_IDS

go run ./cmd/migrate up  # apply migrations
go run ./cmd/server      # starts on :8081
```

In another terminal, forward Stripe test events to your local server:

```sh
stripe listen --forward-to localhost:8081/api/webhook
stripe trigger checkout.session.completed
```

SMTP and Cloudflare are optional: leave them empty and emails stay queued in Postgres, and subdomains are skipped.

## HTTP surface

| Method | Path                             | Auth          | Purpose |
| ------ | -------------------------------- | ------------- | ------- |
| POST   | `/api/checkout`                  | rate-limited  | Creates a Stripe Checkout Session |
| POST   | `/api/webhook`                   | Stripe sig    | Receives Stripe events (idempotent) |
| POST   | `/api/support/resend-welcome`    | rate-limited  | Re-sends the server details, with a link to the panel's password reset |
| POST   | `/api/support/manage`            | rate-limited  | Emails a Stripe billing-portal link |
| GET    | `/livez`                         | open          | Liveness — always 200 if process is up |
| GET    | `/readyz`                        | open          | Readiness — pings DB, panel, SMTP |
| GET    | `/health`                        | open          | Alias for `/livez` |
| GET    | `/metrics`                       | bearer (opt.) | Prometheus exposition |
| POST   | `/admin/emails/{id}/retry`       | bearer        | Force-retry a queued email |
| GET    | `/admin/emails`                  | bearer        | List pending email rows |
| GET    | `/admin/reconciliation`          | bearer        | DB ↔ Pterodactyl drift report |
| POST   | `/admin/test-email`              | bearer        | Send a test email through SMTP |

The support endpoints always answer 200 with the same message, so they don't reveal whether an email address has an account.

The bearer token for `/admin/*` and `/metrics` is `ADMIN_BEARER_TOKEN`. When
that env var is empty, `/admin/*` is not registered and `/metrics` is open.

## Environment variables

See [.env.example](.env.example) for the complete list with comments.
The required ones are:

- `STRIPE_SECRET_KEY`, `STRIPE_WEBHOOK_SECRET`
- `STRIPE_PRICE_2GB`…`STRIPE_PRICE_32GB` (per-plan price IDs)
- `PTERODACTYL_URL`, `PTERODACTYL_API_KEY`
- `NODE_IDS` — comma-separated pool of node IDs the provisioner may use
  (e.g. `1,2,3`). The legacy single-node `NODE_ID` is honored as a
  fallback when `NODE_IDS` is unset.
- `DATABASE_URL`

Recommended for production:

- `SMTP_HOST`, `SMTP_PORT`, `SMTP_USERNAME`, `SMTP_PASSWORD`, `SMTP_FROM`
- `CLOUDFLARE_API_TOKEN`, `CLOUDFLARE_ZONE_ID`, `CLOUDFLARE_DOMAIN` —
  enables subdomain CNAMEs. Leave the token empty to disable.
- `TRUSTED_PROXY_CIDRS` — anything fronted by a reverse proxy must set this
  or `clientIP()` will treat the proxy as the client.
- `ADMIN_BEARER_TOKEN` — `openssl rand -hex 32`. Gates `/admin/*` and
  `/metrics`.
- `CORS_ALLOWED_ORIGIN` — your frontend's origin. Defaults to
  `https://kuu.gg`.

## Testing

```sh
make check    # gofmt check, go vet, go test -race
make lint     # golangci-lint (config in .golangci.yml)
make build    # binaries in bin/
```

Unit tests need no external services: the provisioner and webhook handler are tested against in-memory fakes of Pterodactyl, Postgres, DNS and email. That includes a table-driven test that injects a failure at each provisioning step and checks that nothing is left behind. Checkout and billing-portal tests point stripe-go at a local fake server.

Database integration tests run against a real Postgres when `TEST_DATABASE_URL` points at a throwaway database, and skip otherwise:

```sh
docker run --rm -d --name kuu-testdb -p 5433:5432 -e POSTGRES_PASSWORD=test postgres:16-alpine
TEST_DATABASE_URL='postgres://postgres:test@localhost:5433/postgres?sslmode=disable' go test -race ./internal/database/
```

CI runs everything, including the database tests against a Postgres service container, on every pull request.

Coverage (from CI): handlers 83.4%, provisioner 82.1%, database 59.8%.

## Deploying

- **Docker Compose path (recommended for OVH VPS):**
  see [docs/DEPLOYMENT.md](docs/DEPLOYMENT.md).
- **systemd path (no Docker):** unit files in
  [deploy/systemd/](deploy/systemd/); install instructions inline.

## Operations

Day-2 procedures (tail logs, retry an email, run reconciliation, rotate the
admin token, restore from backup) live in
[docs/OPERATIONS.md](docs/OPERATIONS.md).

## License

Proprietary. © kuu.gg.
