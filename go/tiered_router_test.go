package main

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"testing"
)

// fakeProvider is a test double implementing Provider via a closure, so each
// test can script exactly the outcome a provider should return without any
// network I/O.
type fakeProvider struct {
	name  string
	calls *int
	fn    func(ctx context.Context, query string, params url.Values) ProviderResult
}

func (f fakeProvider) Name() string { return f.name }

func (f fakeProvider) Search(ctx context.Context, query string, params url.Values) ProviderResult {
	if f.calls != nil {
		*f.calls++
	}
	return f.fn(ctx, query, params)
}

// withTestEnv swaps the package-level capLimiter/diskCache/tierProviderOrderFile
// globals for fresh, isolated, tmp-backed instances for the duration of one
// test, restoring the originals afterward. This is REQUIRED: the production
// globals point at real on-disk paths under cache/ and must never be mutated
// by `go test`.
func withTestEnv(t *testing.T) {
	t.Helper()
	origCapLimiter := capLimiter
	origDiskCache := diskCache
	origOrderFile := tierProviderOrderFile
	origRegistry := make(map[string]Provider, len(providerRegistry))
	for k, v := range providerRegistry {
		origRegistry[k] = v
	}

	dir := t.TempDir()
	capLimiter = NewCapLimiter(filepath.Join(dir, "caps.json"), capWindowDefs)
	diskCache = NewDiskCache(filepath.Join(dir, "cache"), cacheTTL, cacheMaxBytes)
	tierProviderOrderFile = filepath.Join(dir, "does-not-exist-unless-written.json")

	t.Cleanup(func() {
		capLimiter = origCapLimiter
		diskCache = origDiskCache
		tierProviderOrderFile = origOrderFile
		for k := range providerRegistry {
			delete(providerRegistry, k)
		}
		for k, v := range origRegistry {
			providerRegistry[k] = v
		}
	})
}

func writeOrderFile(t *testing.T, order map[string][]string) {
	t.Helper()
	b, err := json.Marshal(order)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tierProviderOrderFile, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSkipTierRouting(t *testing.T) {
	cases := []struct {
		name   string
		params url.Values
		query  string
		want   bool
	}{
		{"engines param set", url.Values{"engines": {"bing"}}, "misconduct", true},
		{"engine param set", url.Values{"engine": {"bing"}}, "misconduct", true},
		{"bang in query", url.Values{}, "!google misconduct", true},
		{"non-general category", url.Values{"categories": {"science"}}, "misconduct", true},
		{"general category explicit", url.Values{"categories": {"general"}}, "misconduct", false},
		{"no params, no bang", url.Values{}, "misconduct", false},
		{"site filter alone does NOT skip (tier routing applies)", url.Values{}, "site:lawphil.net x", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := skipTierRouting(c.params, c.query); got != c.want {
				t.Errorf("skipTierRouting(%v, %q) = %v, want %v", c.params, c.query, got, c.want)
			}
		})
	}
}

