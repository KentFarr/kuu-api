# kuu-api — Frontend Integration Guide

This document is the contract for any client (browser, mobile app, AI
agent building a frontend) that talks to **kuu-api**. It covers the
public HTTP surface, the games / variants / plans catalog the frontend
needs to render, the post-purchase flow, and the error model.

If you're operating the service rather than integrating with it, see
[DEPLOYMENT.md](DEPLOYMENT.md) and [OPERATIONS.md](OPERATIONS.md)
instead.

---

## 1. What kuu-api is, what the frontend's job is

kuu-api is a server-side Go service that turns a Stripe Checkout
payment into a running game server on a Pterodactyl panel. It exposes
**one** endpoint for unauthenticated frontend traffic:
`POST /api/checkout`. Everything else (provisioning the server,
talking to Pterodactyl, suspending on cancellation, hard-deleting after
14 days) happens server-side, off the request path, driven by Stripe
webhooks.

The frontend's only jobs in the purchase flow are:

1. Render a catalog UI: pick a game, pick a variant, pick a plan, optionally pick a version.
2. Call `POST /api/checkout` with that selection.
3. Redirect the browser to the returned Stripe Checkout URL.
4. Render the `/success` and `/cancel` landing pages Stripe redirects
   back to.
5. Tell the user to expect two emails: a "server ready" email from Kuu
   with their server details, and (first purchase only) an "Account
   Created" email from the panel with a link to set their password.

The frontend does **not**:

- Talk to Pterodactyl directly.
- Poll for "is my server ready yet?" — provisioning is asynchronous and
  the user-facing signal is the welcome email. There is no
  customer-facing status endpoint today.
- Carry any kuu-api authentication. All authenticated endpoints
  (`/admin/*`, `/metrics`) are bearer-locked and operations-only.

---

## 2. Base URL and environment

| Environment | Base URL |
| ----------- | -------- |
| Production  | `https://api.kuu.gg` |
| Local dev   | `http://localhost:8081` |

All endpoints are HTTPS in production and use JSON for request and
response bodies (request bodies are limited to **4 KB**; the API will
reject larger payloads with `400`).

### CORS

Production allows the single origin set in
`CORS_ALLOWED_ORIGIN`. By default that is `https://kuu.gg`. The
frontend should be served from that origin or CORS will block the
preflight. For local development, point the frontend at
`http://localhost:8081` and run kuu-api with
`CORS_ALLOWED_ORIGIN=http://localhost:3000` (or whatever your frontend
dev server uses).

CORS preflight (`OPTIONS /api/checkout`) is handled by the same CORS
middleware as the main request. No separate path is needed.

### Required and optional headers

| Header | Required | Notes |
| ------ | -------- | ----- |
| `Content-Type: application/json` | Yes | Body is JSON. |
| `Origin` | Yes (browser auto) | Must match `CORS_ALLOWED_ORIGIN`. |
| `Accept: application/json` | No, but recommended | The API returns JSON either way. |

---

## 3. The endpoint: `POST /api/checkout`

### Request body

```json
{
  "game":     "minecraft",
  "variant":  "paper",
  "version":  "1.21.1",
  "price_id": "price_1TUrteL5ibjgKZejVswcDP1y"
}
```

Field reference:

| Field | Type | Required | Notes |
| ----- | ---- | -------- | ----- |
| `game` | string | **Yes** | Game key. See § 5 for valid values. Must be a configured game. |
| `variant` | string | No | Variant key. See § 5. If omitted, the game's default variant is used. |
| `version` | string | No | Version string for variants that support a version field. **Must match `^[A-Za-z0-9._-]{1,32}$`** — letters, digits, `.`, `_`, `-`, max 32 chars. Sending a `version` for a variant that doesn't accept one is a `400`. |
| `price_id` | string | **Yes** | Stripe price ID for the plan. See § 6. |

The body is strict-decoded — **any unknown field fails with `400`**.
Whitespace in `variant` and `version` is trimmed before validation.

### Successful response (`200 OK`)

```json
{
  "checkout_url": "https://checkout.stripe.com/c/pay/cs_test_a1..."
}
```

The frontend should redirect the browser to `checkout_url` immediately.
The URL is single-use and time-limited by Stripe.

### Error responses

All errors are `text/plain` from `http.Error` (subject to change to
JSON in a future revision; treat the status code as authoritative, the
body as advisory).

