# Catical

[![CI](https://github.com/kittsville/CatiCal/actions/workflows/build.yml/badge.svg)](https://github.com/kittsville/CatiCal/actions/workflows/build.yml)
[![WARN-LLM GENERATED](https://img.shields.io/badge/WARN-LLM%20GENERATED-FF6347)](https://github.com/40ants/ai-badges)

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
| `TURNSTILE_SITE_KEY` | Cloudflare widget (optional locally; production sitekey `0x4AAAAAAE9wkjLiDKI_GMpI`) |
| `TURNSTILE_SECRET` | Server-side siteverify; unset = Turnstile off (local/dev) |
| `TURNSTILE_HOSTNAMES` | Comma-separated hosts siteverify must return (optional; defaults to `BASE_URL` host, never loopback) |
| `LOG_LEVEL` | `debug` / `info` / `warn` / `error` |
| `SOURCE_COMMIT` | Git SHA for the footer; Coolify sets this on deploy |

Migrations apply on process start (version table; safe to run twice). Listen `:8080`. Healthcheck: `GET /healthz`.

## Production (Coolify)

Push to `main` runs GitHub Actions: tests, `docker build`, push `ghcr.io/kittsville/catical:latest`, then the same Coolify webhook curl as Recibase (`COOLIFY_TOKEN` + `COOLIFY_DEPLOY_WEBHOOK` repo secrets).

Coolify app is a **Docker Image** (not a git build): image `ghcr.io/kittsville/catical`, tag `latest`, listen `8080`, healthcheck `GET /healthz`, domain `https://catical.sci1.uk`. Pair with a Postgres resource. Set `DATABASE_URL`, `BASE_URL=https://catical.sci1.uk`, `TURNSTILE_SITE_KEY`, `TURNSTILE_SECRET`, and `LOG_LEVEL`. Optional `TURNSTILE_HOSTNAMES=catical.sci1.uk` if you do not want the host taken from `BASE_URL`. Migrations run on boot. The image includes `curl` and `wget` so Coolify’s healthcheck can exec inside the container.

## Tests

Store integration tests need Postgres. CI should set `DATABASE_URL`. Locally:

```sh
docker compose up -d postgres
export DATABASE_URL='postgres://catical:catical@localhost:5433/catical?sslmode=disable'
go test ./...
```

Without `DATABASE_URL`, store tests are skipped; other packages still run. The compose file maps container 5432 to host **5433** so a local Postgres on 5432 can keep running.

## Cloudflare Turnstile

Create and manage **POST**s are gated when `TURNSTILE_SECRET` is set (pair with `TURNSTILE_SITE_KEY` for the widget). Siteverify requires `success`, the surface action (`create` or `manage`), and a hostname in `TURNSTILE_HOSTNAMES` or the `BASE_URL` host. If `TURNSTILE_SECRET` is unset, Turnstile is off — intended for local/dev. Production should set both keys. ICS `GET` stays token-only; calendar clients never solve CAPTCHA.

The secret stays in Coolify (or local env). Do not commit it. Dummy siteverify responses with `invalid-input-response` mean the secret is valid; `invalid-input-secret` means it did not reach the process.
