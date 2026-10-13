// tiered_router.go — the provider/tier waterfall itself. Classifies the
// query's tier, checks the disk cache, then walks that tier's configured
// provider order (SearXNG is one stop among several, delegating to the
// existing, already-hardened handleSearch() for it), failing over on
// error/429/timeout/empty-results/cap-reached. Maps every success into the
// SearXNG JSON response shape so existing consumers never see a format they
// can't parse.
package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/url"
	"strings"
)

var capLimiter = NewCapLimiter(
	envOr("PROVIDER_CAPS_FILE", "/home/cmark/server/searxng-rotation/cache/provider_caps.json"),
	capWindowDefs,
)

var diskCache = NewDiskCache(
	envOr("SEARCH_CACHE_DIR", "/home/cmark/server/searxng-rotation/cache/search"),
	cacheTTL,
	cacheMaxBytes,
)

// skipTierRouting reports whether the caller has ALREADY and explicitly
// chosen where to search — an engines=/engine= param, a SearXNG !bang, or a
// non-general category. Those callers keep TODAY'S SearXNG-only behavior
// completely unchanged (the P2/P3 fixes); the provider/tier system never
// engages for them.
//
// A bare `site:` filter is deliberately NOT in this list: a legal `site:`
// filter is one of the rules that FORCES the crucial tier (tiers.go), so it
// must still flow through the provider waterfall. The SearXNG stop inside
// that waterfall still honors `site:` correctly via handleSearch()'s
// existing hasSiteFilter() rotation-skip (P3) — nothing there changes.
func skipTierRouting(params url.Values, query string) bool {
	cat := params.Get("categories")
	return params.Get("engines") != "" || params.Get("engine") != "" ||
		hasBang(query) || (cat != "" && cat != "general")
}

type failoverStep struct {
	Provider string `json:"provider"`
	Reason   string `json:"reason"`
}

type tieredOutcome struct {
	status         int
	body           []byte
	searchTier     string
	tierRule       string
	shadowTier     string
	providerUsed   string
	providersTried []string
	failovers      []failoverStep
	cacheHit       bool
	costUSD        float64
}

// handleTieredSearch is the provider/tier entry point. tierRequested is the
// raw `tier=`/`X-Search-Tier` value (possibly empty); classifyTier decides
// the effective tier and may override it (legal rules always win).
func handleTieredSearch(params url.Values, tierRequested string, strict bool) tieredOutcome {
	query := params.Get("q")
	tier, rule := classifyTier(query, strings.ToLower(tierRequested))
	shadow := shadowSuggestedTier(query)

	cacheKey := buildCacheKey(tier, params)
	if cached, ok := diskCache.Get(cacheKey); ok {
		body := mergeAdditiveKeys(cached, map[string]any{"router_cache_hit": true})
		recordTieredEvent(query, tier, rule, shadow, "cache", nil, nil, 0, true)
		return tieredOutcome{status: http.StatusOK, body: body, searchTier: tier, tierRule: rule,
			shadowTier: shadow, cacheHit: true}
	}

	order := getTierProviderOrder()[tier]
	var tried []string
	var failovers []failoverStep

	for _, prov := range order {
		tried = append(tried, prov)

		if prov == "searxng" {
			oc := handleSearch(cloneValues(params), strict)
			n := countResultsInBody(oc.body)
			if oc.status == http.StatusOK && n > 0 {
				extra := map[string]any{
					"router_search_tier":     tier,
					"router_tier_rule":       rule,
					"router_shadow_tier":     shadow,
					"router_provider_used":   "searxng",
					"router_providers_tried": tried,
					"router_failovers":       failovers,
					"router_cache_hit":       false,
				}
				body := mergeAdditiveKeys(oc.body, extra)
				diskCache.Set(cacheKey, body)
				recordTieredEvent(query, tier, rule, shadow, "searxng", tried, failovers, 0, false)
				return tieredOutcome{status: http.StatusOK, body: body, searchTier: tier, tierRule: rule,
					shadowTier: shadow, providerUsed: "searxng", providersTried: tried, failovers: failovers}
			}
			failovers = append(failovers, failoverStep{"searxng", "empty_or_error"})
			continue
		}

		p, ok := providerRegistry[prov]
		if !ok {
			failovers = append(failovers, failoverStep{prov, "unknown_provider"})
			continue
		}
		if allowed, reason := capLimiter.Allowed(prov); !allowed {
			log.Printf("  tier=%s provider=%s SKIPPED: %s", tier, prov, reason)
			failovers = append(failovers, failoverStep{prov, reason})
			continue
		}

		ctx, cancel := context.WithTimeout(context.Background(), providerTimeout)
		res := p.Search(ctx, query, params)
		cancel()

		if res.Err != "" {
			log.Printf("  tier=%s provider=%s FAIL: %s", tier, prov, res.Err)
			failovers = append(failovers, failoverStep{prov, res.Err})
			continue
		}
		if len(res.Results) == 0 {
			failovers = append(failovers, failoverStep{prov, "empty_results"})
			continue
		}

		// Success: record cap usage BEFORE returning, so the spend is never
		// lost even if something downstream errors.
		recordProviderUsage(prov, res)

		body := buildSearxBody(query, res.Results, tier, rule, shadow, prov, tried, failovers)
		diskCache.Set(cacheKey, body)
		recordTieredEvent(query, tier, rule, shadow, prov, tried, failovers, res.CostDollars, false)
		return tieredOutcome{status: http.StatusOK, body: body, searchTier: tier, tierRule: rule, shadowTier: shadow,
			providerUsed: prov, providersTried: tried, failovers: failovers, costUSD: res.CostDollars}
	}

	// Every stop in the tier's order failed, returned empty, or was capped.
	// Respond AS SEARXNG (HTTP 200 + empty results) — same contract as
	// handleSearch()'s own exhaustion path — so existing consumers never see
	// a shape they can't parse. Opt-in strict mode returns 503 instead.
	log.Printf("  tier=%s ALL PROVIDERS EXHAUSTED: %v", tier, failovers)
	body := buildSearxBody(query, nil, tier, rule, shadow, "", tried, failovers)
	status := http.StatusOK
	if strict {
		status = http.StatusServiceUnavailable
	}
	recordTieredEvent(query, tier, rule, shadow, "", tried, failovers, 0, false)
	return tieredOutcome{status: status, body: body, searchTier: tier, tierRule: rule, shadowTier: shadow,
		providersTried: tried, failovers: failovers}
}