| Status | Body | Cause | What to show the user |
| ------ | ---- | ----- | --------------------- |
| `400` | `invalid request body` | Body wasn't valid JSON, exceeded 4 KB, or contained an unknown field. | "Something went wrong. Please refresh and try again." (developer-side bug) |
| `400` | `game and price_id are required` | One of the two mandatory fields was empty or missing. | Highlight the missing form field. |
| `400` | `unsupported game` | `game` wasn't in the catalog, or refers to a stub (e.g. a pre-launch game). | Don't expose unsupported games in the UI; this should be unreachable. |
| `400` | `unsupported variant for game` | `variant` doesn't exist on this game. | Form-level validation should prevent this. |
| `400` | `this variant does not accept a version` | The variant has no `VersionField` (e.g. Rust). | Hide the version picker for that variant. |
| `400` | `version contains invalid characters or is too long` | `version` failed regex validation. | "Version must be letters, digits, `.`, `_`, `-`, max 32 characters." |
| `400` | `unsupported price` | `price_id` wasn't recognised (mode mismatch — using a live-mode price ID against a test-mode backend, or vice versa — is the most common cause). | "This plan isn't available right now." Log + alert. |
| `429` | (rate-limit body) | Too many requests from this IP. See § 4. | "Slow down — please try again in a moment." |
| `500` | `failed to create checkout session` | Stripe API call failed. Likely a Stripe outage or a misconfigured secret key on the backend. | "Something went wrong. Please try again shortly." Log to monitoring. |

Everything 2xx is success; anything else, treat as a failure path.

---

## 4. Rate limiting

`POST /api/checkout` is rate-limited per client IP at:

- **1 request every 2 seconds** sustained
- **Burst of 5 requests**

That's per-IP, leaky-bucket style. A normal user clicking "Buy" once
will never hit it. A scraping client or a tight retry loop will.

When the limit is exceeded, the API returns **`429 Too Many Requests`**.
The frontend should not retry automatically — surface a brief "slow
down" message instead. There is no `Retry-After` header today.

If the frontend is served behind a CDN or shared NAT, the IP seen by
the rate limiter is the client IP per `X-Forwarded-For` (only honored
when the connection comes from a CIDR in the backend's
`TRUSTED_PROXY_CIDRS` config — operations problem, not yours).

---

## 5. Games and variants catalog

The frontend needs to render the games and their variants. The
authoritative source is
[`internal/plans/plans.go`](../internal/plans/plans.go); this section
mirrors it for ease of reading and will be kept in sync.

| Game key | Display name | Default variant | Variants exposed | Version field | Notes |
| -------- | ------------ | --------------- | ---------------- | ------------- | ----- |
| `minecraft` | Minecraft | `paper` | `paper`, `forge` | Yes (`MINECRAFT_VERSION` for paper, `MC_VERSION` for forge) | Send a Minecraft version string like `1.21.1`. Vanilla is intentionally not exposed. |
| `rust` | Rust | `vanilla` | `vanilla`, `oxide`, `carbon` | No | Modding framework chosen via the `variant` key; no user-selectable version. |
| `fivem` | FiveM | `vanilla` | `vanilla` | Yes (`FIVEM_VERSION`) | Accepts `recommended` (default), `latest`, or a build like `6013-d8ae399d`. Customer must paste their cfx.re license in the Pterodactyl panel before the server starts — see § 8. |
| `gmod` | Garry's Mod | `vanilla` | `vanilla` | No | Default `GAMEMODE` is `sandbox` on `gm_flatgrass`. Customer changes gamemode and workshop collection in the panel. |

### How the frontend should think about this

- **Always send `game`.** It's required.
- Send `variant` when you want a non-default. Omitting it always lands
  on the game's default variant.
- Send `version` **only if the variant has a version field** (third
  column above). Sending it for `rust` or `gmod` returns `400`.
- A "version" picker should be a free-text field for now (or a
  curated dropdown of recommended values that you maintain on the
  frontend). The backend doesn't validate the version string against
  reality — Pterodactyl does that during install. An invalid
  Minecraft version, for example, is accepted by the API and fails
  later during the egg installer.

### Adding a new game / variant

If you find yourself wanting to surface a new variant in the UI that
isn't in the table above: it isn't supported until someone lands a code
change in `internal/plans/plans.go` and redeploys. Don't expose
phantom options.

