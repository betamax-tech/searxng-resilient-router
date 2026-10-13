# searxng-resilient-router

A resilient self-hosted [SearXNG](https://github.com/searxng/searxng) search
stack with automatic **proxy rotation**, **rate limiting**, and **in-request
tiered failover** — built so automated tools (e.g. OpenWebUI, a `local-researcher`
MCP) get good multi-engine results instead of the datacenter-IP failure modes.

---

## ⚠️ ORIENTATION FOR ANYONE (agent or human) WORKING ON THIS — read first

**Who calls this / topology.** The single stable endpoint is the Go router on
`127.0.0.1:8899` (systemd *user* unit `searxng-router`). Behind it:
`:8890` = **direct** SearXNG (our real IP), `:8891` = **proxied** SearXNG (rotating
free proxies). Known consumers:
- **OpenWebUI** (`openwebui` container) — Admin ▸ Settings ▸ Web Search ▸ SearXNG
  Query URL = `http://host.docker.internal:8899/search?q=<query>`. This is why
  OWUI search quality == router quality.
- **`searxng.betamaxgroup.tech`** (Caddy) proxies to **`:8890` directly — it
  BYPASSES the router**. So "works in the browser but bad in OWUI" = a router bug,
  not a SearXNG bug. Compare `:8899` vs `:8890` with `&format=json` to see it.

**The #1 gotcha — engine names must match THIS instance.** The router restricts
each query to a rotating engine subset via `engines=`. Those names (in
`go/router.go` → `engineFamilies` / `independentPool`) MUST be engines that are
enabled AND actually return results from our datacenter IP. Measured 2026-07-22
on `:8890` (isolated per-engine): **working** = `google cse` (20, API-key, the
best), `google` (39 but flaky/CAPTCHA), `duckduckgo` (10), `bing` (10), `yandex`
(10); **dead (return 0 here)** = `startpage`, `brave`, `qwant`, `mojeek`, `yahoo`.
A stale list (asking for `google` scraper + the dead five, never `google cse`/`ddg`)
silently collapses every search to **bing-only ~10 junk results**. If quality
regresses, re-measure and update those lists. `google cse` has a ~100/**day** free
API cap the router can't count — it's the family primary with `google` as spare.

**No dead-letter / async retry (removed 2026-07-22).** The old design persisted
failed queries to disk and a timer re-ran them — but a background retry reaches
**no live caller**, and it fed a runaway loop that grew to **147k files / 585 MB**
and starved the live engine budgets. Replaced with a **bounded in-request retry
ladder** in `handleSearch` (direct-rotated → direct-all-engines → proxied-all,
looped until `maxWait`≈20s, then return empty). "Keep retrying for the user"
belongs at the caller's layer (OWUI), not here. The `searxng-deadletter.*` units,
`deadletter_retry.go`, and `cache/deadletter/` are gone — do not resurrect them.

**Build.** `go/router.go` + `go/analytics.go` are one program (both `package main`,
only router.go has `main()`) → build them TOGETHER. `bash go/build.sh` does this;
then `systemctl --user restart searxng-router`.

---

## Provider/tier waterfall (2026-10-13)

The router now sits in front of **SearXNG plus four paid providers** (Exa,
Tavily, Brave, TinyFish), chosen per query via an optional `tier=` param
(or `X-Search-Tier` header): `crucial` | `standard` | `bulk`, default
`standard`. **Callers that already pick their own engines
(`engines=`/`engine=` param, a SearXNG `!bang`, or a non-`general`
`categories=`) are untouched** — they keep exactly today's direct→proxied
SearXNG behavior (the P2/P3 fixes). Everything else is tier-classified and
routed through a provider waterfall where SearXNG itself is one stop among
several.

### Tier classification (rules only, no model)

A query is forced to **`crucial`**, overriding whatever `tier=` was
requested, when it contains a PH legal citation pattern (`G.R. No.`,
`A.M. No.`, `A.C. No.`, `MTJ-`, `RTJ-`, `Republic Act No.`, `R.A. No.`) or a
`site:` filter targeting `lawphil.net`, `elibrary.judiciary.gov.ph`,
`sc.judiciary.gov.ph`, `officialgazette.gov.ph`, or `chanrobles.com`. See
`go/tiers.go` → `classifyTier()`. The rule that fired is surfaced as
`router_tier_rule` in the response and in the analytics log.

A **shadow classifier hook** (`go/tiers.go` → `ShadowClassifier`) exists for
a *future* ML/heuristic tier suggestion, logged next to the rules-based
tier for comparison — it is `nil` today and never influences routing. No
model is wired in this codebase.

### Provider order per tier — DATA, not code

`config/tier_providers.json` (re-read fresh on every request — edit and the
next request picks it up, no rebuild/restart needed):

```json
{
  "crucial":  ["exa", "tavily", "tinyfish", "brave", "searxng"],
  "standard": ["tinyfish", "searxng", "brave", "exa"],
  "bulk":     ["searxng", "tinyfish"]
}
```

Failover happens on error, 429/timeout, an empty result set, OR a
provider's cap being reached — see `go/tiered_router.go`. The failed-over
chain is returned to the caller as `router_failovers` and logged.
`"searxng"` is a special provider name that delegates to the existing
`handleSearch()` direct→proxied logic (nothing about that logic changed).

### Hard spend caps — "zero expenditure, ever"

Every paid provider has a hard, disk-persisted cap at ~90% of its free
allowance, enforced in `go/caps.go` **before** any call is made:

| Provider | Cap | Mode | Why |
|---|---|---|---|
| Exa | $9.00/month | cost (actual `costDollars`) | $10 free credit/mo, resets 1st, no card |
| Tavily | 900/month | count | 1,000 free credits/mo, no card |
| Brave | 900/month | count | $5 free credit/mo ≈ 1,000 req; **card on file — overage WOULD bill** |
| TinyFish | 10,800/day, 450/hr, 25/min | count | free ≤12,000/day, 30/min, 500/hr |

If usage can't be determined — a corrupt on-disk cap file, or a cost-mode
provider that returns HTTP 200 without a usable cost field — the limiter
**fails closed**: either every provider is blocked (corrupt state) or that
one provider is saturated for the rest of the period. Usage is recorded for
**any confirmed HTTP 200 response**, even one this router failed to parse
or that legitimately found zero results — a billed call must never go
untracked. State lives at `cache/provider_caps.json` (gitignored,
atomic writes, survives restarts). See it live at `GET /stats` →
`provider_caps`.

### Cache

Identical `(normalized query, tier, categories/language/safesearch/pageno)`
requests are served from disk for 24h, capped at 256MB total
(`cache/search/`, gitignored, oldest-evicted-first). See `go/searchcache.go`.

### Analytics / `/stats`

Every tiered request logs one JSON line (same daily-rotated file under
`cache/analytics/`) with `search_tier`, `tier_rule`, `shadow_tier`,
`provider_used`, `providers_tried`, `failovers`, `cost_usd`, `cache_hit` —
see `go/analytics.go` → `recordTieredEvent()`. `GET /stats` now also
includes `provider_caps`: usage vs cap, per provider, per period.

### Keys

`EXA_API_KEY`, `TAVILY_API_KEY`, `TINYFISH_API_KEY`, `BRAVE_API_KEY` live in
`.env` (gitignored) and are loaded by the systemd unit via
`EnvironmentFile=/home/cmark/server/searxng-rotation/.env`. Never logged,
never committed.

---

## R1/R2 (2026-10-13 anti-blocking investigation) — engine retune + proxy tier retirement

**R1 — only the six engines verified live, globally-broken scrapers
disabled.** A 94-public-instance cross-check (searx.space) showed
startpage/duckduckgo/qwant/yahoo/mojeek/brave/google scrapers are 77-96%
broken **globally**, not just for us. `searxng/settings.yml` and
`settings.proxied.yml` now enable only `bing`, `yandex`, `google cse`
(upstream default), `mwmbl`, `yep`, `wiby`; the dead six are kept as
explicit `disabled: true` entries. `go/router.go`'s `engineFamilies` /
`independentPool` / `engineRateDefaults` match. Brave's web index is now
reached through the Go router's own `braveapi`-style adapter (provider
waterfall above), not SearXNG's dead scraper.

**R2 — the free rotating-proxy tier is retired.** It recovered zero
engines, *lost* a working Yandex, and exposed every legal-research query to
20 anonymous third parties. `searxng-router.service` now sets
`ROUTER_DIRECT_ONLY=1`, pinning all SearXNG-tier traffic to the direct
instance; `searxng-proxypool.timer` has been `disable --now`'d. **The code
is untouched** (`go/proxypool.go`, `go/build.sh`'s proxypool build target) —
to re-enable:

```bash
# 1. Remove (or set to 0/false) ROUTER_DIRECT_ONLY in systemd/searxng-router.service
#    and ~/.config/systemd/user/searxng-router.service, then:
systemctl --user daemon-reload
systemctl --user restart searxng-router.service

# 2. Resume proxy pool refresh:
systemctl --user enable --now searxng-proxypool.timer
```

---

## Why this exists (original design notes)

A plain self-hosted SearXNG on a datacenter IP breaks in three ways:

1. **Stale engine scrapers** — an out-of-date image silently returns 0 results.
2. **Engine CAPTCHA / rate-limit blocks** — Google/DDG/Brave/etc. block the
   server's datacenter IP; SearXNG returns **HTTP 200 with `results: []`**, which
   a normal load-balancer treats as success and never fails over.
3. **No query preservation** — a failed search is just lost.

This stack fixes all three.

## Architecture

```
research tools ─────────►  Go router  :8899   (single stable endpoint)
                                │
        ┌───────────────────────┼───────────────────────────┐
        ▼                        ▼                            ▼
  Tier 1 DIRECT            Tier 2 PROXIED               Tier 3 DEAD-LETTER
  SearXNG :8890            SearXNG :8891                persist query to disk,
  your real IP            engine traffic → rotating    return "queued_for_retry"
  (fast, preferred)       free-proxy pool (round-robin) (never dropped)

  Router policy:
   • Success = HTTP 200 AND results[] non-empty (0 results = failure → next tier).
   • DIRECT is rate-limited (token bucket 6/min sustained, 15 burst) AND
     concurrency-gated (max 2). Exceeding either → spill to PROXIED.
   • A 429/403 from direct trips a 180s cooldown (skip direct, use proxied).
   • The PROXIED tier is intentionally NOT rate-limited (rotating IPs absorb it).
```

Only the **direct** tier exposes your server's real IP, so only it is throttled.
The **proxied** tier rides on rotating third-party IPs.

## Components

| Path | What it is |
|------|-----------|
| `searxng/` | The two-instance SearXNG docker-compose stack (direct + proxied). |
| `go/router.go` | Front-door router (`:8899`). Tiered failover, rate limit, dead-letter. |
| `go/proxypool.go` | Incremental self-healing proxy pool. Health-checks proxies vs Google, writes the proxied instance's managed proxy block. |
| `go/deadletter_retry.go` | Reprocesses queued failed queries through the router. |
| `systemd/` | User units: router service + proxypool timer + deadletter timer. |
| `cache/` | Generated state (gitignored): active proxy pool, candidate backlog, dead-letter queue. |

### Proxy pool design (incremental, not full-scan)

`proxypool` keeps a small **active pool (~20)** of healthy proxies:

1. Re-verify **only** the existing 20 (cheap).
2. Drop failures.
3. If below target, refill from a cached **candidate backlog** in small batches.
4. Only fetch the public source lists when the backlog runs dry.

Sources are popular public proxy-list GitHub repos + the geonode API — all free,
no keys. A proxy only counts as healthy if it can actually reach Google.

## Fresh deployment (runbook)

### 1. Prerequisites
- Docker + docker compose
- Go (to build the binaries):
  ```bash
  curl -sL https://go.dev/dl/go1.26.5.linux-amd64.tar.gz | tar -C ~/.local -xz
  export PATH="$HOME/.local/go/bin:$PATH"
  ```

### 2. Configure secrets
```bash
cd searxng
cp .env.example .env
# edit .env — set SEARXNG_SECRET (openssl rand -hex 32) and SEARXNG_BASE_URL
```

### 3. Start the SearXNG instances
```bash
cd searxng
docker compose up -d          # brings up :8890 (direct) and :8891 (proxied)
```

### 4. Build the Go binaries
```bash
cd ../go
./build.sh                    # → searxng-router, proxypool, deadletter-retry
```

### 5. Seed the proxy pool
```bash
./proxypool                   # populates cache/ and the proxied managed block
docker compose -f ../searxng/docker-compose.yml restart searxng-proxied
```

### 6. Install systemd user services
```bash
cp ../systemd/* ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now searxng-router.service
# NOTE (R2, 2026-10-13): the free-proxy tier is RETIRED by default —
# searxng-router.service ships with ROUTER_DIRECT_ONLY=1. Do NOT enable
# searxng-proxypool.timer on a fresh install unless you've deliberately
# re-enabled the proxy tier (see "R1/R2" section above for why, and how).
systemctl --user enable --now searxng-health-canary.timer
# survive logout:
loginctl enable-linger "$USER"
```

### 7. Point your research tools at the router
Set the SearXNG endpoint to the **router**, not the raw instances. The router
binds both `127.0.0.1:8899` (host-side tools) and `172.17.0.1:8899` (docker
bridge, so containers reach it via `host.docker.internal:8899`). It is never
bound to a public interface.

**OpenCode `local-researcher` MCP** (host-side):
```json
"environment": { "SEARXNG_ENDPOINT": "http://localhost:8899" }
```

**Open WebUI `deep_research` / web search** (containerized) — note Open WebUI
persists this in its DB, which **overrides the env var**, so set both:
```bash
# .env
SEARXNG_QUERY_URL=http://host.docker.internal:8899/search?q=<query>
```
```sql
-- webui.db (config table) — the DB value wins after first admin-panel save
UPDATE config SET value='"http://host.docker.internal:8899/search?q=<query>"'
  WHERE key='web.search.searxng_query_url';
```
The router forwards the caller's full SearXNG parameter set (`categories`,
`pageno`, `language`, `safesearch`, …) unchanged, so category routing is
preserved through the resilience tiers.

## Verify
```bash
curl -s http://127.0.0.1:8899/healthz | jq
curl -s "http://127.0.0.1:8899/search?q=hello+world" | jq '.results | length'
```
`/healthz` shows live direct in-flight count, cooldown state, and rate tokens.

## Tuning (edit `go/router.go` constants, then rebuild)

| Constant | Default | Meaning |
|----------|---------|---------|
| `directMax` | 2 | Concurrent direct searches before spilling to proxied. |
| `rateBurst` | 15 | Token-bucket capacity (max burst on direct). |
| `rateRefillRate` | 6/60 | Sustained direct requests per second (6/min). |
| `cooldown` | 180s | Direct skip window after a 429/403. |

Proxy pool sizing lives in `go/proxypool.go` (`target`, `batch`, `checkWorkers`).

## Notes / known constraints
- **Public SearXNG instances can't be a JSON fallback** — all surveyed public
  instances disable `format=json`. Resilience comes from the proxied instance +
  proxy rotation, not public instances.
- **Content fetching / JS rendering** is handled downstream by the research
  tool's reader (e.g. Jina Reader), not by this stack. SearXNG only returns the
  list of result URLs; it never renders result pages.
- Keep the SearXNG image current (`docker compose pull && up -d`) — stale
  scrapers are a common cause of silent 0-result failures.
