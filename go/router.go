// router.go — front-door search router for the self-hosted SearXNG stack.
//
// A single stable endpoint (:8899) that ALL research tools point at (drop-in
// SearXNG /search API). It owns the resilience policy so the tools stay dumb:
//
//	Tier 1  DIRECT   (127.0.0.1:8890) — preferred fast path.
//	Tier 2  PROXIED  (127.0.0.1:8891) — rotating proxy pool; used when the direct
//	                                    tier is saturated or cooling down.
//	Tier 3  DEAD-LETTER               — if every tier fails, the query is persisted
//	                                    to disk (never dropped) and a structured
//	                                    "queued_for_retry" envelope is returned.
//
// Load policy ("prefer direct, spill to proxy as aggressively as needed"):
//   - Up to DirectMax concurrent searches go DIRECT.
//   - The (DirectMax+1)-th concurrent search spills to PROXIED.
//   - A direct 429/403 sets a cooldown -> direct is skipped for CooldownS seconds.
//
// Success = HTTP 200 AND a non-empty results array. A 200 with results:[]
// (engines CAPTCHA-blocked) is treated as FAILURE and falls through to the next
// tier — the exact bug a plain nginx/Caddy LB could not detect.
//
// Stdlib only — compiles to a single static binary, zero dependencies.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ------------------------------------------------------------------ config
const (
	listenPort = "8899"
	direct     = "http://127.0.0.1:8890"
	proxied    = "http://127.0.0.1:8891"

	directMax      = 2                // concurrent direct searches before spilling
	cooldown       = 180 * time.Second
	directTimeout  = 20 * time.Second
	proxiedTimeout = 45 * time.Second

	// PER-IP outbound rate limits. Search engines rate-limit per source IP at
	// ~6/min from a datacenter IP, so each egress IP gets its own budget:
	//   - DIRECT  = this server's real IP        -> 6/min, burst 15.
	//   - PROXIED = pool of N rotating proxy IPs  -> N×6/min aggregate (SearXNG
	//     spreads proxied requests across all N proxies internally), so total
	//     sustainable engine load scales with the proxy pool.
	perIPRate  = 6.0 / 60.0 // tokens/sec per egress IP (6 per minute)
	perIPBurst = 15         // burst capacity per egress IP

	// When both buckets are dry, a request waits up to this long for a token
	// before giving up and dead-lettering (bounded latency, never blocks forever).
	maxWait = 20 * time.Second // in-request retry ceiling: hold a failing search this long, escalating, before returning empty

	activePoolFile = "/home/cmark/server/searxng-rotation/cache/active_pool.json"
)


// defaultDirectOnly pins ALL searches to the direct tier (no proxy spillover)
// when ROUTER_DIRECT_ONLY is truthy. Per-request ?direct= overrides this.
var defaultDirectOnly = func() bool {
	v := os.Getenv("ROUTER_DIRECT_ONLY")
	return v == "1" || v == "true" || v == "yes"
}()

// ------------------------------------------------------------------ state
var (
	directInflight int64        // atomic
	cooldownUntil  atomic.Int64 // unix-nano; direct skipped until this time

	// Direct IP bucket: fixed 6/min, burst 15.
	directBucket = newTokenBucket(perIPBurst, perIPRate)

	// Proxied-pool bucket: capacity/refill = (proxy count) × per-IP budget,
	// refreshed periodically from the active proxy pool so it auto-scales as the
	// pool heals. Starts at 1×IP until the first refresh.
	proxiedBucket = newTokenBucket(perIPBurst, perIPRate)
)

// refreshProxiedCapacity resizes the proxied-pool bucket to (proxy count) × the
// per-IP budget, reading the live pool. Run once at boot + on a ticker.
func refreshProxiedCapacity() {
	n := readProxyCount()
	if n < 1 {
		n = 1
	}
	proxiedBucket.Resize(float64(perIPBurst*n), perIPRate*float64(n))
}