---

## 6. Plans (RAM tiers and Stripe price IDs)

Each game can be sold at any of six plan tiers, mapped to Stripe price
IDs through the backend's environment variables.

| Plan name | RAM | CPU (% of 1 core) | Disk | `.env` key |
| --------- | --- | ----------------- | ---- | ---------- |
| `2GB`  | 2048 MB  | 100 | 10 GB  | `STRIPE_PRICE_2GB`  |
| `4GB`  | 4096 MB  | 150 | 20 GB  | `STRIPE_PRICE_4GB`  |
| `8GB`  | 8192 MB  | 200 | 40 GB  | `STRIPE_PRICE_8GB`  |
| `16GB` | 16384 MB | 300 | 80 GB  | `STRIPE_PRICE_16GB` |
| `24GB` | 24576 MB | 400 | 120 GB | `STRIPE_PRICE_24GB` |
| `32GB` | 32768 MB | 500 | 160 GB | `STRIPE_PRICE_32GB` |

The frontend submits the **Stripe price ID** in the request, not the
plan name. The backend maps the price ID back to the plan tier via
`FindByPriceID`.

### Where the frontend gets the price IDs

There is **no `/api/plans` endpoint today**. The frontend needs to know
the live + test mode price IDs out of band. Two acceptable patterns:

1. **Hardcode them in the frontend's environment**, mirroring the
   server's `.env`. Two files, one source of truth (Stripe), kept in
   sync at deploy time. Simplest for a small team.
2. **Build a static catalog file** in the frontend repo (e.g.
   `src/catalog.ts`) that maps `{game, variant, plan} → priceId`, plus
   any UI-only metadata you want (price display, feature blurb,
   recommended badges).

Whatever you pick, **don't fetch price IDs at runtime from a public
endpoint** — there isn't one, and exposing one would just leak
information about your Stripe account structure.

### Validating before checkout

Before calling `POST /api/checkout`, the frontend should already have
validated:

1. The selected game is in the catalog.
2. The selected variant exists for that game.
3. If a version is being sent, the variant accepts versions.
4. The selected plan has a non-empty price ID for the current
   environment (test vs live).

A failed `POST /api/checkout` is more expensive than a client-side
guard.

---

## 7. The full purchase flow

```text
┌─────────┐  POST /api/checkout         ┌─────────┐
│ Browser │ ──────────────────────────► │ kuu-api │
│         │ ◄────────────────────────── │         │
│         │     200 {checkout_url}      └─────────┘
│         │
│         │  redirect
│         ▼
│  ┌──────────────┐                     ┌─────────┐
│  │ Stripe       │  webhook            │ kuu-api │
│  │ Checkout UI  │ ──────────────────► │         │
│  └──────┬───────┘                     │         │
│         │ pay                         │         │ ┌─────────────┐
│         ▼                             │         │►│ Pterodactyl │
│  ┌─────────────┐                      │         │ │ panel       │
│  │ /success    │ ◄────────────────────┤         │ └─────────────┘
└─►│ landing pg  │   redirect           │         │
   └─────────────┘                      │         │ ┌─────────────┐
                                        │         │►│ SMTP relay  │
                                        └─────────┘ │ welcome ✉   │
                                                    └─────────────┘
```

Flow narrative:

1. **User clicks Buy** → frontend `POST /api/checkout` → gets
   `checkout_url`.
2. **Frontend redirects** the browser to `checkout_url`.
3. **Stripe Checkout UI loads.** The user enters payment, optionally
   fills the **"Choose a username (optional)"** custom field
   (3–32 chars), and confirms.
4. **Stripe redirects to `CHECKOUT_SUCCESS_URL`** (default
   `https://kuu.gg/success`) on payment, or
   **`CHECKOUT_CANCEL_URL`** (default `https://kuu.gg`) on cancel.
   Provisioning has not happened yet at the moment of redirect.
5. **Stripe asynchronously fires a webhook** to kuu-api, which
   creates a Pterodactyl user (or reuses one), provisions a server
   on the least-loaded node, persists the customer ↔ subscription ↔
   server mapping, and queues a welcome email.
