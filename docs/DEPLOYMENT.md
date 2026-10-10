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
    that has `users` and `servers` read+write, plus `nodes` read so the
    multi-node load picker can fetch utilization.
  - One or more configured **Nodes**, each with an **Allocation pool**.
    kuu-api picks the least-loaded node from the configured pool per
    request — list every node ID you want it to use in `NODE_IDS`.
  - The **Nests** and **Eggs** for each variant you sell. The current
    catalog (Minecraft Paper egg 4 nest 1, Minecraft Forge egg 2 nest 1,
    Rust egg 14 nest 4) lives in
    [`internal/plans/plans.go`](../internal/plans/plans.go). Any new
    game/variant requires a code change there before it can be sold.
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

```text
api.kuu.gg.   A   <your VPS public IPv4>
```

You need to decide one thing here: **proxy through Cloudflare (orange
cloud) or point DNS straight at the VPS (gray cloud).** Either is
supported; the choice changes how TLS is provisioned.

- **Gray cloud (direct).** DNS resolves to the VPS IP. Caddy provisions
  a Let's Encrypt cert via HTTP-01 over port 80. Skip § 3a and use
  Caddy's defaults.
- **Orange cloud (Cloudflare proxy).** DNS resolves to Cloudflare anycast
  IPs that proxy traffic to the VPS. HTTP-01 won't work because
  Cloudflare intercepts port 80. You must provision a Cloudflare Origin
  Certificate and serve that from Caddy — see § 3a.

In either case, wait until `dig +short api.kuu.gg` returns the expected
IP from the VPS itself before continuing. With Cloudflare proxy you'll
see Cloudflare anycast IPs; with gray cloud you'll see the VPS public
IP. Both are correct for their respective path.

---

## 3a. Cloudflare Origin Certificate (only if proxied)

Skip this section if you chose gray cloud above.

### Generate the cert

In the Cloudflare dashboard for your zone:

