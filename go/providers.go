// providers.go — shared types for the provider abstraction. Every paid
// search provider (Exa, Tavily, Brave, TinyFish) maps its results into
// SearxResult, the exact shape consumers already parse from SearXNG's own
// response (results[] with url, title, content, engines=[name]), so no
// consumer (OpenWebUI extended_research, local-researcher MCP) needs to
// change to benefit from the new providers.
package main

import (
	"context"
	"net/url"
	"time"
)

// providerTimeout bounds every outbound call to a paid provider — short
// enough that one slow provider can't stall the whole failover waterfall
// within the caller's patience, long enough for a normal API round trip.
const providerTimeout = 12 * time.Second

// SearxResult is the common result shape. See package doc above.
type SearxResult struct {
	URL     string   `json:"url"`
	Title   string   `json:"title"`
	Content string   `json:"content"`
	Engines []string `json:"engines"`
}

// ProviderResult is one provider's answer to one query. Err non-empty means
// failure (the tiered router fails over to the next provider in the order);
// an empty Results slice with no Err means a genuine zero-hit search, which
// the tiered router also treats as a failover trigger per the task spec.
type ProviderResult struct {
	Results     []SearxResult
	StatusCode  int
	Err         string
	LatencyMS   int64
	CostDollars float64 // actual recorded cost, if the provider reports one
	CostKnown   bool    // true only when CostDollars came from the provider's own response
}

// Provider is the common interface every paid search backend implements.
// SearXNG's own direct/proxied tiers are handled separately inside
// handleSearch() and are intentionally NOT part of this registry — the
// tiered router treats "searxng" as a special-cased stop that delegates to
// the existing, already-hardened handleSearch() logic (P2/P3 fixes included).
type Provider interface {
	Name() string
	Search(ctx context.Context, query string, params url.Values) ProviderResult
}

// providerRegistry maps provider name (as referenced in tier_providers.json)
// to its implementation. Tests swap entries for fakes; production uses the
// real adapters constructed below.
var providerRegistry = map[string]Provider{
	"exa":      Provider(NewExaProvider()),
	"tavily":   Provider(NewTavilyProvider()),
	"brave":    Provider(NewBraveProvider()),
	"tinyfish": Provider(NewTinyFishProvider()),
}

// firstString returns the first non-empty string field found in m for any
// of keys, tolerating provider response-shape drift (defensive JSON
// decoding) — needed for TinyFish, whose exact field names are not fully
// documented (2026-10-13 investigation doc, known gap #3).
func firstString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
	}
	return ""
}
