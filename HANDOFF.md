# Handoff — pick up here

## Status

**Variant + version refactor: code complete, migration not yet applied, no smoke test yet.**

We were one command away from testing Paper + Rust end-to-end when you hit the usage limit.

## Resume in this exact order

```sh
cd /home/kentfarr/projects/kuu-api

# 1. Apply migration 004 (adds servers.variant + servers.version columns).
#    Postgres is still running in docker from yesterday; only sudo-docker works.
sudo docker compose run --rm migrate up
# Expect: applied 004_server_variant_and_version.sql in <duration>

# 2. Start kuu-api (foreground or background, your call).
go run ./cmd/server
# Or in background: go run ./cmd/server > /tmp/kuu-api.log 2>&1 &

# 3. (separate terminal) Forward Stripe webhooks.
stripe listen --forward-to localhost:8081/api/webhook
```

If postgres isn't running:
```sh
sudo docker compose up -d postgres
# Wait until: sudo docker compose ps shows healthy
```

## What to test next

### Test 1: Paper with explicit Minecraft version

```sh
curl -s -X POST http://localhost:8081/api/checkout \
  -H "Content-Type: application/json" \
  -d '{"game":"minecraft","variant":"paper","version":"1.21.1","price_id":"price_1TUrteL5ibjgKZejVswcDP1y"}'
```

Open the returned URL, fill `test@kuu.gg` + optional username + `4242 4242 4242 4242`, pay.

Verify in kuu-api logs:
- `variant: "paper"`, `version: "1.21.1"`
- `nest=1, egg=4` on the panel side
- Server row in DB has `variant=paper, version=1.21.1`

### Test 2: Rust with oxide framework

```sh
curl -s -X POST http://localhost:8081/api/checkout \
  -H "Content-Type: application/json" \
  -d '{"game":"rust","variant":"oxide","price_id":"price_1TUrteL5ibjgKZejVswcDP1y"}'
```

Verify:
- Server row has `variant=oxide, version=NULL` (Rust has no VersionField)
- Panel: `nest=4, egg=14`, server env has `FRAMEWORK=oxide` and `RCON_PASS=<32-char hex>`
- `QUERY_PORT/RCON_PORT/APP_PORT` are `allocPort+1/+2/+3`

### Test 3 (optional): Forge

```sh
curl -s -X POST http://localhost:8081/api/checkout \
  -H "Content-Type: application/json" \
  -d '{"game":"minecraft","variant":"forge","version":"1.21.1","price_id":"price_1TUrteL5ibjgKZejVswcDP1y"}'
```

`MC_VERSION=1.21.1`, egg 2.

### Cleanup after each test

