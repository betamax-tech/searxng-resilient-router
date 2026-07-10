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

func tryTier(name, base string, params url.Values, timeout time.Duration) []byte {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	status, body, n := upstreamSearch(ctx, base, params)
	if name == "direct" && (status == http.StatusTooManyRequests || status == http.StatusForbidden) {
		tripCooldown()
	}
	if status == http.StatusOK && n > 0 {
		log.Printf("  %s: OK (%d results)", name, n)
		return body
	}
	log.Printf("  %s: FAIL (status=%d, results=%d)", name, status, n)
	return nil
}

func handleSearch(params url.Values) (int, []byte) {
	query := params.Get("q")

	// Direct-only mode: pin to the direct instance, never spill to the proxied
	// (rotating-IP) tier. Use when the caller must keep a session on ONE
	// consistent egress IP. Triggered per-request via ?direct=true (or 1/yes)
	// or globally via ROUTER_DIRECT_ONLY=true. The `direct` param is stripped
	// before forwarding so it never reaches SearXNG.
	directOnly := defaultDirectOnly
	if v := params.Get("direct"); v != "" {
		directOnly = v == "1" || v == "true" || v == "yes"
		params.Del("direct")
	}

	// Tier 1: direct
	if directOnly {
		// Bypass the concurrency/rate gate's spillover semantics: in direct-only
		// mode we always use direct (still honoring the cooldown to avoid
		// hammering a rate-limited engine), because there is no fallback tier.
		body := tryTier("direct", direct, params, directTimeout)
		if body != nil {
			return http.StatusOK, body
		}
		log.Printf("  direct-only: direct tier failed -> dead-letter (no proxy spill)")
	} else {
		// Per-IP throttle: prefer direct (its own 6/min IP budget), else the
		// proxied pool (N×6/min aggregate across N proxy IPs). If BOTH buckets
		// are momentarily dry, wait up to maxWait for either to refill — this
		// smooths bursts (e.g. extended_research fan-out) into a sustainable
		// engine-request rate instead of overwhelming the engines. On timeout,
		// fall through to dead-letter (bounded latency, never blocks forever).
		deadline := time.Now().Add(maxWait)
		waited := false
		attempted := false // did we actually reach an upstream (vs. only budget-blocked)?
		for !attempted {
			// 1. Direct tier (rate + concurrency + cooldown gated).
			if ok, _ := tryAcquireDirect(); ok {
				attempted = true
				body := tryTier("direct", direct, params, directTimeout)
				releaseDirect()
				if body != nil {
					return http.StatusOK, body
				}
			}

			// 2. Proxied pool (aggregate IP budget).
			if proxiedBucket.TryTake() {
				attempted = true
				body := tryTier("proxied", proxied, params, proxiedTimeout)
				if body != nil {
					return http.StatusOK, body
				}
			}

			if attempted {
				break // an upstream ran but returned nothing -> dead-letter
			}

			// 3. Both buckets dry and nothing tried yet: wait, up to maxWait.
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

	// Tier 3: dead-letter — never drop the query
	id := deadLetter(query)
	log.Printf("  ALL TIERS FAILED -> dead-lettered as %s", id)
	env := map[string]any{
		"query":          query,
		"results":        []any{},
		"router_status":  "queued_for_retry",
		"detail":         "all live search tiers failed; query persisted for retry",
		"deadletter_id":  id,
	}
	b, _ := json.Marshal(env)
	return http.StatusServiceUnavailable, b
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
	log.Printf("search q=%q categories=%q", params.Get("q"), params.Get("categories"))
	status, body := handleSearch(params)
	writeJSON(w, status, body)
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