// recordProviderUsage persists actual usage against the correct cap
// window(s) immediately after a successful call. Cost-mode providers (Exa)
// MUST report a real cost; if they don't, usage is UNKNOWN and the owner's
// fail-closed rule applies (caps.go: RecordUnknownFailClosed).
func recordProviderUsage(name string, res ProviderResult) {
	hasCost, hasCount := false, false
	for _, d := range capWindowDefs {
		if d.Provider != name {
			continue
		}
		if d.Mode == "cost" {
			hasCost = true
		}
		if d.Mode == "count" {
			hasCount = true
		}
	}
	if hasCost {
		if res.CostKnown {
			capLimiter.RecordCost(name, res.CostDollars)
		} else {
			capLimiter.RecordUnknownFailClosed(name, "provider_did_not_report_cost")
		}
	}
	if hasCount {
		capLimiter.RecordCount(name, 1)
	}
}

// buildSearxBody constructs the SearXNG-shaped envelope for a provider (or
// exhaustion) result, so no consumer needs to change to parse it.
func buildSearxBody(query string, results []SearxResult, tier, rule, shadow, providerUsed string,
	tried []string, failovers []failoverStep) []byte {
	if results == nil {
		results = []SearxResult{}
	}
	env := map[string]any{
		"query": query, "number_of_results": len(results), "results": results,
		"answers": []any{}, "corrections": []any{}, "infoboxes": []any{},
		"suggestions": []any{}, "unresponsive_engines": []any{},
		"router_search_tier": tier, "router_tier_rule": rule, "router_shadow_tier": shadow,
		"router_provider_used": providerUsed, "router_providers_tried": tried,
		"router_failovers": failovers, "router_cache_hit": false,
	}
	return mustJSON(env)
}

// mergeAdditiveKeys sets extra top-level keys into an existing JSON object
// body without disturbing anything already present (SearXNG clients ignore
// unknown top-level keys — same convention as finishSearch() in router.go).
func mergeAdditiveKeys(body []byte, extra map[string]any) []byte {
	var doc map[string]json.RawMessage
	if json.Unmarshal(body, &doc) != nil {
		return body
	}
	for k, v := range extra {
		doc[k] = mustJSON(v)
	}
	b, err := json.Marshal(doc)
	if err != nil {
		return body
	}
	return b
}

func countResultsInBody(body []byte) int {
	var env struct {
		Results []json.RawMessage `json:"results"`
	}
	if json.Unmarshal(body, &env) != nil {
		return -1
	}
	return len(env.Results)
}
