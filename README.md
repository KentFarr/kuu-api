# kuu-api

Go service that turns a Stripe Checkout payment into a running game server on
a Pterodactyl panel. The webhook handler is the entire product surface: a
successful `checkout.session.completed` event provisions a Pterodactyl user
(if needed), picks the least-loaded node from the configured pool, creates
a server using the buyer's chosen game + variant + version, persists the
customer ↔ subscription ↔ server mapping in Postgres, and queues a welcome
email with the generated SFTP credentials.

A separate daily reaper hard-deletes servers whose subscription has been
suspended for more than 14 days. Subscription / customer rows and the
Pterodactyl user account are preserved across that boundary.

> **New here?** Jump to [docs/DEPLOYMENT.md](docs/DEPLOYMENT.md) for a full
> step-by-step OVH VPS walkthrough. For day-2 ops see
> [docs/OPERATIONS.md](docs/OPERATIONS.md).

## How it fits together

```text
   Stripe Checkout
        │
        ▼  POST /api/webhook
  ┌───────────────────┐         ┌──────────────────┐
  │   kuu-api         │ ──────► │  Pterodactyl     │
  │  (Go, port 8081)  │         │  Application API │
  └─────────┬─────────┘         └──────────────────┘
            │                       (pool of nodes)
            │  pgx                       SMTP
            ▼                             ▲
       ┌────────┐                   ┌────────────┐
       │  Postgres │                │ Welcome    │
       │  (kuu DB) │ ◄─── queue ──► │ email      │
       └───────────┘   pending_     └────────────┘
                       emails
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
- **FiveM** — stub. The egg has not been imported on the panel yet, so
  the variant table reports `Configured() == false` and `/api/checkout`
  rejects requests for it.

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
  database/       pgx wrapper, models, migrations runner glue, daily cleaner
  email/          SMTP send + persistent retry queue + templating
  handlers/       HTTP handlers, middleware, rate limit, admin surface, health
  logging/        slog JSON setup + request-ID context plumbing
  metrics/        Prometheus instruments + DB pool + per-node samplers
  plans/          static price-id ↔ plan table; game + variant catalog
  provisioner/    Stripe → Pterodactyl → DB → email orchestration; reaper
  pterodactyl/    HTTP client for the panel application API
migrations/       embedded *.sql + embed.go (goose-compatible)
deploy/
  Caddyfile       reverse-proxy config used by docker-compose
  systemd/        unit files for the non-Docker deploy path
docs/
  DEPLOYMENT.md   OVH VPS step-by-step
  OPERATIONS.md   day-2 runbooks
docker-compose.yml   production compose stack (postgres + api + caddy)
Dockerfile           multi-stage; produces kuu-api + kuu-migrate on distroless
Dockerfile.backup    postgres:16-alpine base for kuu-backup (needs pg_dump)
```

## Quickstart (local dev)

Requires Go 1.26+, Postgres reachable on `localhost:5432`, and a Pterodactyl
panel you can hit (or stub).

```sh
cp .env.example .env
$EDITOR .env             # fill in Stripe test keys, panel URL+key, DB URL

go run ./cmd/migrate up  # apply migrations
go run ./cmd/server      # starts on :8081
```

In another terminal, forward Stripe events to your local server:

```sh
stripe listen --forward-to localhost:8081/api/webhook
stripe trigger checkout.session.completed
```

## HTTP surface

| Method | Path                         | Auth          | Purpose |
| ------ | ---------------------------- | ------------- | ------- |
| POST   | `/api/checkout`              | rate-limited  | Creates a Stripe Checkout Session |
| POST   | `/api/webhook`               | Stripe sig    | Receives Stripe events (idempotent) |
| GET    | `/livez`                     | open          | Liveness — always 200 if process is up |
| GET    | `/readyz`                    | open          | Readiness — pings DB, panel, SMTP |
| GET    | `/health`                    | open          | Alias for `/livez` |
| GET    | `/metrics`                   | bearer (opt.) | Prometheus exposition |
| POST   | `/admin/emails/{id}/retry`   | bearer        | Force-retry a queued welcome email |
| GET    | `/admin/emails`              | bearer        | List pending email rows |
| GET    | `/admin/reconciliation`      | bearer        | DB ↔ Pterodactyl drift report |

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
- `TRUSTED_PROXY_CIDRS` — anything fronted by a reverse proxy must set this
  or `clientIP()` will treat the proxy as the client.
- `ADMIN_BEARER_TOKEN` — `openssl rand -hex 32`. Gates `/admin/*` and
  `/metrics`.
- `CORS_ALLOWED_ORIGIN` — your frontend's origin. Defaults to
  `https://kuu.gg`.

## Build, test, deploy

```sh
go build ./...        # compile both binaries
go vet ./...          # static analysis
go test ./...         # unit tests (no external deps required)
gofmt -l .            # must print nothing
```

Production deploy:

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
