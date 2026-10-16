<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/images/banner-dark.png">
    <img src="docs/images/banner-light.png" alt="kuu-api: a Stripe payment in, a running game server out, with no human in the loop" width="100%">
  </picture>
</p>

<p align="center">
  <a href="https://github.com/KentFarr/kuu-api/actions/workflows/ci.yml"><img src="https://img.shields.io/github/actions/workflow/status/KentFarr/kuu-api/ci.yml?branch=main&style=flat-square&logo=githubactions&logoColor=white&label=CI" alt="CI status"></a>
  <img src="https://img.shields.io/github/go-mod/go-version/KentFarr/kuu-api?style=flat-square&logo=go&logoColor=white" alt="Go version">
  <img src="https://img.shields.io/badge/PostgreSQL-16-4169E1?style=flat-square&logo=postgresql&logoColor=white" alt="PostgreSQL 16">
  <img src="https://img.shields.io/badge/Stripe-Checkout_%2B_Billing-635BFF?style=flat-square&logo=stripe&logoColor=white" alt="Stripe Checkout and Billing">
  <img src="https://img.shields.io/badge/Prometheus-metrics-E6522C?style=flat-square&logo=prometheus&logoColor=white" alt="Prometheus metrics">
  <img src="https://img.shields.io/badge/Docker-distroless-2496ED?style=flat-square&logo=docker&logoColor=white" alt="Docker, distroless image">
</p>

<p align="center">
  <a href="#see-it-run">Demo</a> ·
  <a href="#engineering-highlights">Highlights</a> ·
  <a href="#architecture">Architecture</a> ·
  <a href="#design-decisions">Design decisions</a> ·
  <a href="#run-it-locally">Run it locally</a> ·
  <a href="docs/API.md">API guide</a>
</p>