1. **SSL/TLS → Origin Server → Create Certificate**
   - Private key type: ECC (smaller, faster handshake than RSA)
   - Hostnames: include the `api.kuu.gg` you're deploying. A wildcard
     like `*.kuu.gg` is fine if you also serve other subdomains.
   - Validity: 15 years (default — these certs are private to your
     origin and don't follow public CA validity rules).
2. **SSL/TLS → Overview → set encryption mode to "Full (strict)".**
   Without strict, Cloudflare will accept *any* cert from your origin
   (including self-signed), defeating the purpose of using an origin
   cert in the first place. Required.

Cloudflare displays the certificate and private key once. Save both
immediately — the cert is recoverable from the dashboard, the private
key is not.

### Install on the VPS

```sh
sudo install -d -o root -g root -m 0755 /etc/ssl/cloudflare
sudo tee /etc/ssl/cloudflare/origin.crt > /dev/null <<'EOF'
-----BEGIN CERTIFICATE-----
... paste the cert from Cloudflare ...
-----END CERTIFICATE-----
EOF

sudo tee /etc/ssl/cloudflare/origin.key > /dev/null <<'EOF'
-----BEGIN EC PRIVATE KEY-----
... paste the private key from Cloudflare ...
-----END EC PRIVATE KEY-----
EOF

sudo chmod 644 /etc/ssl/cloudflare/origin.crt
sudo chmod 600 /etc/ssl/cloudflare/origin.key
```

`docker-compose.yml` already mounts `/etc/ssl/cloudflare` read-only into
the Caddy container, and the bundled `deploy/Caddyfile` already serves
the cert from that path with `tls /etc/ssl/cloudflare/origin.crt …`. No
Caddy edits needed.

### Bot Fight Mode and the API

While you're in the Cloudflare dashboard, exempt `api.kuu.gg` from
Bot Fight Mode and Browser Integrity Check. The API is already
authenticated (Stripe webhook signatures + bearer token on `/admin/*`),
and Cloudflare's bot heuristics produce false positives that look
identical to a real outage.

**Security → Bots → Bot Fight Mode** — turn off, **or** add a WAF
custom rule:

```text
When (http.host eq "api.kuu.gg")
Then Skip: Bot Fight Mode, Browser Integrity Check
```

### Rotating the origin cert

Origin certs are 15 years by default; in practice you'll rotate when
the private key is compromised or when you re-architect the
Cloudflare zone. To rotate:

```sh
sudo install -m 644 newcert.pem /etc/ssl/cloudflare/origin.crt
sudo install -m 600 newkey.pem  /etc/ssl/cloudflare/origin.key
docker compose restart caddy
```

Caddy reloads the cert on restart; no kuu-api restart needed.

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
# Comma-separated pool of panel node IDs. The provisioner picks the
# least-loaded node from this list per request. For a single-node deploy
# just put one ID here. NODE_ID (singular) is honored as a legacy fallback
# but new deploys should use NODE_IDS.
NODE_IDS=1

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
`up -d`. The migrate binary uses `embed.FS` for the SQL files, so a
**rebuild is required any time `migrations/*.sql` changes** — Compose's
build cache will otherwise reuse a stale image and goose will report
"migrations up to date" against an outdated set:

```sh
docker compose build migrate
docker compose run --rm migrate up
```

You should see goose print one line per applied migration. The `postgres`
service is started as a dependency and stays up after — that's expected.

Verify:

```sh
docker compose run --rm migrate status
```

All four migrations (`001_initial`, `002_idempotency_and_indexes`,
`003_pending_emails`, `004_server_variant_and_version`) should report as
applied.

---

## 8. Bring up the stack

The deploy supports two reverse-proxy layouts; pick one based on what's
already running on the host.

### 8a. Bundled Caddy (fresh VPS, nothing else on 80/443)

```sh
docker compose --profile caddy up -d
docker compose ps
```

Expected: `postgres`, `kuu-api`, `caddy` all `healthy` (or `running` for
caddy, which has no healthcheck).

The bundled Caddy lives behind the `caddy` profile so a plain
`docker compose up -d` on a host that already runs nginx/traefik will
not collide on port 80/443. If you forget the `--profile caddy` flag,
`postgres` and `kuu-api` come up but Caddy doesn't, and `api.kuu.gg`
won't be reachable from the public internet — you'd just get the API
on `127.0.0.1:8081` on the host.

### 8b. External reverse proxy (nginx, traefik, existing on host)

If the host already terminates TLS for other sites (e.g. nginx serving
your marketing page), don't run the bundled Caddy. Instead bring up the
API + DB only and let your existing reverse proxy add an `api.kuu.gg`
site that forwards to `127.0.0.1:8081`:

```sh
docker compose up -d postgres kuu-api
docker compose ps
```

`kuu-api` binds to `127.0.0.1:8081` on the host (set in
`docker-compose.yml`), so it's reachable from the host's nginx/traefik
but not from the public internet directly. Make sure
`TRUSTED_PROXY_CIDRS` in `.env` includes `127.0.0.1/32` (the default
already does) so kuu-api honors `X-Forwarded-For` from the host-side
proxy.

A minimal nginx site that pairs cleanly with a Cloudflare Origin
Certificate (matching the Caddy path in § 3a) looks like this. Save it
as `/etc/nginx/sites-available/api.kuu.gg`, symlink into
`sites-enabled`, then `sudo nginx -t && sudo systemctl reload nginx`.

```nginx
server {
    listen 80;
    listen [::]:80;
    server_name api.kuu.gg;
    return 301 https://$host$request_uri;
}

server {
    listen 443 ssl http2;
    listen [::]:443 ssl http2;
    server_name api.kuu.gg;

    ssl_certificate     /etc/ssl/cloudflare/origin.crt;
    ssl_certificate_key /etc/ssl/cloudflare/origin.key;
    ssl_protocols       TLSv1.2 TLSv1.3;

    # Stripe webhooks are signed against the raw body; do not modify it.
    # nginx's defaults preserve the body fine — just don't add gzip on
    # the request side or any rewriting modules in the request pipeline.
    client_max_body_size 1m;

    location / {
        proxy_pass         http://127.0.0.1:8081;
        proxy_http_version 1.1;
        proxy_set_header   Host              $host;
        proxy_set_header   X-Real-IP         $remote_addr;
        proxy_set_header   X-Forwarded-For   $proxy_add_x_forwarded_for;
        proxy_set_header   X-Forwarded-Proto $scheme;
        proxy_read_timeout 60s;
    }
}
```

Verify connectivity from the host before adding the nginx site:

```sh
curl -fsS http://127.0.0.1:8081/livez
```

### 8c. Verifying TLS startup

If you used § 8a (bundled Caddy):

```sh
docker compose logs -f caddy
```

What to look for, depending on the TLS path you chose in § 3:

- **Gray cloud / Let's Encrypt:** a line like
  `certificate obtained successfully ... identifier: api.kuu.gg`. If you
  see ACME 4xx errors, DNS hasn't propagated or port 80/443 isn't open
  — fix those before retrying or you will burn through Let's Encrypt's
  rate limit.
- **Orange cloud / origin cert:** Caddy logs `serving initial
  configuration` and starts immediately. There is no ACME flow because
  the cert came from Cloudflare. If you see `error loading TLS
  certificate ... no such file`, the host bind mount under
  `/etc/ssl/cloudflare` is wrong or the files were never written —
  recheck § 3a.

If you used § 8b (external nginx), watch the nginx error log instead:

```sh
sudo tail -f /var/log/nginx/error.log
```

You should see a clean reload, no warnings about missing
`ssl_certificate`. Curl `https://api.kuu.gg/livez` from your laptop;
errors here usually mean the cert path in the nginx config is wrong or
the file permissions are blocking nginx from reading the key.

### 8d. Smoke test from your laptop

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
- Events: all five that
  [internal/handlers/webhook.go](../internal/handlers/webhook.go)
  switches on:
  - `checkout.session.completed` (provisions)
  - `invoice.payment_failed` (marks the subscription past due and emails
    the customer; does **not** suspend)
  - `customer.subscription.updated` (suspends when Stripe gives up
    retrying, restores when it's active again)
  - `invoice.paid` (restores a server suspended for non-payment)
  - `customer.subscription.deleted` (suspends — server is reaped 14 days
    later, see [§13 Subscription lifecycle](#13-subscription-lifecycle))
- In **Billing → Revenue recovery**, set "If all retries for a payment
  fail" to **mark the subscription as unpaid** (customers who pay late
  get their server back automatically) or **cancel the subscription**.
  Don't choose "leave the subscription past-due": nothing would ever
  suspend the server.
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

## 11. Set up off-host backups

Postgres data lives in the `postgres-data` named volume on the VPS. The
volume is durable across container restarts but not across VPS loss, so
configure the bundled backup service to ship dumps to S3-compatible
object storage.

1. Sign up for a provider and create a bucket. Recommended:
   - **Backblaze B2** — cheapest, 10 GB free
     (`https://s3.us-west-002.backblazeb2.com` or your bucket region)
   - **Cloudflare R2** — free egress, 10 GB free
     (`https://<accountid>.r2.cloudflarestorage.com`)
2. Create an application key scoped to that bucket. Note the access key
   and secret.
3. Fill in the `BACKUP_S3_*` block in `.env` (see `.env.example` for the
   full list — at minimum `BACKUP_S3_ENDPOINT`, `BACKUP_S3_BUCKET`,
   `BACKUP_S3_ACCESS_KEY`, `BACKUP_S3_SECRET_KEY`).
4. Bring the service up — it lives behind the `backup` profile so it
   doesn't start unless explicitly enabled:

   ```sh
   docker compose --profile backup up -d backup
   ```

The service runs `pg_dump`, gzips, uploads with a timestamped key, and
prunes objects older than `BACKUP_RETENTION_DAYS` on each cycle. Tail
`docker compose logs -f backup` to confirm the first upload — with
`BACKUP_RUN_ON_START=true` (the default) you should see one within
seconds.

**Drill the restore before you need it.** Restore procedure is in
[OPERATIONS.md → Backups → Restore from backup](OPERATIONS.md#restore-from-backup).
An untested backup is not a backup.

---

## 12. Updating to a new release

```sh
cd /srv/kuu-api
git pull
docker compose build kuu-api migrate
docker compose run --rm migrate up      # if migrations changed
docker compose up -d kuu-api            # rolls the API container
```

The order matters: migrations first, API second. Two gotchas:

- **Always rebuild `migrate` when `migrations/` changes.** The migrations
  are embedded in the binary via `go:embed`, so the running image must
  be rebuilt before `migrate up` will see new files. Compose's layer
  cache will silently reuse a stale image and report "migrations up to
  date" otherwise. The `docker compose build kuu-api migrate` line
  above covers this.
- **If the new release adds the backup service or changes its env**, run
  `docker compose --profile backup build backup` and
  `docker compose --profile backup up -d backup` separately — the
  default `up -d` doesn't touch services in non-default profiles.

The unit file in [deploy/systemd/](../deploy/systemd/) makes the
migrate-then-api ordering explicit; with Compose, the operator does. If
a release ships breaking schema changes that the previous binary can't
tolerate, schedule a brief outage — `docker compose stop kuu-api` →
migrate → `up -d`.

---

## 13. Subscription lifecycle

Once paid traffic is flowing, a subscription moves through the following
states. The deploy is wired so the operator does not need to intervene
in the steady-state lifecycle, but the table below names which
component drives each transition so you know where to look when
something is off:

| Stripe event | DB effect | Server effect | Driven by |
| ------------ | --------- | ------------- | --------- |
| `checkout.session.completed` | inserts `customers`, `subscriptions`, `servers` rows | creates Pterodactyl user + server | provisioner |
| `invoice.payment_failed` | `subscriptions.status='past_due'` (from active only); payment-failed email queued on the first attempt | none — server keeps running while Stripe retries | webhook → provisioner |
| `customer.subscription.updated` (status `unpaid`) | `status='suspended'`, `suspended_reason='non_payment'`, `cancelled_at` set if empty | suspends Pterodactyl server | webhook → provisioner |
| `customer.subscription.updated` (status `canceled`) or `customer.subscription.deleted` | `status='suspended'`, `suspended_reason='cancelled'`, `cancelled_at` set if empty | suspends Pterodactyl server | webhook → provisioner |
| `invoice.paid` or `customer.subscription.updated` (status `active`) | past_due → active; suspended for `non_payment` → active, `cancelled_at` and reason cleared | unsuspends Pterodactyl server (non-payment only) | webhook → provisioner |
| (none — internal timer) | deletes `servers` row when `cancelled_at < NOW() - 14d` | deletes Pterodactyl server | reaper (`internal/provisioner/reaper.go`) |

**Suspended servers are kept for 14 days** before the daily reaper hard-
deletes them. Subscription, customer, and Pterodactyl-user rows are
preserved across the reap so billing history stays intact and a
returning customer keeps their login. To change the grace period, edit
`defaultReapGracePeriod` in `internal/provisioner/reaper.go` — there is
no env knob today; flip it to a constant change so the policy is
reviewable in code review.

If a customer wants to come back inside the 14-day window, the supported
path is to manually unsuspend in the panel and re-activate the
subscription in Stripe. There is no admin endpoint for this yet — file
it under "talk to ops" until volume justifies one.

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
