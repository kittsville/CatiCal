Ask mode blocks writing files, so this is the plan to drop into `/Users/kit/Projects/Catical` (for example `PLAN.md`) and run **one phase per agent**. Each phase is TDD: tests first, then the minimum code to pass, then stop.

**Locked product/architecture**

- Go service + Postgres. Domain `catical.sci1.uk`.
- No accounts. Create a mix → one feed URL + one manage URL. Lose manage → gone. Rotate tokens from manage; full URLs are shown **once** (hashes are not reversible).
- Tokens: per-row random salt + hash, lookup by `id` in the URL (not by hashing the URL alone).
- Cache: **Postgres last-good**. Fresh 5 minutes; stale up to 24 hours **only on origin error**; after 24 hours cache is unusable.
- GDPR: delete a mix if its **ICS feed** has not been fetched for 14 days. Manage views do not extend life. Create sets a 14-day grace via `last_request_at = now()`.
- Cloudflare Turnstile on forms: **last feature phase**, not in the HTML phase.
- Merge: `github.com/arran4/golang-ical`, parallel on-demand fetch, `singleflight`, SSRF allowlist (public http/https only), max 8 sources, body size cap, CRLF output, per-source UID prefix, optional `SUMMARY` prefix, editable `X-WR-CALNAME`.
- Never log source ICS URLs or path tokens.

---

### How each agent should work

1. Read this plan + `README.md` + code from **previous phases only**.
2. Implement **one phase**. Do not start the next.
3. **Red → green → refactor.** If a behaviour is specified, a test exists before production code.
4. `go test ./...` green. No leftover `t.Skip` / `t.FailNow` stubs.
5. Do not add Redis, a JS SPA, user emails, or RRULE expansion.
6. Handoff: list files created and what the next phase may use.

**Suggested layout** (create as you go, not all in phase 0):

```text
cmd/catical/main.go
internal/tokens/
internal/cachepolicy/
internal/icalmerge/
internal/fetch/
internal/store/
internal/refresh/
internal/http/
internal/reaper/
migrations/
testdata/ics/
```

---

### Phase 0 — Scaffold (tiny, still TDD)

**Goal:** Module runs tests in CI-shaped layout.

**Tests first**

- `TestHealthHandler` via `httptest`: `GET /healthz` → 200, body `ok`.

**Implement**

- `go mod init` (module path e.g. `sci1.uk/catical`).
- `cmd/catical/main.go` wiring only `/healthz`.
- `.gitignore`, keep README.
- No Postgres yet.

**Done when:** `go test ./...` passes; `go run ./cmd/catical` serves health.

---

### Phase 1 — Token salt+hash (pure)

**Goal:** Capability secrets are verifiable and not stored in plaintext.

**Rules**

- Generate: 16-byte salt, 32-byte secret (`crypto/rand`).
- Store: `salt` + `hash`. `hash = SHA-256(salt || secret)` (fixed, documented). Do **not** use Argon2 on the ICS hot path.
- `Verify(salt, hash, secret) bool` — constant-time compare.
- URL form (both links): `/{kind}/{id}/{secret}` with `id` = UUID (plaintext PK). Secret never in the DB.

**Tests first** (table-driven)

- Same salt+secret → same hash.
- Wrong secret / mutated salt → verify false.
- Verify does not panic on short/nil slices.
- Secrets from `Generate` are 32 bytes, salts 16, not all-zero.

**Done when:** package has no HTTP/DB; next phase can import `internal/tokens`.

---

### Phase 2 — Cache policy (pure)

**Goal:** One function owns freshness. No clocks inside callers later except `time.Now`.

**Behaviour** (`merged_at`, `now`, `hasBody`)

| Age of cache | Origin result | Action |
|---|---|---|
| no body | — | fetch; on error **fail** |
| `< 5m` | (do not fetch) | **serve cache** |
| `5m…24h` | success | **replace + serve new** |
| `5m…24h` | error | **serve stale cache** |
| `> 24h` | success | **replace + serve new** |
| `> 24h` | error | **fail** (do not serve) |

Same 5m / 24h windows apply later to **per-source** `last_ics`.

**Tests first:** table of ages (4m59s, 5m, 5m1s, 24h, 24h1s) × success/error/no-body.

**Done when:** `cachepolicy` has no I/O; refresh phase must call it, not reimplement it.

---

### Phase 3 — iCal merge (pure, fixtures)

**Goal:** Deterministic merge, lossless-enough component copy.

**Tests first** with `testdata/ics/`

- Two calendars, disjoint `VEVENT`s → both present.
- Duplicate `TZID` → one `VTIMEZONE`.
- Same UID from two sources → prefixed (`srcA:uid`, `srcB:uid`) and **all** events from a source share that prefix (exceptions keep the same UID as the master).
- `RRULE` / `RECURRENCE-ID` / `VALARM` / `X-` properties survive round-trip.
- Optional summary prefix: `Work: Standup`.
- Output uses **CRLF**; `VERSION:2.0`; `PRODID` identifies Catical; `X-WR-CALNAME` set from feed name.
- Malformed extra property does not drop the rest of a Google-like fixture (use skip/recover parse options).