**kuu-api** is the backend of [kuu.gg](https://kuu.gg), a subscription game-server host for Minecraft, Rust, FiveM and Garry's Mod. A customer configures a server on the storefront and pays with Stripe Checkout. kuu-api verifies Stripe's webhook, places the server on the least-loaded node of a [Pterodactyl](https://pterodactyl.io) panel, points a subdomain at it and emails the customer the details. After that, billing events suspend or restore the server, and a daily reaper deletes it two weeks after suspension, all without a person touching it.

> [!NOTE]
> kuu.gg ran in production in 2026. Its storefront is still online, but the game-server panel and nodes have been shut down, so it no longer provisions servers. This repository is the complete backend.

## See it run

<p align="center">
  <img src="docs/images/demo.gif" alt="Terminal recording: a signed checkout.session.completed event is delivered twice; kuu-api provisions one server and skips the duplicate" width="100%">
</p>

<p align="center"><sub>A replay of a real run of the local demo stack (the one <code>make demo</code> starts). A signed <code>checkout.session.completed</code> is delivered twice: kuu-api creates the panel user and the server once, skips the duplicate, and the outbox sends the welcome email on its next poll. Log lines are kuu-api's JSON, pretty-printed with selected fields; the pacing is slowed down for reading.</sub></p>

<table>
  <tr>
    <td width="50%" valign="top"><img src="docs/images/storefront.png" alt="The kuu.gg storefront: configuring a Minecraft Pro server with the Paper variant, version 1.21.11 and the subdomain blockparty.kuu.gg, above the plan tiers"></td>
    <td width="50%" valign="top"><img src="docs/images/email.png" alt="The welcome email from the local demo, viewed in Mailpit: server minecraft-8GB-1, connect to blockparty.kuu.gg:25565"></td>
  </tr>
  <tr>
    <td valign="top">The customer picks a game, variant, version, plan and subdomain on <a href="https://kuu.gg">kuu.gg</a>. Those fields are the <code>POST /api/checkout</code> request.</td>
    <td valign="top">The local demo provisioning that configuration: kuu-api's outbox emails the connection details, and the panel separately sends a link to set a password.</td>
  </tr>
</table>

## Engineering highlights

- **One payment, one server.** Stripe delivers webhooks at least once, so every event ID is claimed in Postgres before any work starts and a redelivered or concurrent duplicate can't provision a second server. A database test races 20 claims and expects exactly one winner. → [Idempotent webhooks](#idempotent-webhooks)
- **Rollback across three systems that share no transaction.** Each resource created on Pterodactyl, Postgres or Cloudflare registers a compensating cleanup that runs unless the whole provision succeeds. A table-driven test fails every step in turn and checks that nothing is left behind. → [Compensating rollback](#compensating-rollback-instead-of-a-transaction)
- **Capacity-aware placement.** Nodes are queried in parallel, the least-loaded one with room for the plan wins, and a lost race for a port is retried on a freshly picked node. → [Node selection](#least-loaded-node-selection-and-per-game-ports)
- **An outbox for email.** Emails are written to Postgres and sent by a background worker that claims rows with `FOR UPDATE SKIP LOCKED` and retries with backoff, so an SMTP outage never fails a provision. → [Email outbox](#email-outbox)
- **Billing that survives out-of-order events.** Failed payments get a grace period, every suspension records its reason, and each billing event re-reads the subscription's live status from Stripe instead of trusting the event. → [Grace period](#grace-period-for-failed-payments)
- **No customer passwords anywhere.** Panel accounts are created without one, and the panel emails its own set-password link. → [No passwords in email](#no-passwords-in-email)
- **Built to be operated.** Liveness and readiness probes, Prometheus metrics, JSON logs with request IDs that follow a rollback, a panel-versus-database drift report, off-host backups and graceful shutdown. → [Observability](#observability-and-operations)
- **A demo that CI runs.** `make demo` starts the real binary against an in-memory fake panel and a mail catcher. CI runs it on every pull request and checks that a payment delivered twice creates exactly one server. → [Run it locally](#run-it-locally)

## Architecture

<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/images/architecture-dark.png">
    <img src="docs/images/architecture-light.png" alt="Architecture: the kuu.gg storefront and Stripe talk to kuu-api, whose HTTP handlers, provisioner and background workers use the Pterodactyl panel, PostgreSQL, Cloudflare DNS and SMTP; Prometheus scrapes its metrics" width="100%">
  </picture>
</p>

Three binaries are built from this repo:

- **kuu-api**: the HTTP server, plus its background workers: the email outbox, a daily idempotency-key cleaner, a daily suspended-server reaper, and Prometheus samplers for the DB pool and node capacity.
- **kuu-migrate**: a one-shot binary that applies the embedded migrations. It ships in the same distroless image; run it before starting or restarting `kuu-api`.
- **kuu-backup**: an optional daemon that runs `pg_dump` on a schedule and uploads the result to S3-compatible storage. It has its own image (`Dockerfile.backup`) and sits behind the `backup` Compose profile. See [docs/OPERATIONS.md](docs/OPERATIONS.md#backups).

## What happens when someone pays

```mermaid
sequenceDiagram
    autonumber
    actor C as Customer
    participant FE as kuu.gg
    participant S as Stripe
    participant API as kuu-api
    participant DB as Postgres
    participant P as Pterodactyl
    participant CF as Cloudflare

    C->>FE: configure server
    FE->>API: POST /api/checkout
    API->>S: create Checkout Session
    API-->>FE: session URL
    C->>S: pay
    S->>API: checkout.session.completed (signed)
    API->>API: verify signature
    API->>DB: claim event ID
    break duplicate delivery
        API-->>S: 200, skip
    end
    API->>P: find or create user
    API->>DB: customer + subscription rows
    API->>P: pick node + port, create server
    API->>DB: server row
    API->>CF: CNAME (best effort)
    API->>DB: queue welcome email
    API-->>S: 200 OK
    Note over API,CF: A failure before the DNS step undoes what was created, releases the claim and returns 500, so Stripe retries
```

<details>
<summary><b>Step by step, including the failure handling</b></summary>

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

</details>

## After the first payment

```mermaid
stateDiagram-v2
    state "active" as active
    state "past_due" as past_due
    state "suspended (non_payment)" as unpaid
    state "suspended (cancelled)" as cancelled
    state "server deleted" as reaped

    [*] --> active: checkout.session.completed
    active --> past_due: invoice.payment_failed
    past_due --> active: invoice.paid
    past_due --> unpaid: retries exhausted
    unpaid --> active: invoice.paid
    active --> cancelled: subscription cancelled
    past_due --> cancelled: subscription cancelled
    unpaid --> reaped: 14 days later
    cancelled --> reaped: 14 days later
    reaped --> [*]
```

A failed renewal doesn't touch the server while Stripe retries the card; the customer gets an email with a link to pay. The server is suspended when Stripe gives up or the customer cancels, and restored automatically if the unpaid invoice is settled. A daily reaper deletes servers that have been suspended for more than 14 days, but keeps the customer, the billing history and the panel login.

## Design decisions

### Idempotent webhooks

Stripe delivers events at least once: it retries on timeouts and errors, and can send the same event twice. Provisioning twice would mean two servers for one payment. So before doing any work, the handler claims the event ID with an `INSERT … ON CONFLICT DO NOTHING` into `webhook_events`. The primary key makes Postgres the arbiter, so exactly one concurrent delivery wins (a database test checks this with 20 parallel claims). If processing fails, the claim is deleted so Stripe's next retry can claim it and try again. A daily cleaner prunes claims older than 30 days, long after Stripe stops retrying.

<sub>**Code:** [`handlers/webhook.go`](internal/handlers/webhook.go) · [`database/webhook_events.go`](internal/database/webhook_events.go) · [`database/cleaner.go`](internal/database/cleaner.go)</sub>

### Compensating rollback instead of a transaction

Provisioning touches three systems: Pterodactyl, Postgres and Cloudflare. A database transaction can't roll back a user created through someone else's API. Each step that creates something registers a deferred cleanup (delete the Pterodactyl user, the customer row, the subscription row, the server), and the cleanups are disarmed only once every step has succeeded. Cleanups run on a fresh context with its own 30-second timeout, because the request's context may already be cancelled by then. Pre-existing records, such as a returning customer's account, are never touched. DNS is deliberately outside the rollback: if the CNAME fails, the server still works by node address, so the failure is logged rather than undoing a working server.

<sub>**Code:** [`provisioner/provisioner.go`](internal/provisioner/provisioner.go) · [`provisioner/provisioner_test.go`](internal/provisioner/provisioner_test.go)</sub>

### Least-loaded node selection and per-game ports

Each provision queries every configured node in parallel (cached for 10 seconds to protect the panel under bursts). Nodes without enough free memory for the plan, or with no free allocations, are skipped; of the rest, the one with the most free memory wins, with free allocations as the tie-break. Each game has its own port range (Minecraft 25565–25700, Rust 28015–28200, FiveM 30120–30200, GMod 27015–27200), so players see familiar ports. The first free allocation in range is used. Two concurrent provisions can race for the same allocation; Pterodactyl rejects the loser with a 422, which is retried on a freshly picked node.

<sub>**Code:** [`pterodactyl/nodes.go`](internal/pterodactyl/nodes.go) · [`pterodactyl/allocations.go`](internal/pterodactyl/allocations.go) · `createServerWithRetry` in [`provisioner/provisioner.go`](internal/provisioner/provisioner.go)</sub>

### Email outbox

The welcome email is how the customer learns their server is ready. Sending inline from the webhook would tie provisioning success to SMTP availability. Instead the rendered email is written to `pending_emails`, and a worker polls the table, claims due rows with `FOR UPDATE SKIP LOCKED` (so several instances can run safely), and retries failures with quadratic backoff capped at an hour. Operators can list stuck emails and force a retry through `/admin/emails`.

<sub>**Code:** [`email/queue.go`](internal/email/queue.go) · [`database/pending_emails.go`](internal/database/pending_emails.go)</sub>

### No passwords in email

kuu-api used to generate the panel password and send it in the welcome email, which meant it sat in plaintext in `pending_emails` and in the customer's inbox. Now the panel account is created without a password, and Pterodactyl's own flow emails the customer a 60-minute "Setup Your Account" link; its "Forgot password" page covers expired links and later resets. kuu-api never sees a password, so the welcome email only carries server details, and returning customers get one for each new server too. The cost is a second email with panel branding, and if provisioning rolls back after the account was created, the customer may receive a setup link for an account that no longer exists (the retry sends a working one).

<sub>**Code:** [`pterodactyl/users.go`](internal/pterodactyl/users.go) · [`email/email.go`](internal/email/email.go)</sub>

### Grace period for failed payments

Stripe retries a failed renewal for days (Smart Retries), and most declines are temporary, so `invoice.payment_failed` only marks the subscription `past_due` and emails the customer a link to Stripe's hosted invoice page, where they can pay with another card. That link stays valid for weeks; a billing-portal session link would expire long before the customer opened the email. The server is suspended only when Stripe gives up (`customer.subscription.updated` with status `unpaid` or `canceled`) or the subscription is deleted. Each suspension records a reason, so `invoice.paid` restores a server suspended for non-payment but never one the customer cancelled. A second suspension keeps the original timestamp, so it can't extend the 14-day grace period. Stripe doesn't guarantee event order, so for every billing event the handler fetches the subscription's current status from Stripe and acts on that rather than on the event's snapshot; a delayed `unpaid` arriving after `invoice.paid` can't re-suspend a paid-up server.

<sub>**Code:** [`provisioner/billing.go`](internal/provisioner/billing.go) · [`handlers/webhook.go`](internal/handlers/webhook.go) · [`handlers/subscription_status.go`](internal/handlers/subscription_status.go)</sub>

### Signed links instead of expiring ones

Stripe billing-portal sessions are short-lived, so emailing one would often give the customer a dead link. `/api/support/manage` instead emails a link back to kuu-api carrying the Stripe customer ID and an expiry, signed with HMAC-SHA256 and valid for 24 hours. Clicking it creates a fresh portal session and redirects. The link is built from the configured `API_PUBLIC_URL`, never the request's `Host` header, so nobody can get a phishing link sent from Kuu's address.

<sub>**Code:** [`handlers/portal_link.go`](internal/handlers/portal_link.go) · [`handlers/support.go`](internal/handlers/support.go)</sub>

### Suspend, then reap

A cancellation or exhausted payment retries suspend the server instead of deleting it. That gives a customer who cancelled by mistake, or whose card failed, two weeks to resubscribe with their world intact, or to ask support for their files (a suspended server is locked in the panel). Fourteen days is mid-range for game hosts (7–30 days is typical) and keeps node disks from filling with abandoned servers. After 14 days a daily reaper deletes the server from Pterodactyl, its DNS record and its database row. Customer and subscription rows and the panel account are kept, so billing history survives and a returning customer keeps their login.

<sub>**Code:** [`provisioner/reaper.go`](internal/provisioner/reaper.go)</sub>

### Trade-offs and what I'd change

- **Provisioning runs inside the webhook request.** It's simple and the result is visible in one log trace, but a slow panel makes Stripe wait. Stripe's retries plus the idempotency claim keep it correct. At larger scale I'd acknowledge the webhook immediately and provision from a job queue.
- **One Stripe API call per billing event.** Fetching the live subscription status protects against reordered events, but every `invoice.paid` (including each normal monthly renewal) costs a Stripe request, and a Stripe outage makes those events fail and retry.
- **Configuration is code.** Games, variants and plans live in `internal/plans/plans.go`. That keeps the catalog type-checked and reviewed, but every new game needs a deploy.
- **A crash mid-provision isn't retried.** The event claim is released only when processing returns an error. If the process dies partway through, the claim survives, Stripe's retry is skipped as a duplicate, and whatever was already created stays behind for an operator to clean up (`/admin/reconciliation` lists servers the panel has but the database doesn't). A claim with a lease, reclaimable once it times out, would let the retry finish the job.

## Testing

```sh
make check    # gofmt check, go vet, go test -race
make lint     # golangci-lint (config in .golangci.yml)
make build    # binaries in bin/
```

Unit tests need no external services: the provisioner and webhook handler are tested against in-memory fakes of Pterodactyl, Postgres, DNS and email. That includes a table-driven test that injects a failure at each provisioning step:

```go
// internal/provisioner/provisioner_test.go (excerpt)
cases := []struct {
    name   string
    inject func(ptero *fakePtero, store *fakeStore)
}{
    {
        // The last step, so every earlier cleanup must run.
        name:   "server row insert fails",
        inject: func(_ *fakePtero, store *fakeStore) { store.fail["CreateServer"] = true },
    },
    {
        name:   "pterodactyl user creation fails",
        inject: func(ptero *fakePtero, _ *fakeStore) { ptero.fail["CreateUser"] = true },
    },
    // …plus customer insert, subscription insert, no node capacity,
    // no free allocation and panel server creation.
}
```

Every case then checks that no panel users or servers, no customer, subscription or server rows, no DNS records and no emails are left behind.

Checkout and billing-portal tests point stripe-go at a local fake server. Database integration tests run against a real Postgres when `TEST_DATABASE_URL` points at a throwaway database, and skip otherwise:

```sh
docker run --rm -d --name kuu-testdb -p 5433:5432 -e POSTGRES_PASSWORD=test postgres:16-alpine
TEST_DATABASE_URL='postgres://postgres:test@localhost:5433/postgres?sslmode=disable' go test -race ./internal/database/
```

CI runs three jobs on every pull request: build and test (with the database tests against a Postgres service container), golangci-lint, and the demo smoke test described [below](#run-it-locally).

Coverage (from CI): handlers 84.8%, provisioner 82.9%, database 61.0%.

## Observability and operations

<p align="center">
  <img src="docs/images/ops.png" alt="Terminal: the readiness probe, provisioning metrics and the reconciliation report from the local demo" width="100%">
</p>

- **Health.** `/livez` answers while the process is up; `/readyz` checks Postgres, the panel, SMTP and every node. The distroless image has no shell or curl, so the Compose healthcheck runs `kuu-api -healthcheck`, which probes `/livez` itself.
- **Metrics.** Prometheus counters and histograms for webhook outcomes, end-to-end provisioning time, panel latency, email sends, node memory and free ports, DB pool use and the rate limiter.
- **Logs.** JSON through `log/slog`, tagged with a request ID that also follows a provision into its deferred rollbacks.
- **Admin API.** List and retry stuck emails, send a test email, and diff the panel against Postgres to find orphaned or dangling servers (`/admin/reconciliation`).
- **Lifecycle.** Off-host backups with retention (`kuu-backup`), a daily reaper, and graceful shutdown that drains HTTP connections for up to 30 seconds and waits for the workers.

Runbooks for tailing logs, retrying an email, reconciling, rotating the admin token and restoring a backup are in [docs/OPERATIONS.md](docs/OPERATIONS.md).

## Run it locally

`make demo` runs the full payment → server flow on your machine with no Stripe, Pterodactyl, Cloudflare or SMTP account. It starts Postgres, [Mailpit](https://mailpit.axllent.org/) (catches email), `fakeptero` (an in-memory stand-in for the Pterodactyl API) and kuu-api in Docker. Requires Docker and Go.

```sh
make demo            # build, migrate, start; waits until kuu-api answers
make demo-checkout   # "pay": send kuu-api a signed checkout.session.completed
make demo-down       # stop and delete everything
```

(If Docker needs root on your machine: `make demo DOCKER="sudo docker"`, and the same for `demo-down`.)

What to look at:

- **Logs** (`make demo-logs`): kuu-api claims the event, creates a panel user, picks the node, creates the server and queues the welcome email. fakeptero logs each API call it receives.
- **Email** at <http://localhost:8025>: the panel's "Account Created" set-password email arrives at once, and kuu-api's "Your Kuu server is ready" email within 30 seconds (the queue worker's poll interval).
- **Idempotency**: `make demo-checkout ARGS="-repeat"` delivers the same event twice. The second gets a 200 without provisioning anything (`event already processed, skipping` in the logs).
- **Other options**: `ARGS="-game rust -plan 16GB -email you@example.com -username you"`.
- **Metrics** at <http://localhost:8081/metrics> and the drift report at `/admin/reconciliation`, both with `Authorization: Bearer demo-admin-token`.

`demo-checkout` signs events with the demo webhook secret from [.env.demo](.env.demo), exactly as Stripe signs them, so the real signature check runs. CI runs this same demo on every pull request.

<details>
<summary><b>Against real Stripe and Pterodactyl</b></summary>

Requires a Pterodactyl panel, Stripe test-mode keys and test price IDs, and Postgres.

```sh
cp .env.example .env
$EDITOR .env             # DATABASE_URL, Stripe test keys + price IDs, panel URL + key, NODE_IDS
go run ./cmd/migrate up
go run ./cmd/server      # starts on :8081

# in another terminal: forward Stripe test events to the local server
stripe listen --forward-to localhost:8081/api/webhook
```

Then create a checkout with `POST /api/checkout` (see [docs/API.md](docs/API.md)) and pay on the returned page with Stripe's test card `4242 4242 4242 4242`. (`stripe trigger checkout.session.completed` won't work here: its generic session lacks the game and price metadata kuu-api provisions from.)

SMTP and Cloudflare are optional: leave them empty and emails stay queued in Postgres, and subdomains are skipped.

</details>

## Tech stack

| Area          | What's used                                                                                                  |
| ------------- | ------------------------------------------------------------------------------------------------------------ |
| Language      | Go, standard-library `net/http` routing, `log/slog`                                                          |
| Data          | PostgreSQL 16 through `sqlx` and `lib/pq`; `goose` migrations embedded in the binary                         |
| Payments      | Stripe Checkout and Billing (`stripe-go`), signed webhooks                                                   |
| Game servers  | Pterodactyl Application API: users, nodes, allocations, servers                                              |
| DNS and email | Cloudflare API for subdomains; SMTP through a Postgres outbox                                                |
| Observability | Prometheus `client_golang`, JSON logs with request IDs                                                       |
| Delivery      | Multi-stage Docker build on distroless, Docker Compose with Caddy, or systemd                                |
| Backups       | `pg_dump` to S3-compatible storage with `minio-go`                                                           |
| CI            | GitHub Actions (gofmt, vet, race-enabled tests with Postgres, golangci-lint, demo smoke test) and Dependabot |

## Reference

### Games and variants

| Game        | Variants (default first)     | Ports       | Notes                                                          |
| ----------- | ---------------------------- | ----------- | -------------------------------------------------------------- |
| Minecraft   | `paper`, `forge`             | 25565–25700 | Takes a `version` such as `1.21.1`                             |
| Rust        | `vanilla`, `oxide`, `carbon` | 28015–28200 | RCON password and extra ports derived per server               |
| FiveM       | `vanilla`                    | 30120–30200 | Customer adds their cfx.re license key in the panel            |
| Garry's Mod | `vanilla`                    | 27015–27200 | Sandbox on `gm_flatgrass`; unlisted until a Steam token is set |

<details>
<summary><b>Per-game details</b></summary>

Each game has one or more variants the buyer picks at checkout. A variant binds a Pterodactyl egg, Docker image, and per-server environment contract.

- **Minecraft**: `paper` (default), `forge`. Both accept a `version` (e.g. `1.21.1`); the env-var name differs per variant (`MINECRAFT_VERSION` vs `MC_VERSION`) and is held inside the variant config. Vanilla and Bungeecord eggs exist on the panel but are intentionally not exposed.
- **Rust**: `vanilla` (default), `oxide`, `carbon`. Selected via the `FRAMEWORK` env var. There is no user-selectable version. RCON password and `QUERY_PORT/RCON_PORT/APP_PORT` are derived per server at provision.
- **FiveM**: single `vanilla` variant. Accepts an optional `version` (e.g. `recommended` or a build like `6013-d8ae399d`) bound to `FIVEM_VERSION`. **Customer must replace the `FIVEM_LICENSE` placeholder in the panel before first start**: the egg requires a per-customer key from the cfx.re portal that we can't generate. txAdmin is disabled by default; flip `TXADMIN_ENABLE=1` in the panel to turn it on.
- **Garry's Mod**: single `vanilla` variant. Defaults to the `sandbox` gamemode on `gm_flatgrass`. No version field. The optional `STEAM_TOKEN` (32-char alphanumeric) is left empty so the server is unlisted by default; the customer fills it in the panel to appear in the public browser. Other gamemodes (TTT, DarkRP, etc.) are a one-line `GAMEMODE` change in the panel; promote them to first-class variants if there's demand.

Adding a new game or variant is a code change in [internal/plans/plans.go](internal/plans/plans.go). The table is the source of truth for everything checkout, the webhook, and the provisioner know about games.

</details>

### HTTP surface

<details>
<summary><b>All endpoints</b></summary>

| Method | Path                          | Auth          | Purpose                                                                |
| ------ | ----------------------------- | ------------- | ---------------------------------------------------------------------- |
| POST   | `/api/checkout`               | rate-limited  | Creates a Stripe Checkout Session                                      |
| POST   | `/api/webhook`                | Stripe sig    | Receives Stripe events (idempotent)                                    |
| POST   | `/api/support/resend-welcome` | rate-limited  | Re-sends the server details, with a link to the panel's password reset |
| POST   | `/api/support/manage`         | rate-limited  | Emails a signed, 24-hour link to the Stripe billing portal             |
| GET    | `/api/support/portal`         | signed link   | Checks the emailed link and redirects to a fresh portal session        |
| GET    | `/livez`                      | open          | Liveness: always 200 if the process is up                              |
| GET    | `/readyz`                     | open          | Readiness: pings DB, panel, SMTP                                       |
| GET    | `/health`                     | open          | Alias for `/livez`                                                     |
| GET    | `/metrics`                    | bearer (opt.) | Prometheus exposition                                                  |
| POST   | `/admin/emails/{id}/retry`    | bearer        | Force-retry a queued email                                             |
| GET    | `/admin/emails`               | bearer        | List pending email rows                                                |
| GET    | `/admin/reconciliation`       | bearer        | DB ↔ Pterodactyl drift report                                          |
| POST   | `/admin/test-email`           | bearer        | Send a test email through SMTP                                         |

The support endpoints always answer 200 with the same message, so they don't reveal whether an email address has an account.

The bearer token for `/admin/*` and `/metrics` is `ADMIN_BEARER_TOKEN`. When that env var is empty, `/admin/*` is not registered and `/metrics` is open.

The frontend integration guide, with request shapes, the catalog and the error model, is [docs/API.md](docs/API.md).

</details>

### Environment variables

<details>
<summary><b>Required and recommended settings</b></summary>

See [.env.example](.env.example) for the complete list with comments. The required ones are:

- `STRIPE_SECRET_KEY`, `STRIPE_WEBHOOK_SECRET`
- `STRIPE_PRICE_2GB`…`STRIPE_PRICE_32GB` (per-plan price IDs)
- `PTERODACTYL_URL`, `PTERODACTYL_API_KEY`
- `NODE_IDS`: comma-separated pool of node IDs the provisioner may use (e.g. `1,2,3`). The legacy single-node `NODE_ID` is honored as a fallback when `NODE_IDS` is unset.
- `DATABASE_URL`

Recommended for production:

- `SMTP_HOST`, `SMTP_PORT`, `SMTP_USERNAME`, `SMTP_PASSWORD`, `SMTP_FROM`
- `CLOUDFLARE_API_TOKEN`, `CLOUDFLARE_ZONE_ID`, `CLOUDFLARE_DOMAIN`: enables subdomain CNAMEs. Leave the token empty to disable.
- `TRUSTED_PROXY_CIDRS`: anything fronted by a reverse proxy must set this or `clientIP()` will treat the proxy as the client.
- `ADMIN_BEARER_TOKEN`: `openssl rand -hex 32`. Gates `/admin/*` and `/metrics`.
- `CORS_ALLOWED_ORIGIN`: your frontend's origin. Defaults to `https://kuu.gg`.
- `API_PUBLIC_URL`: this API's public URL, used in emailed links. Defaults to `https://api.kuu.gg`.
- `PORTAL_LINK_SECRET`: `openssl rand -hex 32`. Signs the emailed billing links; if unset, a random key is used and links break on restart.

</details>

### Repository layout

<details>
<summary><b>Directory tree</b></summary>

```text
cmd/
  server/         main.go for kuu-api
  migrate/        main.go for kuu-migrate
  backup/         main.go for kuu-backup (pg_dump + S3 upload daemon)
  fakeptero/      in-memory fake Pterodactyl API for the local demo
  demo-checkout/  sends a signed checkout.session.completed to the local demo
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
  images/         README images
docker-compose.yml   production compose stack (postgres + api + caddy)
Dockerfile           multi-stage; produces kuu-api + kuu-migrate on distroless
Dockerfile.backup    postgres:16-alpine base for kuu-backup (needs pg_dump)
Makefile             build, test, vet, fmt, lint, check, demo
docker-compose.demo.yml + .env.demo + Dockerfile.fakeptero   the local demo (demo values only)
```

</details>

## Deploying

- **Docker Compose (recommended for an OVH VPS):** see [docs/DEPLOYMENT.md](docs/DEPLOYMENT.md).
- **systemd (no Docker):** unit files in [deploy/systemd/](deploy/systemd/); install instructions inline.

## License

Proprietary. © kuu.gg. Built by [@KentFarr](https://github.com/KentFarr).