// Failover order: crucial is exa -> tavily -> tinyfish -> brave (searxng
// excluded from this test's custom order to keep it network-free). exa
// errors, tavily returns zero results, tinyfish succeeds — brave must never
// be called.
func TestHandleTieredSearch_FailoverOrder(t *testing.T) {
	withTestEnv(t)
	writeOrderFile(t, map[string][]string{
		"crucial": {"exa", "tavily", "tinyfish", "brave"},
	})

	var exaCalls, tavilyCalls, tinyfishCalls, braveCalls int
	providerRegistry["exa"] = fakeProvider{name: "exa", calls: &exaCalls, fn: func(ctx context.Context, q string, p url.Values) ProviderResult {
		return ProviderResult{Err: "rate_limited"}
	}}
	providerRegistry["tavily"] = fakeProvider{name: "tavily", calls: &tavilyCalls, fn: func(ctx context.Context, q string, p url.Values) ProviderResult {
		return ProviderResult{Results: nil} // zero results -> failover
	}}
	providerRegistry["tinyfish"] = fakeProvider{name: "tinyfish", calls: &tinyfishCalls, fn: func(ctx context.Context, q string, p url.Values) ProviderResult {
		return ProviderResult{Results: []SearxResult{{URL: "https://lawphil.net/x", Title: "X", Content: "c", Engines: []string{"tinyfish"}}}}
	}}
	providerRegistry["brave"] = fakeProvider{name: "brave", calls: &braveCalls, fn: func(ctx context.Context, q string, p url.Values) ProviderResult {
		return ProviderResult{Results: []SearxResult{{URL: "https://example.com", Title: "should not be reached"}}}
	}}

	params := url.Values{"q": {"G.R. No. 123456 some case"}} // forces crucial tier via legal_citation_pattern
	out := handleTieredSearch(params, "", false)

	if out.searchTier != TierCrucial {
		t.Fatalf("expected crucial tier, got %q", out.searchTier)
	}
	if out.providerUsed != "tinyfish" {
		t.Fatalf("expected tinyfish to win the waterfall, got %q (failovers=%v)", out.providerUsed, out.failovers)
	}
	if exaCalls != 1 || tavilyCalls != 1 || tinyfishCalls != 1 {
		t.Errorf("expected exa/tavily/tinyfish each called once, got exa=%d tavily=%d tinyfish=%d", exaCalls, tavilyCalls, tinyfishCalls)
	}
	if braveCalls != 0 {
		t.Errorf("expected brave NEVER called once tinyfish succeeded, got %d calls", braveCalls)
	}
	if len(out.failovers) != 2 {
		t.Errorf("expected exactly 2 failover steps (exa, tavily), got %v", out.failovers)
	}
	if out.failovers[0].Provider != "exa" || out.failovers[0].Reason != "rate_limited" {
		t.Errorf("unexpected first failover step: %+v", out.failovers[0])
	}
	if out.failovers[1].Provider != "tavily" || out.failovers[1].Reason != "empty_results" {
		t.Errorf("unexpected second failover step: %+v", out.failovers[1])
	}
}

// A provider whose cap is already exhausted must be skipped WITHOUT ever
// calling Search() on it.
func TestHandleTieredSearch_CapReachedSkipsProviderWithoutCalling(t *testing.T) {
	withTestEnv(t)
	writeOrderFile(t, map[string][]string{
		"standard": {"exa", "tinyfish"},
	})

	// Saturate exa's cap before the call.
	capLimiter.RecordCost("exa", 9.00)

	var exaCalls, tinyfishCalls int
	providerRegistry["exa"] = fakeProvider{name: "exa", calls: &exaCalls, fn: func(ctx context.Context, q string, p url.Values) ProviderResult {
		return ProviderResult{Results: []SearxResult{{URL: "https://example.com", Title: "should never be called"}}}
	}}
	providerRegistry["tinyfish"] = fakeProvider{name: "tinyfish", calls: &tinyfishCalls, fn: func(ctx context.Context, q string, p url.Values) ProviderResult {
		return ProviderResult{Results: []SearxResult{{URL: "https://example.com", Title: "ok"}}}
	}}

	out := handleTieredSearch(url.Values{"q": {"misconduct"}}, "standard", false)

	if exaCalls != 0 {
		t.Errorf("expected exa.Search() NEVER invoked once capped, got %d calls", exaCalls)
	}
	if out.providerUsed != "tinyfish" {
		t.Errorf("expected failover to tinyfish, got %q", out.providerUsed)
	}
	if len(out.failovers) != 1 || out.failovers[0].Provider != "exa" {
		t.Fatalf("expected one failover step for exa, got %v", out.failovers)
	}
	if out.failovers[0].Reason == "" {
		t.Errorf("expected a cap-reached reason, got empty")
	}
}