**Implement** with `github.com/arran4/golang-ical` only. Copy components; do not map to a custom `Event` DTO. After `Serialize()`, normalise `\n` → `\r\n` without creating `\r\r\n`.

**Done when:** merge is a function `Merge(name string, sources []NamedCalendar) ([]byte, error)`.

---

### Phase 4 — Fetcher + SSRF (httptest, no Postgres)

**Goal:** Parallel HTTP GET of ICS URLs that cannot hit the internal network.

**Tests first**

- Two `httptest` servers: parallel fetch returns both bodies.
- One 500, one 200 → 200 body + error for the other (caller decides stale).
- Timeout (short client timeout + slow handler) → error.
- Body over cap (e.g. 2MiB) → error, connection not unbounded.
- Reject: `file:`, `ftp:`, non-http(s), `localhost`, `127.0.0.1`, `::1`, RFC1918, link-local, cloud metadata host `169.254.169.254`.
- Redirect to a blocked IP/host → error (do not follow into SSRF).
- Allow: http/https to the test server’s public URL.

**Implement:** `net/http` client, ~5s timeout, no redirects or custom `CheckRedirect` that re-validates. Cap sources at 8 in this layer or in HTTP later, but test the cap here if the function takes a slice.

**Logging:** log host + status, **never** raw URL (query strings on Google ICS are secrets). Tests should fail if a hook logger receives the full URL.

**Done when:** `fetch.GetAll(ctx, urls []string) []Result`.

---

### Phase 5 — Postgres schema + store (integration)

**Goal:** Persistence for feeds, sources, hashed tokens, cache blobs, GDPR timestamp.

**Schema (apply via numbered SQL in `migrations/`)**

- `feeds`: `id UUID PK`, `name`, `prefix_summaries bool`, `feed_token_salt/hash`, `manage_token_salt/hash`, `merged_ics BYTEA`, `merged_at TIMESTAMPTZ`, `last_request_at TIMESTAMPTZ NOT NULL`, `created_at`.
- `sources`: `id`, `feed_id` FK **ON DELETE CASCADE**, `url` (secret), `label`, `position`, `last_ics`, `last_success_at`, `last_error`, `last_attempt_at`.

**Tests first** (real Postgres: Testcontainers **or** `DATABASE_URL` in test env; document one and use it in CI)

- Insert feed + sources; get by id.
- Verify feed/manage secrets via `tokens.Verify`; wrong secret fails.
- Update merged blob + `merged_at`.
- Touch `last_request_at` only via an explicit store method (feed GET will call it).
- `DeleteFeed(id)` removes sources.
- `DeleteStaleFeeds(now, 14 days)` removes rows with `last_request_at < now-14d`, keeps fresher ones.

**Do not** store plaintext secrets. **Do not** implement HTTP.

**Done when:** `store` is the only package that talks SQL.

---

### Phase 6 — Refresh service (composition)

**Goal:** Orchestrate policy + fetch + merge + store. This is the core product logic.

**Tests first** with fake clock, fake fetcher, fake store (interfaces)

- Fresh merge (`<5m`) → fetcher **not** called; returns stored bytes; does not require `TouchLastRequest` here (HTTP will).
- Stale + all fetches OK → merge saved, new body returned.
- Stale + fetch error + merged age `<24h` → fetcher called, **old** merge returned, source `last_error` recorded.
- Stale + fetch error + merged age `>24h` → error, no body.
- Partial fetch failure → successful bodies update `last_ics`; failed source keeps previous `last_ics` if that source success is still within 24h; merge uses remaining last-good sources.
- `singleflight` by feed id: two concurrent refreshes → **one** fetch burst.
- Empty merge (no usable source bodies) → error.

**Implement:** `refresh.Service`. Max 8 sources enforced.

**Done when:** HTTP can be a thin wrapper over `store` + `refresh`.

---

### Phase 7 — HTTP: ICS feed

**Goal:** Subscribers can `GET` a combined calendar.

**Routes**

- `GET /c/{id}/{secret}.ics` (secret may be in the path without suffix; accept `.ics` trim).

**Tests first (`httptest` + store/refresh fakes or testdb)**

- Valid secret → `200`, `Content-Type: text/calendar; charset=utf-8`, body starts with `BEGIN:VCALENDAR`, CRLF.
- Wrong secret / unknown id → `404` (do not leak which).
- Refresh fail and no usable cache → `502` + valid tiny error ICS **or** empty 502; pick **502 with `text/plain`**, not HTML (clients are crawlers). Stick to 502 plain text.
- Successful GET calls `TouchLastRequest`.
- Invalid UUID → 404.
- `Cache-Control: private, max-age=300`.
- Response logging redacts secret path segments.

**Done when:** curl of a created row (can seed via store in test) returns merged ICS.

---

### Phase 8 — HTML create + manage (no Turnstile)

**Goal:** The UI from the discussion. Capability URLs are the authn.