func readProxyCount() int {
	b, err := os.ReadFile(activePoolFile)
	if err != nil {
		return 1
	}
	var doc struct {
		Count int `json:"count"`
	}
	if json.Unmarshal(b, &doc) != nil || doc.Count < 1 {
		return 1
	}
	return doc.Count
}

// tokenBucket is a lazy (no background goroutine) rate limiter. Tokens refill
// continuously at refillPerSec up to capacity; TryTake removes one if available.
type tokenBucket struct {
	mu         sync.Mutex
	capacity   float64
	tokens     float64
	refillRate float64 // tokens per second
	last       time.Time
}

func newTokenBucket(capacity int, refillPerSec float64) *tokenBucket {
	return &tokenBucket{
		capacity:   float64(capacity),
		tokens:     float64(capacity), // start full
		refillRate: refillPerSec,
		last:       time.Now(),
	}
}

func (b *tokenBucket) TryTake() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	b.tokens += now.Sub(b.last).Seconds() * b.refillRate
	if b.tokens > b.capacity {
		b.tokens = b.capacity
	}
	b.last = now
	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}

func (b *tokenBucket) Available() float64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	t := b.tokens + now.Sub(b.last).Seconds()*b.refillRate
	if t > b.capacity {
		t = b.capacity
	}
	return t
}

// Resize changes capacity + refill rate (e.g. when the proxy pool grows/shrinks),
// clamping current tokens to the new capacity.
func (b *tokenBucket) Resize(capacity, refillPerSec float64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.capacity = capacity
	b.refillRate = refillPerSec
	if b.tokens > capacity {
		b.tokens = capacity
	}
}

