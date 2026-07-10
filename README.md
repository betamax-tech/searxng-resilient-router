# searxng-resilient-router

A resilient self-hosted [SearXNG](https://github.com/searxng/searxng) search
stack with automatic **proxy rotation**, **rate limiting**, **tiered failover**,
and **query preservation** — built so automated research tools (e.g. an
`local-researcher` MCP) never get the dreaded *"No search results found"* again.

## Why this exists

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
systemctl --user enable --now searxng-proxypool.timer
systemctl --user enable --now searxng-deadletter.timer
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
