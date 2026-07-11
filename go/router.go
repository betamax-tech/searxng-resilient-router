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
	"path/filepath"
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
	maxWait = 8 * time.Second

	activePoolFile = "/home/cmark/server/searxng-rotation/cache/active_pool.json"
)

var deadletterDir = envOr("DEADLETTER_DIR", "/home/cmark/server/searxng-rotation/cache/deadletter")

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

// index families: pick ONE available member per family per query (order = preference)
var engineFamilies = [][]string{
	{"google", "startpage"},          // Google's index (startpage proxies Google)
	{"bing", "duckduckgo", "yahoo"},  // Bing's index (ddg/yahoo re-serve Bing)
	{"brave"},                        // independent
}

// independent pool: rotated, up to independentsPerQuery picked per query by most-tokens
var independentPool = []string{"yandex", "qwant", "mojeek"}

const independentsPerQuery = 1

// tolerance-based default budgets (requests/min per engine from a datacenter IP)
var engineRateDefaults = map[string]float64{
	"google": 5, "startpage": 5, // aggressive → sip
	"bing": 12, "duckduckgo": 12, "yahoo": 5,
	"brave":  12,
	"yandex": 8, "qwant": 4, "mojeek": 8,
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
	// The caller's explicit choice ALWAYS wins and disables rotation, via either:
	//   - an `engines=` (or `engine=`) query param, OR
	//   - a SearXNG !bang / :bang in the query text (e.g. "!google ...", "!ddg ..."),
	//     which SearXNG parses itself — we must not fight it with an engines= inject.
	callerChoseEngines := params.Get("engines") != "" || params.Get("engine") != "" || hasBang(query)
	if !callerChoseEngines {
		if sel := selectEngines(); sel != "" {
			params.Set("engines", sel)
			params.Del("categories") // let engines win over categories
			log.Printf("  engines rotated -> %s", sel)
		} else {
			log.Printf("  engines: all budgets spent -> SearXNG defaults")
		}
	} else {
		log.Printf("  engines: caller-specified (bang/param) -> rotation skipped")
	}
	enginesUsed := params.Get("engines")

	// Direct-only mode: pin to the direct instance, never spill to proxied.
	directOnly := defaultDirectOnly
	if v := params.Get("direct"); v != "" {
		directOnly = v == "1" || v == "true" || v == "yes"
		params.Del("direct")
	}

	var last tierResult

	if directOnly {
		last = tryTier("direct", direct, params, directTimeout)
		if last.body != nil {
			return finishSearch(query, enginesUsed, "direct", last, strict)
		}
		log.Printf("  direct-only: direct tier failed -> dead-letter (no proxy spill)")
	} else {
		// Per-IP throttle: prefer direct (own 6/min IP budget), else proxied pool
		// (N×6/min across N proxy IPs). If both dry, wait up to maxWait to smooth
		// bursts, then dead-letter (bounded latency).
		deadline := time.Now().Add(maxWait)
		waited := false
		attempted := false
		for !attempted {
			if ok, _ := tryAcquireDirect(); ok {
				attempted = true
				last = tryTier("direct", direct, params, directTimeout)
				releaseDirect()
				if last.body != nil {
					return finishSearch(query, enginesUsed, "direct", last, strict)
				}
			}
			if proxiedBucket.TryTake() {
				attempted = true
				last = tryTier("proxied", proxied, params, proxiedTimeout)
				if last.body != nil {
					return finishSearch(query, enginesUsed, "proxied", last, strict)
				}
			}
			if attempted {
				break
			}
			if time.Now().After(deadline) {
				log.Printf("  throttle: no IP budget within %s -> dead-letter", maxWait)
				break
			}
			if !waited {
				log.Printf("  throttle: all IP budgets dry, waiting up to %s…", maxWait)
				waited = true
			}
			time.Sleep(250 * time.Millisecond)
		}
	}

	// Dead-letter — respond AS SEARXNG (HTTP 200 + empty results) so clients that
	// call raise_for_status() don't break; query persisted for retry. Opt-in
	// X-Router-Strict / ?strict=1 restores the 503 signal.
	id := deadLetter(query)
	log.Printf("  ALL TIERS FAILED -> dead-lettered as %s (returning 200 empty, SearXNG-compatible)", id)
	env := map[string]any{
		"query": query, "number_of_results": 0, "results": []any{},
		"answers": []any{}, "corrections": []any{}, "infoboxes": []any{},
		"suggestions": []any{}, "unresponsive_engines": []any{},
		"router_status": "queued_for_retry", "deadletter_id": id,
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
		Unresponsive: last.unresponsive, Outcome: "deadletter",
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

var dlMu sync.Mutex

func deadLetter(query string) string {
	dlMu.Lock()
	defer dlMu.Unlock()
	_ = os.MkdirAll(deadletterDir, 0o755)
	name := time.Now().UTC().Format("20060102T150405.000000000") + ".json"
	fp := filepath.Join(deadletterDir, name)
	rec := map[string]any{
		"query":     query,
		"queued_at": time.Now().UTC().Format(time.RFC3339Nano),
		"attempts":  0,
	}
	b, _ := json.MarshalIndent(rec, "", "  ")
	_ = os.WriteFile(fp, b, 0o644)
	return name
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