// Refund returns one token (used when a reserved slot won't be consumed).
func (b *tokenBucket) Refund() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.tokens < b.capacity {
		b.tokens++
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// ------------------------------------------------------------------ engine rotation
//
// SearXNG has no built-in per-engine rate limiter or rotation, so the router
// does it: each engine has its own token bucket (tolerance-based budget), and
// each query is sent to a SUBSET of engines chosen so no single engine endpoint
// gets hammered into suspension. Engines are grouped into INDEX FAMILIES —
// members of a family serve the same underlying index, so we only need ONE
// member per family per query, and we pick whichever member still has budget
// (a "hot spare"): e.g. serve Google's index via startpage when google is spent.
//
// Budgets are per-minute, tunable via env ROUTER_ENGINE_RATE_<NAME> (e.g.
// ROUTER_ENGINE_RATE_GOOGLE=5). Researched tolerance defaults below.

type engineDef struct {
	name       string
	perMin     float64
	bucket     *tokenBucket
}

// index families: pick ONE available member per family per query (order = preference).
//
// IMPORTANT: these names must match engines that are ENABLED and actually RETURN
// results on THIS SearXNG instance from our datacenter IP. Measured 2026-07-22
// against the direct instance (:8890), isolated per-engine queries:
//
//     google cse  -> 20  (Google's JSON *API* — reliable, but ~100 queries/DAY cap)
//     google      -> 39  (scraper — high yield but FLAKY: frequently CAPTCHA'd to 0)
//     duckduckgo  -> 10  (works)
//     bing        -> 10  (works, most tolerant)
//     yandex      -> 10  (works)
//     startpage / brave / qwant / mojeek / yahoo -> 0  (blocked or not usable here)
//
// The previous list asked for `google` (flaky scraper, not the API) and the five
// dead engines, and never asked for `google cse` or `duckduckgo` — so a rotated
// query kept collapsing to bing-only (~10 junk results). Families below use the
// engines that work; `google cse` is preferred for Google's index with the
// `google` scraper as a hot-spare, and bing/duckduckgo rotate for Bing's index.
// To retune budgets without a rebuild, set ROUTER_ENGINE_RATE_<NAME> (uppercased;
// note "google cse" has a space and so cannot be overridden via env — edit here).
//
// P4 (2026-10-13): the "google" scraper hot-spare below was DEAD WEIGHT — it is
// not in `/config`'s enabled engine list on :8890 despite `disabled: false` in
// settings.yml, so when `google cse`'s ~100/day quota trips (it does, every
// day), the family silently contributed nothing. Swapped for `startpage`, which
// actually proxies Google's index and IS enabled; it gets CAPTCHA'd sometimes
// too, but self-heals within ~2min via ban_time_on_fail, unlike `google` which
// structurally never works on this instance. Re-measure per README's own
// 2026-07-22-style note if quality regresses again.
var engineFamilies = [][]string{
	{"google cse", "startpage"}, // Google's index — cse (API, reliable) preferred; startpage proxies Google when cse is spent
	{"bing", "duckduckgo"},      // Bing's index (ddg re-serves Bing) — rotate to avoid hammering one
}

// independent pool: rotated, up to independentsPerQuery picked per query by most-tokens
var independentPool = []string{"yandex"}

const independentsPerQuery = 1

// tolerance-based default budgets (requests/min per engine from a datacenter IP).
// "google cse" is API-key based (no per-request block, but a hard ~100/DAY free
// quota the router can't count) so keep it modest; when its per-min budget is
// spent the family falls back to `startpage`, conserving the daily cap. "google"
// has no budget entry (P4): it's not enabled on this instance, so a budget for
// it would be dead weight — see engineFamilies comment above.
var engineRateDefaults = map[string]float64{
	"google cse": 5,
	"startpage":  6,
	"bing":       12, "duckduckgo": 12,
	"yandex": 8,
}

var engines = func() map[string]*engineDef {
	m := map[string]*engineDef{}
	for name, def := range engineRateDefaults {
		rate := def
		if v := os.Getenv("ROUTER_ENGINE_RATE_" + strings.ToUpper(name)); v != "" {
			if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 {
				rate = f
			}
		}
		// burst = 1 minute's worth (min 3) so short bursts are absorbed.
		burst := int(rate)
		if burst < 3 {
			burst = 3
		}
		m[name] = &engineDef{name: name, perMin: rate, bucket: newTokenBucket(burst, rate/60.0)}
	}
	return m
}()

// hasBang reports whether the query contains a SearXNG engine/category bang
// (a token beginning with '!' , e.g. "!google", "!!wikipedia", "!general").
// When present, the caller has explicitly chosen where to search, so the router
// must not inject its own engines= (which would fight SearXNG's bang parsing).
func hasBang(q string) bool {
	for _, tok := range strings.Fields(q) {
		if strings.HasPrefix(tok, "!") {
			return true
		}
	}
	return false
}

// hasSiteFilter reports whether the query contains a `site:` boolean filter
// (case-insensitive — SearXNG/Bing/Google all accept "Site:" too). P3: Yandex
// and the DuckDuckGo-HTML scraper in SearXNG are unreliable with Google/Bing-
// style `site:` syntax (extended_research's legal "(site:lawphil.net OR ...)"
// queries need Bing/Google-cse specifically). Treated the same as an explicit
// caller engine choice: rotation is skipped and the query falls through to
// SearXNG's full enabled engine set, which includes the site:-honoring engines,
// rather than being pinned to whatever the rotated 2-3 engine subset happens
// to contain this request.
func hasSiteFilter(q string) bool {
	return strings.Contains(strings.ToLower(q), "site:")
}

// decideRotation reports whether per-query engine rotation should apply, given
// whether the caller already chose engines (explicit engines=/engine= param,
// a SearXNG bang, or a site: filter — see hasSiteFilter) and the caller's
// `categories` param. Pure/testable; handleSearch() is the only caller.
//
// P2 (2026-10-13): a caller-specified NON-general category (science/it/news/...)
// already has its own curated, SearXNG-enabled engine set (arxiv/pubmed/
// stackoverflow/bing news/...). Rotating + deleting that category on every
// request silently discarded every consumer's category routing (OWUI
// extended_research legal/academic/it/news, local-researcher's categories
// param) — the categories param never actually reached SearXNG. Rotation now
// only applies to the general/unset case, which is where engine-suspension
// protection actually matters (the generic-web giants are what gets CAPTCHA'd).
func decideRotation(callerChoseEngines bool, categories string) bool {
	isGeneralOrUnset := categories == "" || categories == "general"
	return !callerChoseEngines && isGeneralOrUnset
}

// selectEngines picks the engine subset for this query, spending one token per
// chosen engine. Returns the comma-separated engines= value (empty = let SearXNG
// use its enabled defaults, i.e. all — a safe fallback when everything's spent).
func selectEngines() string {
	var chosen []string

	// one available member per index family (hot-spare fallback within family)
	for _, fam := range engineFamilies {
		for _, name := range fam {
			e := engines[name]
			if e != nil && e.bucket.TryTake() {
				chosen = append(chosen, name)
				break // got this family's index; move on
			}
		}
	}

	// rotate independents: pick the ones with the most tokens, up to N
	type it struct {
		name string
		tok  float64
	}
	var pool []it
	for _, name := range independentPool {
		if e := engines[name]; e != nil {
			pool = append(pool, it{name, e.bucket.Available()})
		}
	}
	sort.Slice(pool, func(i, j int) bool { return pool[i].tok > pool[j].tok })
	added := 0
	for _, p := range pool {
		if added >= independentsPerQuery {
			break
		}
		if engines[p.name].bucket.TryTake() {
			chosen = append(chosen, p.name)
			added++
		}
	}

	return strings.Join(chosen, ",")
}

// tryAcquireDirect reserves a direct slot if: (1) not cooling down after a
// rate-limit, (2) within the outbound rate budget (token bucket protecting our
// real IP), and (3) under the concurrency limit. Any failure -> spill to proxied.
func tryAcquireDirect() (bool, string) {
	if time.Now().UnixNano() < cooldownUntil.Load() {
		return false, "cooldown"
	}
	if !directBucket.TryTake() {
		return false, "rate-budget"
	}
	for {
		n := atomic.LoadInt64(&directInflight)
		if n >= directMax {
			directBucket.Refund() // spilling, not using direct — give the token back
			return false, "saturated"
		}
		if atomic.CompareAndSwapInt64(&directInflight, n, n+1) {
			return true, ""
		}
	}
}

func releaseDirect() { atomic.AddInt64(&directInflight, -1) }

func tripCooldown() {
	cooldownUntil.Store(time.Now().Add(cooldown).UnixNano())
	log.Printf("direct tier cooldown tripped for %s", cooldown)
}

// upstreamSearch queries a SearXNG instance. Returns (status, body, resultCount).
// resultCount = -1 when the body can't be parsed as a results envelope.
// It forwards the caller's full parameter set (categories, pageno, language,
// safesearch, …) unchanged, only forcing format=json and ensuring q is present.
func upstreamSearch(ctx context.Context, base string, params url.Values) (int, []byte, int) {
	q := cloneValues(params)
	q.Set("format", "json")
	u := base + "/search?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, nil, -1
	}
	req.Header.Set("User-Agent", "searxng-router/1.0")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, -1
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	n := -1
	var env struct {
		Results []json.RawMessage `json:"results"`
	}
	if json.Unmarshal(body, &env) == nil {
		n = len(env.Results)
	}
	return resp.StatusCode, body, n
}

