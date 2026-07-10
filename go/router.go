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
	"sync"
	"sync/atomic"
	"time"
)

// ------------------------------------------------------------------ config
const (
	listenAddr = "127.0.0.1:8899"
	direct     = "http://127.0.0.1:8890"
	proxied    = "http://127.0.0.1:8891"

	directMax      = 2                // concurrent direct searches before spilling
	cooldown       = 180 * time.Second
	directTimeout  = 20 * time.Second
	proxiedTimeout = 45 * time.Second

	// Direct-tier outbound rate limit — protects THIS server's real datacenter IP
	// (the proxied tier is intentionally NOT rate-limited; rotating IPs absorb it).
	// Token bucket: sustained 6/min, burst up to 15. Researched safe zone for
	// Google web scraping from a datacenter IP (exceeding risks a 24h suspension).
	rateBurst      = 15               // bucket capacity (max burst)
	rateRefillRate = 6.0 / 60.0       // tokens per second (6 per minute sustained)
)

var deadletterDir = envOr("DEADLETTER_DIR", "/home/cmark/server/searxng-rotation/cache/deadletter")

// ------------------------------------------------------------------ state
var (
	directInflight int64        // atomic
	cooldownUntil  atomic.Int64 // unix-nano; direct skipped until this time
	directBucket   = newTokenBucket(rateBurst, rateRefillRate)
)

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
			// Refund the token we took — we're spilling, not using direct.
			directBucket.mu.Lock()
			if directBucket.tokens < directBucket.capacity {
				directBucket.tokens++
			}
			directBucket.mu.Unlock()
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
func upstreamSearch(ctx context.Context, base, query string) (int, []byte, int) {
	u := base + "/search?" + url.Values{"q": {query}, "format": {"json"}}.Encode()
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

func tryTier(name, base, query string, timeout time.Duration) []byte {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	status, body, n := upstreamSearch(ctx, base, query)
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

func handleSearch(query string) (int, []byte) {
	// Tier 1: direct (rate + concurrency gated)
	if ok, reason := tryAcquireDirect(); ok {
		body := tryTier("direct", direct, query, directTimeout)
		releaseDirect()
		if body != nil {
			return http.StatusOK, body
		}
	} else {
		log.Printf("  direct tier skipped (%s) -> spilling to proxied", reason)
	}

	// Tier 2: proxied
	if body := tryTier("proxied", proxied, query, proxiedTimeout); body != nil {
		return http.StatusOK, body
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
func extractQuery(r *http.Request) string {
	if r.Method == http.MethodPost {
		defer r.Body.Close()
		raw, _ := io.ReadAll(r.Body)
		var j struct {
			Q string `json:"q"`
		}
		if json.Unmarshal(raw, &j) == nil && j.Q != "" {
			return j.Q
		}
		if v, err := url.ParseQuery(string(raw)); err == nil {
			return v.Get("q")
		}
		return ""
	}
	return r.URL.Query().Get("q")
}

func searchHandler(w http.ResponseWriter, r *http.Request) {
	query := extractQuery(r)
	if query == "" {
		writeJSON(w, http.StatusBadRequest, []byte(`{"error":"missing q"}`))
		return
	}
	log.Printf("search q=%q", query)
	status, body := handleSearch(query)
	writeJSON(w, status, body)
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	state := map[string]any{
		"status":                 "ok",
		"direct_inflight":        atomic.LoadInt64(&directInflight),
		"direct_cooldown_active": time.Now().UnixNano() < cooldownUntil.Load(),
		"direct_max":             directMax,
		"direct_rate_tokens":     int(directBucket.Available()),
		"direct_rate_burst":      rateBurst,
		"direct_rate_per_min":    rateRefillRate * 60,
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
	mux := http.NewServeMux()
	mux.HandleFunc("/search", searchHandler)
	mux.HandleFunc("/healthz", healthHandler)

	srv := &http.Server{
		Addr:              listenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Printf("searxng-router listening on http://%s", listenAddr)
	log.Printf("  tiers: direct=%s (max %d) -> proxied=%s -> dead-letter", direct, directMax, proxied)
	fmt.Fprintln(os.Stderr, "ready")
	log.Fatal(srv.ListenAndServe())
}