DB: rerun the same probe pattern as yesterday (we deleted `/tmp/dbprobe`, you'd need to re-create it or just run a quick truncate).

Panel:
```sh
source /home/kentfarr/projects/kuu-api/.env
curl -s -X DELETE -H "Authorization: Bearer $PTERODACTYL_API_KEY" \
  -H "Accept: application/json" "$PTERODACTYL_URL/api/application/servers/<server_id>"
curl -s -X DELETE -H "Authorization: Bearer $PTERODACTYL_API_KEY" \
  -H "Accept: application/json" "$PTERODACTYL_URL/api/application/users/<user_id>"
```

Stripe: `echo yes | stripe subscriptions cancel <sub_id>`

## Code state

### What we built this session

| Layer | Files | What changed |
|---|---|---|
| Plans | `internal/plans/plans.go`, `_test.go` | Game now has `Variants` map + `DefaultVariant`. New `GameVariant` struct with `VersionField`, `VersionDefault`, `ConfigureEnv` hook. Helpers `ResolveVariant` and `BuildEnvironment` |
| Schema | `migrations/004_server_variant_and_version.sql` | Adds `servers.variant TEXT` + `servers.version TEXT`. **Not yet applied** |
| DB | `internal/database/servers.go` | `CreateServerParams` struct (replaces 8-arg positional), `Server` struct gains `Variant` + `Version` |
| Checkout | `internal/handlers/checkout.go` | Accepts `variant` + `version`, validates against `versionPattern`, propagates via Stripe metadata |
| Webhook | `internal/handlers/webhook.go` | Reads `variant` + `version` from metadata, calls `provisioner.Request{...}` instead of positional |
| Provisioner | `internal/provisioner/provisioner.go` | New `Request` struct. Resolves variant, calls `BuildEnvironment` per attempt (so Rust gets fresh RCON_PASS), persists variant + version via `CreateServerParams` |

### Earlier in the session (already deployed-ready)

- Username flow: user-picked at Stripe Checkout custom field, friendly random fallback (`brave-otter-4821`), SHA hash final fallback
- Multi-node Pterodactyl: `NodeIDs` config, `PickLeastLoadedNode` with 10s utilization cache, per-node health probe, `kuu_node_*` Prometheus gauges, `kuu_provision_node_chosen_total` counter
- Backup service: `cmd/backup` daemon, `Dockerfile.backup` (postgres:16-alpine base for pg_dump), gated behind `backup` docker-compose profile
- Stripe API version mismatch fix: `webhook.ConstructEventWithOptions{IgnoreAPIVersionMismatch: true}` in webhook.go

### Yesterday's E2E test (passed)

User picked `testprime` at checkout → webhook fired → real Pterodactyl user (id 3) + server (id 2) created on `panel.kuu.gg` → DB rows correct → cleanup chain verified by intentional first-attempt failure (wrong egg ID).

## Panel reference (panel.kuu.gg)

```
nest 1 (Minecraft):
  egg 1: Bungeecord       (NOT exposed)
  egg 2: Forge            → variant "forge"  (MC_VERSION)
  egg 3: Sponge           (NOT exposed)
  egg 4: Paper            → variant "paper"  (MINECRAFT_VERSION) ★ default
  egg 5: Vanilla          (intentionally NOT exposed)
nest 4 (Rust):
  egg 14: Rust            → variants "vanilla" ★ / "oxide" / "carbon" via FRAMEWORK env
nest 2,3: Source / Voice  (not used)
FiveM: NOT IMPORTED        → import from github.com/parkervcp/eggs first, then fill plans.go
```

To re-verify: `curl -H "Authorization: Bearer $PTERODACTYL_API_KEY" $PTERODACTYL_URL/api/application/nests/<id>/eggs?include=variables`

## After smoke test passes

1. `git status` should show ~15-20 changed files; review and commit
2. Push to remote, deploy to VPS via the existing docker-compose flow on the OVH host
3. Sign up for Backblaze B2 or Cloudflare R2, fill in `BACKUP_S3_*` in production `.env`, then `docker compose --profile backup up -d backup`

## Known issues we deferred (NOT blocking deploy)

- Provisioner treats every 422 as "allocation collided" — should distinguish validation errors via response body's `meta.rule`
- `cmd/migrate` reuses full `config.Load()` and demands Stripe + Pterodactyl env vars it doesn't need; should narrow to just `DATABASE_URL`
- `stripe-go v82` → v85 to support `dahlia` API version natively (currently using `IgnoreAPIVersionMismatch` band-aid)
- Rust QUERY_PORT/RCON_PORT/APP_PORT use port-offset trick (allocPort+1/+2/+3); not externally reachable unless operator adds additional Pterodactyl allocations manually

## Local-dev quirks worth remembering

- You're not in the `docker` group → every docker command needs `sudo`. Permanent fix when you want it: `sudo usermod -aG docker kentfarr && newgrp docker`
- An orphaned `kuu-postgres` container (separate from the docker-compose `kuu-api-postgres-1`) was holding port 5432 when we started. We `docker stop`ped + `docker rm`'d it. If 5432 is held again, check `sudo docker ps -a` for stragglers
- `.env` has `POSTGRES_PASSWORD=password`, `KUU_API_DOMAIN=localhost`, and `sslmode=disable` on `DATABASE_URL` — these are local-dev shims. Production needs different values
- `STRIPE_WEBHOOK_SECRET` in `.env` is the `stripe listen` secret, which (luckily) is stable per Stripe account, so it works for both local + production webhook receivers as long as the same account is used