func cloneValues(v url.Values) url.Values {
	out := make(url.Values, len(v))
	for k, vals := range v {
		cp := make([]string, len(vals))
		copy(cp, vals)
		out[k] = cp
	}
	return out
}

// tierResult carries everything the analytics needs from one upstream attempt.
type tierResult struct {
	body         []byte
	status       int
	results      int
	latencyMS    int64
	unresponsive []unresponsiveEng
}

func tryTier(name, base string, params url.Values, timeout time.Duration) tierResult {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	t0 := time.Now()
	status, body, n := upstreamSearch(ctx, base, params)
	lat := time.Since(t0).Milliseconds()
	if name == "direct" && (status == http.StatusTooManyRequests || status == http.StatusForbidden) {
		tripCooldown()
	}
	tr := tierResult{status: status, results: n, latencyMS: lat, unresponsive: parseUnresponsive(body)}
	if status == http.StatusOK && n > 0 {
		log.Printf("  %s: OK (%d results)", name, n)
		tr.body = body
		return tr
	}
	log.Printf("  %s: FAIL (status=%d, results=%d)", name, status, n)
	return tr
}

// searchOutcome is the router's result for one query, including metadata the
// handler surfaces to the caller (headers + additive JSON keys).
type searchOutcome struct {
	status       int
	body         []byte
	enginesUsed  string            // engines= we asked SearXNG for ("" = defaults)
	tier         string            // direct | proxied | none
	unresponsive []unresponsiveEng // engines SearXNG reported down/suspended
}