6. **The customer receives the welcome email** with their panel URL
   (`panel.kuu.gg`), username, and server address. It contains no
   password. On a first purchase, the panel separately emails an
   "Account Created" message with a **Setup Your Account** link to
   choose a password (valid 60 minutes; after that, the panel's
   "Forgot password" page at `panel.kuu.gg/auth/password` issues a new
   one). The welcome email is the user-facing "your server is ready"
   signal — typically arrives within a minute of payment confirmation.

### What the `/success` page should say

Provisioning is **not synchronous** with the redirect. Your `/success`
page must not promise an instant server. Suggested copy:

> Payment received. Your server is being provisioned now — you'll get
> a welcome email at *the address you used at checkout* within a few
> minutes with your panel URL, username and server address. New
> customers also get an email from our panel with a link to set their
> password.
> If it's been more than 10 minutes and you haven't seen it, check
> your spam folder, then contact support.

If the user comes from a Stripe Checkout where they entered an email
that's known to be undeliverable, the welcome email queues forever;
ops needs the admin endpoints to recover. Surface a clear support
contact path on the success page.

### What the `/cancel` page should say

Stripe redirects here when the user backs out of payment. No
subscription was created and nothing happened server-side. The
frontend should let them retry the purchase.

---

## 8. Per-game post-purchase notes

These are things the customer needs to know *after* their server is
provisioned. The welcome email handles the generic case; the frontend
may want to surface them up front in the catalog UI to set
expectations.

### FiveM