// A cache hit must short-circuit the entire waterfall — no provider should
// be called a second time for an identical (query, tier, params) request.
func TestHandleTieredSearch_CacheHitSkipsProviders(t *testing.T) {
	withTestEnv(t)
	writeOrderFile(t, map[string][]string{
		"standard": {"tinyfish"},
	})

	var calls int
	providerRegistry["tinyfish"] = fakeProvider{name: "tinyfish", calls: &calls, fn: func(ctx context.Context, q string, p url.Values) ProviderResult {
		return ProviderResult{Results: []SearxResult{{URL: "https://example.com", Title: "ok"}}}
	}}

	params := url.Values{"q": {"misconduct"}}
	first := handleTieredSearch(params, "standard", false)
	if first.cacheHit {
		t.Fatalf("first call must be a cache MISS")
	}
	if calls != 1 {
		t.Fatalf("expected exactly 1 provider call on first request, got %d", calls)
	}

	second := handleTieredSearch(url.Values{"q": {"misconduct"}}, "standard", false)
	if !second.cacheHit {
		t.Errorf("expected second identical request to be a cache HIT")
	}
	if calls != 1 {
		t.Errorf("expected NO additional provider call on cache hit, got %d total calls", calls)
	}
}

// When every provider in the order fails/empties/caps-out, the router must
// respond AS SEARXNG (HTTP 200, empty results[]) rather than erroring, so
// existing consumers never see an unparseable shape.
func TestHandleTieredSearch_AllExhaustedReturnsEmptySearxShape(t *testing.T) {
	withTestEnv(t)
	writeOrderFile(t, map[string][]string{
		"bulk": {"tinyfish"},
	})
	providerRegistry["tinyfish"] = fakeProvider{name: "tinyfish", fn: func(ctx context.Context, q string, p url.Values) ProviderResult {
		return ProviderResult{Err: "http_503"}
	}}

	out := handleTieredSearch(url.Values{"q": {"misconduct"}}, "bulk", false)
	if out.status != 200 {
		t.Errorf("expected HTTP 200 on exhaustion (non-strict), got %d", out.status)
	}
	var env struct {
		Results []any `json:"results"`
	}
	if err := json.Unmarshal(out.body, &env); err != nil {
		t.Fatalf("response body is not valid JSON: %v (%s)", err, out.body)
	}
	if len(env.Results) != 0 {
		t.Errorf("expected empty results[] on exhaustion, got %d", len(env.Results))
	}
}

// strict=1 / X-Router-Strict must turn total exhaustion into a 503, matching
// the existing handleSearch() strict-mode contract.
func TestHandleTieredSearch_StrictModeReturns503OnExhaustion(t *testing.T) {
	withTestEnv(t)
	writeOrderFile(t, map[string][]string{
		"bulk": {"tinyfish"},
	})
	providerRegistry["tinyfish"] = fakeProvider{name: "tinyfish", fn: func(ctx context.Context, q string, p url.Values) ProviderResult {
		return ProviderResult{Err: "http_503"}
	}}

	out := handleTieredSearch(url.Values{"q": {"misconduct"}}, "bulk", true /* strict */)
	if out.status != 503 {
		t.Errorf("expected HTTP 503 in strict mode on exhaustion, got %d", out.status)
	}
}

// recordAttemptedCall bridges a ProviderResult into the cap limiter
// correctly for both cost-mode (Exa) and count-mode (Brave/Tavily/TinyFish)
// providers, including the fail-closed path when cost is unknown. Only a
// confirmed HTTP 200 response records usage.
func TestRecordAttemptedCall(t *testing.T) {
	withTestEnv(t)

	recordAttemptedCall("exa", ProviderResult{StatusCode: 200, CostKnown: true, CostDollars: 1.23})
	snap := capLimiter.Snapshot()
	windows := snap["windows"].(map[string]any)
	exaWin := windows["exa_monthly"].(map[string]any)
	if used := exaWin["used"].(float64); used != 1.23 {
		t.Errorf("expected exa usage 1.23 after a known-cost call, got %v", used)
	}

	recordAttemptedCall("brave", ProviderResult{StatusCode: 200}) // count-mode, no cost field needed
	snap2 := capLimiter.Snapshot()
	braveWin2 := snap2["windows"].(map[string]any)["brave_monthly"].(map[string]any)
	if used := braveWin2["used"].(float64); used != 1 {
		t.Errorf("expected brave usage 1 after one count-mode call, got %v", used)
	}
}

