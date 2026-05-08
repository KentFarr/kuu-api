# Deploying kuu-api on an OVH VPS

This is the canonical, opinionated walkthrough for getting kuu-api running
on a fresh OVH VPS. The end state is:

- A single VPS running Docker Compose
- Three long-running containers: `postgres`, `kuu-api`, `caddy`
- One on-demand container: `migrate` (run by hand or by deploy script)
- Caddy terminates TLS for `api.your-domain.tld` using Let's Encrypt
- `kuu-api` is **not** exposed to the public internet directly — only Caddy
  (80/443) and SSH (22) are. Postgres binds to `127.0.0.1` only.

If you prefer a non-Docker deploy with systemd, jump to
[Appendix A — systemd alternative](#appendix-a--systemd-alternative).

---

## 0. What you need before you start

- An OVH VPS (Ubuntu 24.04 LTS recommended). Even the smallest plan is
  enough to host kuu-api itself; size up if Postgres will host real
  customer data.
- Root or sudo access on the VPS.
- A registered domain. We'll use `api.kuu.gg` in examples — substitute
  your real subdomain.
- **A Pterodactyl panel** already running somewhere reachable, with:
  - An [Application API key](https://pterodactyl.io/community/config/api_keys.html)
    that has `users` and `servers` read+write.
  - At least one configured **Node**, **Allocation pool**, **Nest**, and
    **Egg** for each game you sell. Note the `node_id` and the egg/nest
    IDs — kuu-api maps plans → eggs in `internal/plans/plans.go`, so any
    new game/egg requires a code change.
- **A Stripe account**:
  - Test mode keys initially (`sk_test_…`).
  - One Price for each plan size (2 GB → 32 GB). Copy the `price_…` IDs.
  - A webhook endpoint added once the public URL is live (we do this
    near the end).
- **An SMTP relay**. SES, Postmark, Mailgun, Fastmail SMTP — anything that
  speaks SMTP+STARTTLS on 587. Welcome emails contain credentials, so
  don't pick a sketchy provider.

You will also want the GitHub repo cloned locally so you can edit `.env`
and the `Caddyfile` from your laptop and `scp` them up — or `git clone` on
the VPS itself.

---

## 1. Prepare the VPS

Connect over SSH:

```sh
ssh root@vps-host.ovh.net
```

Update and create a non-root deploy user:

```sh
apt update && apt -y full-upgrade
adduser --disabled-password --gecos "" deploy
usermod -aG sudo deploy

# copy your SSH key so you can stop using root
mkdir -p /home/deploy/.ssh
cp ~/.ssh/authorized_keys /home/deploy/.ssh/
chown -R deploy:deploy /home/deploy/.ssh
chmod 700 /home/deploy/.ssh
chmod 600 /home/deploy/.ssh/authorized_keys
```

Lock the firewall down to 22/80/443. OVH's default firewall is permissive;
configure it from the OVH control panel **and** install ufw locally so the
host has its own defence:

```sh
apt -y install ufw
ufw default deny incoming
ufw default allow outgoing
ufw allow OpenSSH
ufw allow 80/tcp
ufw allow 443/tcp
ufw enable
```

Disable root SSH and password auth:

```sh
sed -i 's/^#*PermitRootLogin.*/PermitRootLogin no/' /etc/ssh/sshd_config
sed -i 's/^#*PasswordAuthentication.*/PasswordAuthentication no/' /etc/ssh/sshd_config
systemctl reload ssh
```

Reconnect as `deploy` from now on:

```sh
ssh deploy@vps-host.ovh.net
```

---

## 2. Install Docker

Use the official Docker convenience script — the Ubuntu repo version is
usually too old:

```sh
curl -fsSL https://get.docker.com | sudo sh
sudo usermod -aG docker deploy
exit   # log out and back in so the group takes effect
```

Reconnect, then verify:

```sh
docker --version
docker compose version
```

You should see Docker Engine ≥ 27 and Compose ≥ 2.30.

---

## 3. Point DNS at the VPS

In your DNS provider, create an `A` record:

```
api.kuu.gg.   A   <your VPS public IPv4>
```

Wait until `dig +short api.kuu.gg` returns the right IP from the VPS
itself. Caddy needs DNS to resolve before it can finish the ACME
challenge — if you skip ahead, the first start will hammer Let's Encrypt
and may rate-limit you.

---

## 4. Get the code on the VPS

```sh
sudo mkdir -p /srv && sudo chown deploy:deploy /srv
cd /srv
git clone https://github.com/KentFarr/kuu-api.git
cd kuu-api
```

(Or `scp` the working tree up if the repo isn't public.)

---

## 5. Configure secrets

Copy the example file and fill it in. **Never commit this**:

```sh
cp .env.example .env
chmod 600 .env
$EDITOR .env
```

Required values for an OVH compose deploy:

```dotenv
PORT=8081

STRIPE_SECRET_KEY=sk_test_...
STRIPE_WEBHOOK_SECRET=                 # filled in step 9
STRIPE_PRICE_2GB=price_...
STRIPE_PRICE_4GB=price_...
STRIPE_PRICE_8GB=price_...
STRIPE_PRICE_16GB=price_...
STRIPE_PRICE_24GB=price_...
STRIPE_PRICE_32GB=price_...

PTERODACTYL_URL=https://panel.your-domain.com
PTERODACTYL_API_KEY=ptlc_...
NODE_ID=1                              # numeric panel node ID

# Compose injects DATABASE_URL pointing at the postgres service — the
# value here is only used by `docker compose run --rm migrate up` if you
# ever want to override it. Leave the example placeholder.
DATABASE_URL=postgres://kuu:replaced-by-compose@postgres:5432/kuu?sslmode=disable

SMTP_HOST=smtp.your-relay.com
SMTP_PORT=587
SMTP_USERNAME=...
SMTP_PASSWORD=...
SMTP_FROM=noreply@kuu.gg

CHECKOUT_SUCCESS_URL=https://kuu.gg/success
CHECKOUT_CANCEL_URL=https://kuu.gg
CORS_ALLOWED_ORIGIN=https://kuu.gg

# Caddy → kuu-api hops through the docker bridge, which lives in this CIDR.
TRUSTED_PROXY_CIDRS=172.16.0.0/12

ADMIN_BEARER_TOKEN=                    # generate in step 6
```

Two more values are read directly by `docker-compose.yml` (not from `.env`)
because they're used by `postgres` and `caddy` themselves. Add them to
your shell profile or a separate file `docker compose --env-file` reads:

```sh
# /srv/kuu-api/.env (same file is fine — Compose reads it for interpolation)
KUU_API_DOMAIN=api.kuu.gg
POSTGRES_PASSWORD=$(openssl rand -hex 24)   # paste the literal hex string
```

Re-run `chmod 600 .env` after editing.

---

## 6. Generate the admin bearer token

```sh
openssl rand -hex 32
```

Paste it into `.env` as `ADMIN_BEARER_TOKEN=…`. Save the same value in
your password manager — there's no recovery if you lose it; it's just
rotated by editing the file and restarting.

---

## 7. Run the database migrations

Compose's `migrate` service has `profiles: ["tools"]` so it doesn't run on
`up -d`. Trigger it explicitly:

```sh
docker compose run --rm migrate up
```

You should see goose print one line per applied migration. The `postgres`
service is started as a dependency and stays up after — that's expected.

Verify:

```sh
docker compose run --rm migrate status
```

All three migrations (`001_initial`, `002_idempotency_and_indexes`,
`003_pending_emails`) should report as applied.

---

## 8. Bring up the stack

```sh
docker compose up -d
docker compose ps
```

Expected: `postgres`, `kuu-api`, `caddy` all `healthy` (or `running` for
caddy, which has no healthcheck).

Watch Caddy obtain a certificate:

```sh
docker compose logs -f caddy
```

You're looking for a line like
`certificate obtained successfully ... identifier: api.kuu.gg`. If you see
ACME 4xx errors, DNS hasn't propagated or port 80/443 isn't open — fix
those before retrying or you will burn through Let's Encrypt's rate limit.

Smoke test from your laptop:

```sh
curl https://api.kuu.gg/livez   # → "ok"
curl https://api.kuu.gg/readyz  # → JSON, "ok":true
```

If `readyz` returns 503, check the JSON body — it tells you which probe
failed (DB / Pterodactyl / SMTP).

---

## 9. Configure the Stripe webhook

In the Stripe dashboard → Developers → Webhooks → **Add endpoint**:

- URL: `https://api.kuu.gg/api/webhook`
- Events: at minimum `checkout.session.completed`,
  `customer.subscription.deleted`, `customer.subscription.updated`,
  `invoice.payment_failed` (mirror what the handler watches for in
  [internal/handlers/webhook.go](../internal/handlers/webhook.go)).
- Save, then copy the **Signing secret** (`whsec_…`).

Update `.env` on the VPS:

```sh
$EDITOR .env   # paste STRIPE_WEBHOOK_SECRET=whsec_...
docker compose up -d kuu-api   # picks up the new env
```

Trigger a test event:

```sh
# from your laptop — install the Stripe CLI first
stripe trigger checkout.session.completed
```

On the VPS, watch the structured logs:

```sh
docker compose logs -f kuu-api
```

A successful provision shows `provisioning started` →
`pterodactyl user resolved` → `pterodactyl server created` →
`provisioning succeeded`. The matching `request_id` runs through every
line.

If you see `rolled back …` lines, follow the trail — the rollback
chain unwinds in reverse order and tells you exactly which step failed.

---

## 10. Verify Prometheus metrics

```sh
curl -H "Authorization: Bearer $ADMIN_BEARER_TOKEN" \
     https://api.kuu.gg/metrics | head -40
```

You should see counters like `kuu_webhook_events_total` and histograms
like `kuu_provision_duration_seconds_bucket`. If any cardinality looks
wrong, see [docs/OPERATIONS.md](OPERATIONS.md#metrics-cheatsheet).

---

## 11. Set up backups

The Postgres data lives in the named volume `kuu-api_postgres-data`. At
minimum, schedule a nightly logical dump:

```sh
sudo install -d -o deploy -g deploy /var/backups/kuu-api
crontab -e
```

Add:

```
15 3 * * *  cd /srv/kuu-api && docker compose exec -T postgres pg_dump -U kuu -d kuu | gzip > /var/backups/kuu-api/kuu-$(date +\%F).sql.gz
0  4 * * *  find /var/backups/kuu-api -mtime +14 -delete
```

Then **do a restore drill**: copy a dump to a scratch host, `gunzip | psql`
it into a fresh Postgres, and run `kuu-migrate status` against it to prove
the schema and goose version are intact. An untested backup is not a
backup.

For better durability, ship the dumps off-host (S3, B2, OVH Object
Storage) — losing the VPS loses the local backups too.

---

## 12. Updating to a new release

```sh
cd /srv/kuu-api
git pull
docker compose build kuu-api migrate
docker compose run --rm migrate up      # if migrations changed
docker compose up -d kuu-api            # rolls the API container
```

The order matters: migrations first, API second. The unit file in
[deploy/systemd/](../deploy/systemd/) makes this dependency explicit; with
Compose, the operator does. If a release ships breaking schema changes
that the previous binary can't tolerate, schedule a brief outage —
`docker compose stop kuu-api` → migrate → `up -d`.

---

## Appendix A — systemd alternative

If you don't want Docker, the binaries can run directly under systemd.
This is fewer moving parts but no Caddy bundled, so you'd run Caddy or
nginx as a system package separately.

```sh
# Build on a build host (matching glibc) or cross-compile:
GOOS=linux GOARCH=amd64 go build -o kuu-api ./cmd/server
GOOS=linux GOARCH=amd64 go build -o kuu-migrate ./cmd/migrate
scp kuu-api kuu-migrate deploy@vps:/tmp/

# On the VPS:
sudo apt install postgresql-16
sudo -u postgres createuser --pwprompt kuu
sudo -u postgres createdb -O kuu kuu

sudo useradd --system --home /var/lib/kuu-api --shell /usr/sbin/nologin kuu
sudo install -d -o kuu -g kuu /var/lib/kuu-api
sudo install -m 0755 /tmp/kuu-api      /usr/local/bin/kuu-api
sudo install -m 0755 /tmp/kuu-migrate  /usr/local/bin/kuu-migrate
sudo install -d -o root -g kuu -m 0750 /etc/kuu-api
sudo install -m 0640 -o root -g kuu .env /etc/kuu-api/secrets

sudo cp deploy/systemd/kuu-migrate.service /etc/systemd/system/
sudo cp deploy/systemd/kuu-api.service     /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now kuu-api
```

`kuu-api.service` `Requires=` `kuu-migrate.service`, so `systemctl start
kuu-api` runs migrations first and only starts the API if they succeed.

You'll still want Caddy or nginx in front for TLS:

```sh
sudo apt install caddy
# then point /etc/caddy/Caddyfile at 127.0.0.1:8081 — see deploy/Caddyfile
# for a starting template; remove the docker-network host name and use
# localhost.
```

Logs go to journald:

```sh
journalctl -u kuu-api -f
journalctl -u kuu-migrate -n 50
```

---

## Troubleshooting

- **Caddy stuck on ACME challenge** — DNS hasn't propagated, or 80/443
  isn't actually open at the OVH firewall (separate from `ufw`). Confirm
  with `curl http://api.kuu.gg/` from another machine.
- **`/readyz` returns 503 with `pterodactyl: ...`** — panel is down or
  the API key lost permissions. The probe issues
  `GET /api/application/nodes?per_page=1`; reproduce manually.
- **`/readyz` returns 503 with `smtp: ...`** — the SMTP TCP dial fails.
  Provider rotated credentials, network egress blocked, or wrong port.
- **Stripe webhook 400s** — almost always `STRIPE_WEBHOOK_SECRET`
  mismatch. The dashboard shows the raw response body, which is the
  Go signature error string.
- **`pq: relation "..." does not exist`** — you started `kuu-api` before
  running `migrate up`. Stop the API, run migrations, restart.
- **Welcome emails never send** — `GET /admin/emails` to inspect the
  queue. Check `last_error`. If SMTP credentials are wrong, fix env and
  `POST /admin/emails/{id}/retry`.

For day-2 procedures see [OPERATIONS.md](OPERATIONS.md).