**Pages**

- `GET /` — form: name, 1–8 source URL fields, optional labels, checkbox prefix summaries. `Referrer-Policy: no-referrer`. Noindex.
- `POST /` — create; **303** to a one-time success page **or** render success once: feed URL `https://catical.sci1.uk/c/{id}/{secret}.ics`, manage `https://catical.sci1.uk/m/{id}/{secret}`. Warn: manage is unrecoverable; feed URL will not be shown again.
- `GET /m/{id}/{secret}` — edit name, sources, prefix flag, last fetch errors (not raw source URLs in logs; showing URLs **on this page** is OK — holder is admin). Buttons: save, delete, rotate feed token, rotate manage token.
- `POST` save / delete / rotate with the manage secret. Delete → 303 home. Rotate → show **new** URL once.

**Tests first**

- POST create with 2 https URLs → 2xx/3xx, DB row, hashes not secrets, success body contains both `catical.sci1.uk` links.
- GET manage wrong secret → 404.
- POST save changes name/sources.
- POST delete → gone, subsequent feed GET 404.
- Rotate feed token → old feed 404, new secret works; manage URL unchanged.
- Rotate manage token → old manage 404.
- Reject >8 sources, empty name, non-https URL (same rules as fetcher).
- Success page for create is the **only** response that includes the feed secret (manage GET must not reprint the feed secret). Test that.

**Implement:** `html/template` only. Security headers: `Referrer-Policy: no-referrer`, `X-Frame-Options: DENY`, `X-Content-Type-Options: nosniff`, `Content-Security-Policy` tight enough for no inline JS if possible (Turnstile later will need a CSP exception).

**Done when:** a human can create, subscribe, edit, delete without accounts.

---

### Phase 9 — Rate limit + hardening

**Goal:** Quiet public site, not an open proxy.

**Tests first**

- N+1 creates from the same `httptest` peer hit a 429 (in-memory limiter is enough).
- Manage POST similarly limited.
- `GET /robots.txt` disallows `/m/`.
- Source URLs and tokens never appear in a captured `slog` output during a create+get fixture.

**Done when:** limiter is wired; no extra deps required (stdlib or a tiny token bucket).

---

### Phase 10 — GDPR reaper

**Goal:** Process-local worker, not cron.

**Tests first**

- Feed with `last_request_at` 15 days ago is deleted; 13 days ago is kept.
- After delete, feed GET is 404.
- Worker uses the same `DeleteStaleFeeds` as phase 5 (do not duplicate SQL).

**Implement:** goroutine in `main`, tick hourly, `DELETE` 14 days. Log **counts**, not ids+tokens.

**Done when:** `main` starts reaper; tests freeze time via injected `now`.

---

### Phase 11 — Cloudflare Turnstile (forms only)

**Goal:** Protect `POST /` create and manage POST (save/delete/rotate). ICS `GET` stays token-only (calendar clients cannot solve CAPTCHA).

**Tests first**

- Missing token → 400, no DB write.
- Fake verifier `ok` → create proceeds.
- Fake verifier `fail` → 403, no write.
- Skip verify when env `TURNSTILE_SECRET` empty **only in tests**; production must require it once configured. Prefer: `TURNSTILE_SECRET` unset → Turnstile off (local/dev); set → required. Document that.

**Implement:** server-side siteverify; widget on templates; CSP allows Cloudflare scripts. No accounts, no cookies required.

**Done when:** create/manage POSTs are gated; feed GET unchanged.

---

### Phase 12 — Docker, migrations on boot, deploy

**Goal:** Run on Coolify at `catical.sci1.uk`.

**Tests first**

- `migrations` apply twice without error (idempotent or goose version table).
- Optional: compile-only `go build -o /dev/null ./cmd/catical`.

**Implement**

- Multi-stage `Dockerfile` (distroless or `scratch` + CA certs). Listen `:8080`.
- Env: `DATABASE_URL`, `BASE_URL=https://catical.sci1.uk`, `TURNSTILE_SITE_KEY`, `TURNSTILE_SECRET`, `LOG_LEVEL`.
- Apply migrations on startup.
- README: create mix, copy two links, Google “add by URL”, 5m/24h cache, 14-day deletion, lost manage = gone.
- Compose snippet: app + Postgres, for local TDD.

**Deploy (human/Coolify, not fake it in unit tests):** app + Postgres, HTTPS at `catical.sci1.uk`, healthcheck `/healthz`.

---

### Agent order (do not parallelise 5–8)

`0 → 1 → 2 → 3 → 4 → 5 → 6 → 7 → 8 → 9 → 10 → 11 → 12`

Phases 1–3 can be one agent if small; do **not** merge 5 with 8.

---

### Out of scope for every agent

Redis, background ICS cron, reconstructing tokens from the DB, email recovery, Google OAuth, expanding recurrences, putting manage tokens in ICS, logging secrets, serving ICS without verifying the secret.

If you switch to Agent mode, the first agent should add this as `PLAN.md` and execute **phase 0 only**.