FiveM servers ship with a placeholder `FIVEM_LICENSE` that **must** be
replaced before the server will start. The customer obtains a license
key from the [cfx.re portal](https://portal.cfx.re/) (free). They then:

1. Open `panel.kuu.gg` and log in with the username from the welcome
   email and the password they set through the panel's setup link.
2. Open their server → **Startup** tab.
3. Replace the `Server License` value with their cfx.re key.
4. Save and start the server.

Frontend should mention "FiveM requires a free license key from
cfx.re — link" on the FiveM purchase page and on the `/success` page
when game was FiveM.

### Garry's Mod

GMod servers default to `sandbox` on `gm_flatgrass`. Other gamemodes
(TTT, DarkRP, Murder, etc.) are a per-server config change in the
panel:

1. Server → Startup → change `GAMEMODE` (`sandbox`, `terrortown`,
   `darkrp`, etc.) and `SRCDS_MAP`.
2. Optionally set `WORKSHOP_ID` to a workshop collection ID for
   addons.
3. Restart the server.

Optional: customer creates a Game Server Login Token (GSLT) from
[Steam](https://steamcommunity.com/dev/managegameservers) and pastes
it into `STEAM_TOKEN` so the server appears in the public browser.

### Minecraft (Paper / Forge)

Minecraft servers come up running the chosen version on default world
gen. Standard panel customization (server.properties, plugins/, mods/)
applies via SFTP.

### Rust

Rust servers run with a strong randomly-generated RCON password
exposed in the panel under **Startup → RCON Password**. World seed,
size, and other server.cfg values are panel-editable.

---

## 9. Subscription lifecycle (from the customer's POV)

Once the customer has paid, the lifecycle is:

| State | Trigger | What the customer sees |
| ----- | ------- | ---------------------- |
| Active | Initial purchase succeeded | Server reachable, panel access works, SFTP works. |
| Suspended | Subscription cancelled in Stripe, or `invoice.payment_failed` | Server stopped (won't start in panel). Customer's panel login still works; server files still on disk. |
| Hard-deleted | 14 days after suspension, by the daily reaper | Server gone from panel. World files and configs are gone with it. Customer login still exists; a new purchase from the same email will reuse it. |

A customer who cancels and changes their mind within 14 days can be
restored by ops manually re-activating the Stripe subscription and
unsuspending the server in the panel — there is no self-serve
reactivation today.

The frontend's job here is really just to set expectations correctly:
"if you cancel, your server keeps your data for 14 days, then it's
deleted." That belongs on the pricing or terms page, not in the
purchase flow itself.

---

## 10. Health and status endpoints

If the frontend needs a status indicator (e.g. "API healthy" badge):

| Endpoint | Auth | Purpose |
| -------- | ---- | ------- |
| `GET /livez` | none | Always 200 if the process is up. Cheapest health check. |
| `GET /readyz` | none | Returns 200 with a JSON body of probe results, or 503 if any dependency is down. |
| `GET /health` | none | Alias for `/livez`, kept for backwards compatibility. |

Sample `/readyz` response:

```json
{
  "checks": {
    "database":    "ok",
    "pterodactyl": "ok",
    "smtp":        "ok"
  },
  "nodes": [
    { "node_id": 1, "status": "ok", "memory_total_mb": 28672, "free_allocations": 3 }
  ],
  "service": "kuu-api"
}
```

`smtp` may be `"skipped"` if email is not configured (welcome emails
won't send in that case — operations problem). `nodes` reflects
Pterodactyl panel utilization snapshots.

The frontend should not poll these aggressively — once a minute is
fine if you really need a status pill; once on page load is more
typical.

---

## 11. What kuu-api does NOT expose to the frontend

Don't try to build features that depend on these — they don't exist:

- **No `/api/plans` catalog endpoint.** Hardcode (§ 6).
- **No "is my server ready yet?" customer endpoint.** The welcome
  email is the signal.
- **No customer login / session.** Account state is implied by the
  Stripe customer + the Pterodactyl user; we don't issue our own
  session cookies.
- **No "list my servers" endpoint for end users.** They see their
  servers by logging into `panel.kuu.gg` directly.
- **No "cancel my subscription" endpoint.** Cancellation goes through
  Stripe's customer portal (which you'd configure on the Stripe side
  and link from the success/account pages).
- **No password reset or username change flow on the API.**
  kuu-api never sets or sees customer passwords. Customers set and
  reset them through the Pterodactyl panel (`panel.kuu.gg/auth/password`).
- **No `Retry-After` on rate-limit responses.** Just back off.

---

## 12. Code examples

### Plain `fetch` from the browser

```ts
type CheckoutRequest = {
  game: 'minecraft' | 'rust' | 'fivem' | 'gmod';
  variant?: string;
  version?: string;
  price_id: string;
};

async function startCheckout(req: CheckoutRequest): Promise<void> {
  const r = await fetch('https://api.kuu.gg/api/checkout', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(req),
  });

  if (!r.ok) {
    const detail = await r.text();
    throw new Error(`checkout failed: ${r.status} ${detail}`);
  }

  const { checkout_url } = (await r.json()) as { checkout_url: string };
  window.location.href = checkout_url;
}
```

### Validating client-side before posting

```ts
type Game = 'minecraft' | 'rust' | 'fivem' | 'gmod';

const variantsByGame: Record<Game, string[]> = {
  minecraft: ['paper', 'forge'],
  rust:      ['vanilla', 'oxide', 'carbon'],
  fivem:     ['vanilla'],
  gmod:      ['vanilla'],
};

const variantsAcceptingVersion = new Set([
  'minecraft:paper', 'minecraft:forge', 'fivem:vanilla',
]);

function validate(req: CheckoutRequest): string | null {
  if (!variantsByGame[req.game]) return 'unsupported game';

  const variant = req.variant ?? variantsByGame[req.game][0];
  if (!variantsByGame[req.game].includes(variant)) return 'unsupported variant';

  if (req.version) {
    if (!variantsAcceptingVersion.has(`${req.game}:${variant}`)) {
      return 'this variant does not accept a version';
    }
    if (!/^[A-Za-z0-9._-]{1,32}$/.test(req.version)) {
      return 'invalid version format';
    }
  }
  if (!req.price_id) return 'price_id required';
  return null;
}
```

### curl

```sh
curl -s -X POST https://api.kuu.gg/api/checkout \
  -H 'Content-Type: application/json' \
  -d '{
    "game":     "minecraft",
    "variant":  "paper",
    "version":  "1.21.1",
    "price_id": "price_..."
  }'
```

---

## 13. Versioning

The API does not have an explicit version prefix today (no `/v1/`).
Breaking changes will be announced via a new path prefix; non-breaking
additions (new fields, new optional features) will land in place. The
frontend should:

- Not depend on the **string contents** of error response bodies — they
  are advisory and may change. Treat status codes as the contract.
- Not break on **unknown JSON fields** in responses. The frontend
  should ignore fields it doesn't recognise.
- Not assume any particular ordering of fields in JSON responses.

---

## 14. Reporting bugs / asking questions

The integration story isn't perfect — common things you'll likely hit
that aren't covered above (pricing display, account portal
links) need product decisions and a backend change.
Open an issue or ping ops with the use case rather than working around
the API surface.