func handleSearch(params url.Values, strict bool) searchOutcome {
	query := params.Get("q")

	// Per-query engine rotation: unless the caller has ALREADY chosen engines,
	// choose a per-index-family subset (spending per-engine budget) so no single
	// engine endpoint gets hammered into suspension. Empty selection (all budgets
	// spent) falls back to SearXNG's enabled defaults.
	//
	// The caller's explicit choice ALWAYS wins and disables rotation, via:
	//   - an `engines=` (or `engine=`) query param, OR
	//   - a SearXNG !bang / :bang in the query text (e.g. "!google ...", "!ddg ..."),
	//     which SearXNG parses itself — we must not fight it with an engines= inject, OR
	//   - a `site:` filter in the query text (P3) — Yandex and the DuckDuckGo
	//     scraper don't reliably honor site:, so fall through to SearXNG's full
	//     enabled set (which includes Bing/google cse) instead of the rotated subset.
	cat := params.Get("categories")
	callerChoseEngines := params.Get("engines") != "" || params.Get("engine") != "" ||
		hasBang(query) || hasSiteFilter(query)
	if decideRotation(callerChoseEngines, cat) {
		if sel := selectEngines(); sel != "" {
			params.Set("engines", sel)
			// P2: do NOT params.Del("categories") here. Deleting it was the bug —
			// the caller's categories param (science/it/news/legal routing) never
			// reached SearXNG because rotation always wins for general/unset
			// categories anyway (decideRotation only rotates when categories is
			// "" or "general"), so leaving it set is harmless for THIS branch and
			// keeps the param alive for any downstream tier retry.
			log.Printf("  engines rotated -> %s", sel)
		} else {
			log.Printf("  engines: all budgets spent -> SearXNG defaults")
		}
	} else {
		log.Printf("  engines: caller-specified (bang/param/site:/category=%s) -> rotation skipped", cat)
	}
	enginesUsed := params.Get("engines")

	// Direct-only mode: pin to the direct instance, never spill to proxied.
	directOnly := defaultDirectOnly
	if v := params.Get("direct"); v != "" {
		directOnly = v == "1" || v == "true" || v == "yes"
		params.Del("direct")
	}

	// Bounded, IN-REQUEST retry ladder. We hold the request open and keep trying,
	// escalating coverage, until something non-empty comes back or maxWait elapses;
	// then we return best-effort. There is NO disk dead-letter / async retry: a
	// recovered result only helps the caller that is STILL WAITING right now, and
	// a background retry into a drawer nobody reads just bred an unbounded backlog.
	// If a caller wants "keep trying," that belongs at their layer (e.g. OWUI).
	//
	//   1) DIRECT, rotated engine subset  (fast path — spares engines)
	//   2) DIRECT, ALL enabled engines    (max coverage — strongest recovery)
	//   3) PROXIED, ALL enabled engines   (rotating IPs — real direct-IP blocks; skipped in direct-only)
	//
	// Steps 2–3 repeat until the deadline so a transient blip self-heals here.
	rotated := cloneValues(params) // engines= from selection (may be "")
	allEng := cloneValues(params)
	allEng.Del("engines") // force SearXNG's full enabled set

	var last tierResult
	deadline := time.Now().Add(maxWait)
	firstPass := true
	for {
		// 1) direct, rotated subset — worth trying once (its engines don't change mid-request)
		if firstPass {
			firstPass = false
			if ok, _ := tryAcquireDirect(); ok {
				last = tryTier("direct", direct, rotated, directTimeout)
				releaseDirect()
				if last.body != nil {
					return finishSearch(query, enginesUsed, "direct", last, strict)
				}
			}
		}
		// 2) direct, all engines
		if ok, _ := tryAcquireDirect(); ok {
			last = tryTier("direct", direct, allEng, directTimeout)
			releaseDirect()
			if last.body != nil {
				return finishSearch(query, "", "direct", last, strict)
			}
		}
		// 3) proxied, all engines (unless pinned to direct-only)
		if !directOnly && proxiedBucket.TryTake() {
			last = tryTier("proxied", proxied, allEng, proxiedTimeout)
			if last.body != nil {
				return finishSearch(query, "", "proxied", last, strict)
			}
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(400 * time.Millisecond)
	}

	// Exhausted within maxWait — respond AS SEARXNG (HTTP 200 + empty results) so
	// clients that call raise_for_status() don't break. Opt-in X-Router-Strict /
	// ?strict=1 returns 503 instead. Nothing is persisted: the only consumer is the
	// caller waiting on THIS request, which has now given up.
	log.Printf("  ALL TIERS EXHAUSTED within %s -> returning empty (no dead-letter)", maxWait)
	env := map[string]any{
		"query": query, "number_of_results": 0, "results": []any{},
		"answers": []any{}, "corrections": []any{}, "infoboxes": []any{},
		"suggestions": []any{}, "unresponsive_engines": []any{},
		"router_status": "exhausted",
	}
	status := http.StatusOK
	if strict {
		status = http.StatusServiceUnavailable
	}
	oc := searchOutcome{status: status, body: mustJSON(env), enginesUsed: enginesUsed,
		tier: "none", unresponsive: last.unresponsive}
	recordEvent(searchEvent{
		TS: time.Now().UTC().Format(time.RFC3339Nano), Query: query, Tier: "none",
		Engines: enginesUsed, Results: 0, Status: last.status, LatencyMS: last.latencyMS,
		Unresponsive: last.unresponsive, Outcome: "exhausted",
	})
	return oc
}

// finishSearch records analytics for a successful upstream hit, injects additive
// router metadata into the JSON body, and returns the outcome.
func finishSearch(query, enginesUsed, tier string, tr tierResult, strict bool) searchOutcome {
	// Inject additive router_* keys (SearXNG clients ignore unknown top-level keys).
	body := tr.body
	var doc map[string]json.RawMessage
	if json.Unmarshal(body, &doc) == nil {
		doc["router_engines_used"] = mustJSON(splitCSV(enginesUsed))
		doc["router_tier"] = mustJSON(tier)
		if b, err := json.Marshal(doc); err == nil {
			body = b
		}
	}
	recordEvent(searchEvent{
		TS: time.Now().UTC().Format(time.RFC3339Nano), Query: query, Tier: tier,
		Engines: enginesUsed, Results: tr.results, Status: tr.status, LatencyMS: tr.latencyMS,
		Unresponsive: tr.unresponsive, Outcome: "ok",
	})
	return searchOutcome{status: http.StatusOK, body: body, enginesUsed: enginesUsed,
		tier: tier, unresponsive: tr.unresponsive}
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

// ------------------------------------------------------------------ http
// extractParams returns the caller's full SearXNG parameter set. GET uses the
// query string directly; POST accepts JSON {"q":...} or form-encoded bodies.
func extractParams(r *http.Request) url.Values {
	if r.Method == http.MethodPost {
		defer r.Body.Close()
		raw, _ := io.ReadAll(r.Body)
		var j map[string]any
		if json.Unmarshal(raw, &j) == nil && len(j) > 0 {
			v := url.Values{}
			for k, val := range j {
				v.Set(k, fmt.Sprintf("%v", val))
			}
			return v
		}
		if v, err := url.ParseQuery(string(raw)); err == nil {
			return v
		}
		return url.Values{}
	}
	return r.URL.Query()
}

func searchHandler(w http.ResponseWriter, r *http.Request) {
	params := extractParams(r)
	if params.Get("q") == "" {
		writeJSON(w, http.StatusBadRequest, []byte(`{"error":"missing q"}`))
		return
	}
	// Opt-in strict mode: return 503 on total failure instead of a SearXNG-shaped
	// 200 empty. Header `X-Router-Strict: 1` or `?strict=1`.
	strict := r.Header.Get("X-Router-Strict") == "1" || params.Get("strict") == "1"
	params.Del("strict")
	log.Printf("search q=%q categories=%q", params.Get("q"), params.Get("categories"))
	oc := handleSearch(params, strict)

	// Router metadata as response headers (invisible to JSON parsers; safe).
	if oc.enginesUsed != "" {
		w.Header().Set("X-Router-Engines", oc.enginesUsed)
	}
	w.Header().Set("X-Router-Tier", oc.tier)
	if len(oc.unresponsive) > 0 {
		var names []string
		for _, u := range oc.unresponsive {
			names = append(names, u.Engine)
		}
		w.Header().Set("X-Router-Unresponsive", strings.Join(names, ","))
	}
	writeJSON(w, oc.status, oc.body)
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	state := map[string]any{
		"status":                 "ok",
		"direct_inflight":        atomic.LoadInt64(&directInflight),
		"direct_cooldown_active": time.Now().UnixNano() < cooldownUntil.Load(),
		"direct_max":             directMax,
		"direct_rate_tokens":     int(directBucket.Available()),
		"direct_rate_burst":      perIPBurst,
		"direct_rate_per_min":    perIPRate * 60,
		"default_direct_only":    defaultDirectOnly,
		"proxy_count":            readProxyCount(),
		"proxied_rate_tokens":    int(proxiedBucket.Available()),
		"proxied_rate_per_min":   int(proxiedBucket.refillRate * 60),
		"max_wait_seconds":       int(maxWait.Seconds()),
	}
	// per-engine bucket state (rotation visibility)
	eng := map[string]any{}
	for name, e := range engines {
		eng[name] = map[string]any{
			"tokens":  int(e.bucket.Available()),
			"per_min": e.perMin,
		}
	}
	state["engines"] = eng
	b, _ := json.Marshal(state)
	writeJSON(w, http.StatusOK, b)
}

func writeJSON(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func main() {
	// Size the proxied-pool bucket from the live proxy count, then keep it in
	// sync as the pool heals/shrinks.
	refreshProxiedCapacity()
	go func() {
		t := time.NewTicker(60 * time.Second)
		for range t.C {
			refreshProxiedCapacity()
		}
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("/search", searchHandler)
	mux.HandleFunc("/healthz", healthHandler)
	mux.HandleFunc("/stats", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, mustJSON(statsSnapshot()))
	})

	// Bind loopback (host-side tools) + docker bridge gateway (containers reach
	// via host.docker.internal). Extra bind addrs can be set via ROUTER_BIND_ADDRS
	// (comma-separated host:port). Never bound to a public interface.
	binds := []string{"127.0.0.1:" + listenPort, "172.17.0.1:" + listenPort}
	if env := os.Getenv("ROUTER_BIND_ADDRS"); env != "" {
		binds = strings.Split(env, ",")
	}

	log.Printf("  tiers: direct=%s (max %d) -> proxied=%s -> dead-letter", direct, directMax, proxied)
	errCh := make(chan error, len(binds))
	for _, addr := range binds {
		addr := addr
		srv := &http.Server{
			Addr:              addr,
			Handler:           mux,
			ReadHeaderTimeout: 5 * time.Second,
		}
		go func() {
			log.Printf("searxng-router listening on http://%s", addr)
			errCh <- srv.ListenAndServe()
		}()
	}
	fmt.Fprintln(os.Stderr, "ready")
	log.Fatal(<-errCh)
}
