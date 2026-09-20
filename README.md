# Catical

Concatenates multiple iCal subscription feeds into a single feed. Makes it easier to share your complex calendar with friends or partners. Unlocking new polyamorous possibilities.

**https://catical.sci1.uk** — no accounts. Create a mix and you get two URLs: a feed to subscribe to, and a manage link. Copy both immediately. The manage URL is unrecoverable; the feed URL is not shown again. Lose manage and the mix is gone (you can still rotate tokens from manage while you have it).

## Use it

1. Open the site, name the mix, paste 1–8 https ICS URLs (optional labels, optional “prefix summaries”).
2. Copy the **feed** URL (`…/c/{id}/{secret}.ics`) and the **manage** URL (`…/m/{id}/{secret}`).
3. In Google Calendar: Settings → Add calendar → From URL, paste the feed URL.
4. Edit name/sources, rotate tokens, or delete from the manage page.

Cache: a successful merge is served fresh for **5 minutes**. If an origin fails, last-good is served up to **24 hours**; after that the feed errors until origins work. Unused mixes are **deleted after 14 days** without an ICS fetch. Opening manage does not extend that. Create starts a 14-day grace.

## Run locally

```sh
docker compose up --build
```

App: `http://localhost:8080`. Postgres is published on `localhost:5433` (avoids a host Postgres on 5432).

Env (production / Coolify):

| Variable | Role |
|---|---|
| `DATABASE_URL` | Postgres URL (required) |
| `BASE_URL` | Public origin, e.g. `https://catical.sci1.uk` |
| `TURNSTILE_SITE_KEY` | Cloudflare widget (optional locally) |
| `TURNSTILE_SECRET` | Server-side siteverify; unset = Turnstile off (local/dev) |
| `LOG_LEVEL` | `debug` / `info` / `warn` / `error` |

Migrations apply on process start (version table; safe to run twice). Listen `:8080`. Healthcheck: `GET /healthz`.

## Production (Coolify)

Dockerfile build pack, listen `8080`, healthcheck `GET /healthz`, domain `https://catical.sci1.uk`. Pair with a Postgres resource. Set `DATABASE_URL`, `BASE_URL=https://catical.sci1.uk`, Turnstile keys, and `LOG_LEVEL`. Migrations run on boot.

## Tests

Store integration tests need Postgres. CI should set `DATABASE_URL`. Locally:

```sh
docker compose up -d postgres
export DATABASE_URL='postgres://catical:catical@localhost:5433/catical?sslmode=disable'
go test ./...
```

Without `DATABASE_URL`, store tests are skipped; other packages still run. The compose file maps container 5432 to host **5433** so a local Postgres on 5432 can keep running.

## Cloudflare Turnstile

Create and manage **POST**s are gated when `TURNSTILE_SECRET` is set (pair with `TURNSTILE_SITE_KEY` for the widget). If `TURNSTILE_SECRET` is unset, Turnstile is off — intended for local/dev. Production should set both. ICS `GET` stays token-only; calendar clients never solve CAPTCHA.
