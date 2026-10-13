package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestClassifyTier_LegalCitationForcesCrucial(t *testing.T) {
	cases := []string{
		"Cervantes v. Pangilinan A.M. No. MTJ-08-1709",
		"G.R. No. 123456 some case",
		"A.C. No. 7399",
		"RTJ-02-1698 judge dismissed",
		"Republic Act No. 4726 Condominium Act",
		"R.A. No. 9262 VAWC",
	}
	for _, q := range cases {
		t.Run(q, func(t *testing.T) {
			// Even an explicit "bulk" request must be overridden by the legal rule.
			tier, rule := classifyTier(q, TierBulk)
			if tier != TierCrucial {
				t.Errorf("classifyTier(%q, bulk) tier = %q, want crucial", q, tier)
			}
			if rule != "legal_citation_pattern" {
				t.Errorf("classifyTier(%q, bulk) rule = %q, want legal_citation_pattern", q, rule)
			}
		})
	}
}

func TestClassifyTier_LegalSiteFilterForcesCrucial(t *testing.T) {
	cases := []string{
		"site:lawphil.net Hulst PR Builders",
		"Site:LawPhil.net Hulst PR Builders",
		"Hulst PR Builders (site:lawphil.net OR site:chanrobles.com)",
		"site:elibrary.judiciary.gov.ph some decision",
		"site:sc.judiciary.gov.ph administrative matter",
		"site:officialgazette.gov.ph proclamation",
		"site:chanrobles.com revised penal code",
	}
	for _, q := range cases {
		t.Run(q, func(t *testing.T) {
			tier, rule := classifyTier(q, TierStandard)
			if tier != TierCrucial {
				t.Errorf("classifyTier(%q, standard) tier = %q, want crucial", q, tier)
			}
			if rule != "legal_site_filter" {
				t.Errorf("classifyTier(%q, standard) rule = %q, want legal_site_filter", q, rule)
			}
		})
	}
}

func TestClassifyTier_NonLegalSiteFilterDoesNotForceCrucial(t *testing.T) {
	tier, rule := classifyTier("site:github.com golang router", TierBulk)
	if tier != TierBulk {
		t.Errorf("expected non-legal site: filter to NOT force crucial, got tier=%q rule=%q", tier, rule)
	}
}

func TestClassifyTier_ExplicitParamHonored(t *testing.T) {
	cases := []struct{ requested, want string }{
		{"crucial", TierCrucial},
		{"standard", TierStandard},
		{"bulk", TierBulk},
	}
	for _, c := range cases {
		tier, rule := classifyTier("misconduct", c.requested)
		if tier != c.want {
			t.Errorf("classifyTier(misconduct, %q) tier = %q, want %q", c.requested, tier, c.want)
		}
		if rule != "explicit_param" {
			t.Errorf("classifyTier(misconduct, %q) rule = %q, want explicit_param", c.requested, rule)
		}
	}
}

// classifyTier itself does not lowercase its `requested` argument — the HTTP
// handler (router.go) does that before calling it. A non-lowercased value
// must fall back to the "standard" default rather than matching nothing and
// panicking.
func TestClassifyTier_NonLowercasedRequestedFallsBackToDefault(t *testing.T) {
	tier, rule := classifyTier("misconduct", "CRUCIAL")
	if tier != TierStandard || rule != "default" {
		t.Errorf("classifyTier(misconduct, CRUCIAL) = (%q,%q), want (standard,default)", tier, rule)
	}
}

func TestClassifyTier_DefaultIsStandard(t *testing.T) {
	tier, rule := classifyTier("misconduct", "")
	if tier != TierStandard {
		t.Errorf("default tier = %q, want standard", tier)
	}
	if rule != "default" {
		t.Errorf("default rule = %q, want default", rule)
	}
}

func TestClassifyTier_InvalidRequestedFallsBackToDefault(t *testing.T) {
	tier, rule := classifyTier("misconduct", "urgent") // not a real tier name
	if tier != TierStandard || rule != "default" {
		t.Errorf("invalid requested tier -> got (%q,%q), want (standard,default)", tier, rule)
	}
}

func TestShadowSuggestedTier_NoOpByDefault(t *testing.T) {
	if shadowClassifier != nil {
		t.Fatalf("task scope: shadowClassifier must remain nil — no model wired")
	}
	if got := shadowSuggestedTier("any query"); got != "" {
		t.Errorf("shadowSuggestedTier with nil classifier = %q, want empty", got)
	}
}

type fakeShadow struct{ tier string }

func (f fakeShadow) SuggestTier(string) string { return f.tier }

func TestShadowSuggestedTier_UsesInstalledClassifierWithoutAffectingRouting(t *testing.T) {
	old := shadowClassifier
	defer func() { shadowClassifier = old }()
	shadowClassifier = fakeShadow{tier: "crucial"}

	if got := shadowSuggestedTier("misconduct"); got != "crucial" {
		t.Errorf("shadowSuggestedTier = %q, want crucial", got)
	}
	// Must NEVER influence the rules-based classification.
	tier, rule := classifyTier("misconduct", "bulk")
	if tier != TierBulk || rule != "explicit_param" {
		t.Errorf("shadow classifier leaked into routing decision: tier=%q rule=%q", tier, rule)
	}
}

func TestLoadTierProviderOrder_DefaultsWhenFileMissing(t *testing.T) {
	order := loadTierProviderOrder(filepath.Join(t.TempDir(), "does-not-exist.json"))
	if order[TierCrucial][0] != "exa" {
		t.Errorf("crucial order[0] = %q, want exa (task default)", order[TierCrucial][0])
	}
	if order[TierStandard][0] != "tinyfish" {
		t.Errorf("standard order[0] = %q, want tinyfish (task default)", order[TierStandard][0])
	}
	if order[TierBulk][0] != "searxng" {
		t.Errorf("bulk order[0] = %q, want searxng (task default)", order[TierBulk][0])
	}
}

func TestLoadTierProviderOrder_FileOverridesOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tier_providers.json")
	custom := `{"crucial": ["tavily", "exa", "searxng"]}`
	if err := os.WriteFile(path, []byte(custom), 0o644); err != nil {
		t.Fatal(err)
	}
	order := loadTierProviderOrder(path)
	if order[TierCrucial][0] != "tavily" {
		t.Errorf("expected file override to re-order crucial, got %v", order[TierCrucial])
	}
	// Tiers NOT present in the override file must still fall back to default.
	if order[TierBulk][0] != "searxng" {
		t.Errorf("expected bulk to fall back to default when absent from override file, got %v", order[TierBulk])
	}
}

func TestLoadTierProviderOrder_InvalidJSONFallsBackToDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tier_providers.json")
	if err := os.WriteFile(path, []byte("{not valid"), 0o644); err != nil {
		t.Fatal(err)
	}
	order := loadTierProviderOrder(path)
	if order[TierCrucial][0] != "exa" {
		t.Errorf("expected fallback to defaults on invalid JSON, got %v", order[TierCrucial])
	}
}