func TestRecordAttemptedCall_UnknownCostFailsClosed(t *testing.T) {
	withTestEnv(t)
	recordAttemptedCall("exa", ProviderResult{StatusCode: 200, CostKnown: false}) // 200 but no costDollars
	ok, reason := capLimiter.Allowed("exa")
	if ok {
		t.Fatalf("expected exa fail-closed after an unknown-cost 200 response")
	}
	if reason == "" {
		t.Errorf("expected a non-empty fail-closed reason")
	}
}

// Regression (caught during this task's own design review): a response
// that failed to parse into usable results, or legitimately found zero
// hits, STILL means the provider processed the request and may have
// billed. Usage must be recorded in both cases, not just on the happy path
// with non-empty results — otherwise repeated "empty/bad" queries could
// burn real quota while the cap limiter believes nothing was spent.
func TestRecordAttemptedCall_RecordsOnNon200OnlyStatusOK(t *testing.T) {
	withTestEnv(t)

	// Non-200 (e.g. rate_limited, network failure) -> NOT recorded.
	recordAttemptedCall("brave", ProviderResult{StatusCode: 429, Err: "rate_limited"})
	recordAttemptedCall("brave", ProviderResult{StatusCode: 0, Err: "request_failed"})
	snap := capLimiter.Snapshot()
	used := snap["windows"].(map[string]any)["brave_monthly"].(map[string]any)["used"].(float64)
	if used != 0 {
		t.Fatalf("expected 0 usage recorded for non-200 responses, got %v", used)
	}

	// 200 with bad_json / zero results -> IS recorded (the provider did work).
	recordAttemptedCall("brave", ProviderResult{StatusCode: 200, Err: "bad_json"})
	recordAttemptedCall("brave", ProviderResult{StatusCode: 200}) // empty results, no Err
	snap2 := capLimiter.Snapshot()
	used2 := snap2["windows"].(map[string]any)["brave_monthly"].(map[string]any)["used"].(float64)
	if used2 != 2 {
		t.Fatalf("expected 2 usage units recorded for the two 200 responses, got %v", used2)
	}
}

// Sanity: a successful waterfall body round-trips the additive router_*
// metadata so a consumer (or the /stats-style debugging) can see which tier
// and provider answered.
func TestBuildSearxBody_AdditiveMetadata(t *testing.T) {
	results := []SearxResult{{URL: "https://lawphil.net/x", Title: "X", Content: "c", Engines: []string{"exa"}}}
	body := buildSearxBody("G.R. No. 1", results, TierCrucial, "legal_citation_pattern", "", "exa",
		[]string{"exa"}, nil)

	var env map[string]any
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if env["router_search_tier"] != "crucial" {
		t.Errorf("router_search_tier = %v, want crucial", env["router_search_tier"])
	}
	if env["router_provider_used"] != "exa" {
		t.Errorf("router_provider_used = %v, want exa", env["router_provider_used"])
	}
	if int(env["number_of_results"].(float64)) != 1 {
		t.Errorf("number_of_results = %v, want 1", env["number_of_results"])
	}
}

func TestMergeAdditiveKeys_PreservesExistingFields(t *testing.T) {
	orig := []byte(`{"query":"x","results":[{"url":"https://a"}],"router_tier":"direct"}`)
	merged := mergeAdditiveKeys(orig, map[string]any{"router_cache_hit": true})
	var env map[string]any
	if err := json.Unmarshal(merged, &env); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if env["router_tier"] != "direct" {
		t.Errorf("expected pre-existing router_tier preserved, got %v", env["router_tier"])
	}
	if env["router_cache_hit"] != true {
		t.Errorf("expected router_cache_hit=true merged in, got %v", env["router_cache_hit"])
	}
}

func TestCountResultsInBody(t *testing.T) {
	if n := countResultsInBody([]byte(`{"results":[{},{},{}]}`)); n != 3 {
		t.Errorf("countResultsInBody = %d, want 3", n)
	}
	if n := countResultsInBody([]byte(`{"results":[]}`)); n != 0 {
		t.Errorf("countResultsInBody(empty) = %d, want 0", n)
	}
	if n := countResultsInBody([]byte(`not json`)); n != -1 {
		t.Errorf("countResultsInBody(invalid) = %d, want -1", n)
	}
}
