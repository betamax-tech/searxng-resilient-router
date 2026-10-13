// tiers.go — rules-first tier classification (no model). A query is routed
// to one of three tiers (crucial|standard|bulk) via an optional `tier=`
// param (or `X-Search-Tier` header), default "standard". PH legal-citation
// patterns and legal-site `site:` filters ALWAYS force "crucial", regardless
// of what the caller asked for — the owner's hard requirement that the most
// important queries never silently degrade to a cheap/no-cost tier.
package main

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
)

const (
	TierCrucial  = "crucial"
	TierStandard = "standard"
	TierBulk     = "bulk"
)

// legalCitationRE matches the PH legal citation patterns named in the task:
// G.R. No., A.M. No., A.C. No., MTJ-, RTJ-, "Republic Act No.", "R.A. No."
var legalCitationRE = regexp.MustCompile(`(?i)(G\.R\.\s*No\.?|A\.M\.\s*No\.?|A\.C\.\s*No\.?|MTJ-|RTJ-|Republic\s+Act\s+No\.?|R\.A\.\s*No\.?)`)

// legalSiteDomains are the PH primary-source legal domains named in the task.
var legalSiteDomains = []string{
	"lawphil.net",
	"elibrary.judiciary.gov.ph",
	"sc.judiciary.gov.ph",
	"officialgazette.gov.ph",
	"chanrobles.com",
}

func isLegalCitationQuery(q string) bool {
	return legalCitationRE.MatchString(q)
}

// isLegalSiteQuery reports whether q contains a `site:` filter targeting one
// of the legal primary-source domains (case-insensitive, matches the
// existing hasSiteFilter() convention in router.go).
func isLegalSiteQuery(q string) bool {
	lower := strings.ToLower(q)
	if !strings.Contains(lower, "site:") {
		return false
	}
	for _, d := range legalSiteDomains {
		if strings.Contains(lower, d) {
			return true
		}
	}
	return false
}

// classifyTier returns the effective tier and the RULE that produced it
// (surfaced in logs/response so a wrong classification is diagnosable).
// Legal rules always win over an explicit caller request; otherwise a valid
// explicit `requested` tier wins; default is "standard".
func classifyTier(query, requested string) (tier string, rule string) {
	if isLegalCitationQuery(query) {
		return TierCrucial, "legal_citation_pattern"
	}
	if isLegalSiteQuery(query) {
		return TierCrucial, "legal_site_filter"
	}
	switch requested {
	case TierCrucial, TierStandard, TierBulk:
		return requested, "explicit_param"
	}
	return TierStandard, "default"
}

// ---------------------------------------------------------------- shadow hook
//
// ShadowClassifier is a no-op extension point for a FUTURE tier classifier
// (ML or heuristic). It NEVER influences routing today — only its
// suggestion is logged next to the rules-based tier, so a future classifier
// can be evaluated against real traffic before it is ever wired to decide
// anything. nil (the default, and the only value set in this codebase) means
// "no shadow classifier installed". Per task scope, no model is wired here.
type ShadowClassifier interface {
	SuggestTier(query string) string
}

var shadowClassifier ShadowClassifier // nil today — intentionally never assigned in this task.

func shadowSuggestedTier(query string) string {
	if shadowClassifier == nil {
		return ""
	}
	return shadowClassifier.SuggestTier(query)
}

// --------------------------------------------------------- provider ordering
//
// tierProviderOrder maps a tier name to the provider-name sequence tried in
// order. This is DATA, not code, so the waterfall can be re-tuned (e.g.
// after the legal `site:` smoke test) by editing the JSON file and
// restarting — no rebuild required. "searxng" is a special provider name:
// the tiered router delegates that stop to the existing handleSearch()
// direct/proxied logic.
type tierProviderOrder map[string][]string

// defaultTierProviderOrder matches the task's required waterfalls exactly:
//
//	crucial  : Exa -> Tavily -> TinyFish -> Brave -> SearXNG
//	standard : TinyFish -> SearXNG -> Brave -> Exa
//	bulk     : SearXNG -> TinyFish
var defaultTierProviderOrder = tierProviderOrder{
	TierCrucial:  {"exa", "tavily", "tinyfish", "brave", "searxng"},
	TierStandard: {"tinyfish", "searxng", "brave", "exa"},
	TierBulk:     {"searxng", "tinyfish"},
}

var tierProviderOrderFile = envOr("TIER_PROVIDER_ORDER_FILE",
	"/home/cmark/server/searxng-rotation/config/tier_providers.json")

// loadTierProviderOrder reads the JSON config file fresh (cheap: a handful
// of bytes) so an operator edit takes effect on the NEXT request, no restart
// needed. Any tier missing from the file falls back to the built-in default
// for that tier; a missing/invalid file falls back entirely to defaults.
//
// Decoding is deliberately LENIENT (field-by-field, via json.RawMessage)
// rather than a single strict map[string][]string unmarshal: the config
// file is meant to be hand-edited by operators and may contain non-array
// bookkeeping fields (e.g. "_comment") that would otherwise fail the WHOLE
// unmarshal and silently fall back to defaults — exactly the failure caught
// during live verification (2026-10-13) when a "_comment" string field
// broke parsing entirely.
func loadTierProviderOrder(path string) tierProviderOrder {
	b, err := os.ReadFile(path)
	if err != nil {
		return defaultTierProviderOrder
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil || len(raw) == 0 {
		return defaultTierProviderOrder
	}
	m := tierProviderOrder{}
	for key, v := range raw {
		var order []string
		if err := json.Unmarshal(v, &order); err != nil {
			continue // not a string array (e.g. "_comment") — skip, not fatal
		}
		m[key] = order
	}
	if len(m) == 0 {
		return defaultTierProviderOrder
	}
	for tier, order := range defaultTierProviderOrder {
		if _, ok := m[tier]; !ok {
			m[tier] = order
		}
	}
	return m
}

func getTierProviderOrder() tierProviderOrder {
	return loadTierProviderOrder(tierProviderOrderFile)
}
