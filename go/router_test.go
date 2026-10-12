package main

import "testing"

// P3: `site:`-filtered queries must be treated as caller-chosen so rotation
// (which would pin the query to a 2-3 engine subset that may not honor site:)
// is skipped in favor of SearXNG's full enabled engine set.
func TestHasSiteFilter(t *testing.T) {
	cases := []struct {
		name string
		q    string
		want bool
	}{
		{"canary: lowercase site:", "site:lawphil.net Hulst PR Builders", true},
		{"mixed case Site:", "Site:LawPhil.net Hulst PR Builders", true},
		{"site: mid-query", "Hulst PR Builders (site:lawphil.net OR site:chanrobles.com)", true},
		{"no site filter", "RA 4726 Condominium Act Philippines", false},
		{"empty query", "", false},
		{"word containing site but not a filter", "website design tips", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := hasSiteFilter(c.q); got != c.want {
				t.Errorf("hasSiteFilter(%q) = %v, want %v", c.q, got, c.want)
			}
		})
	}
}

func TestHasBang(t *testing.T) {
	cases := []struct {
		name string
		q    string
		want bool
	}{
		{"leading bang", "!google misconduct", true},
		{"double bang", "!!wikipedia rule", true},
		{"bang mid-query", "misconduct !general", true},
		{"no bang", "misconduct", false},
		{"empty", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := hasBang(c.q); got != c.want {
				t.Errorf("hasBang(%q) = %v, want %v", c.q, got, c.want)
			}
		})
	}
}

// P2: rotation must only apply to the general/unset category — a caller that
// specified a non-general category (science/it/news/...) must have that
// category preserved all the way to SearXNG, not silently discarded by
// rotation (the pre-fix behavior called params.Del("categories") whenever it
// rotated, which fired on every query that didn't already pick its own
// engines — i.e. nearly every call from every consumer).
func TestDecideRotation(t *testing.T) {
	cases := []struct {
		name           string
		callerChoseEng bool
		categories     string
		wantRotate     bool
	}{
		{"no categories, no caller choice -> rotate", false, "", true},
		{"explicit general category, no caller choice -> rotate", false, "general", true},
		{"science category, no caller choice -> do NOT rotate (preserve category)", false, "science", false},
		{"it category, no caller choice -> do NOT rotate", false, "it", false},
		{"news category, no caller choice -> do NOT rotate", false, "news", false},
		{"caller chose engines, general category -> do NOT rotate", true, "general", false},
		{"caller chose engines, science category -> do NOT rotate", true, "science", false},
		{"caller chose engines, no category -> do NOT rotate", true, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := decideRotation(c.callerChoseEng, c.categories); got != c.wantRotate {
				t.Errorf("decideRotation(%v, %q) = %v, want %v",
					c.callerChoseEng, c.categories, got, c.wantRotate)
			}
		})
	}
}

// P4: the dead `google` scraper hot-spare must be gone from engineFamilies,
// replaced by `startpage` (which is actually enabled on :8890 and proxies
// Google's index), so the family still contributes a result when `google cse`'s
// daily quota is spent.
func TestEngineFamiliesGoogleSpareIsStartpageNotDeadGoogle(t *testing.T) {
	sawStartpage := false
	for _, fam := range engineFamilies {
		for _, name := range fam {
			if name == "google" {
				t.Errorf("dead `google` scraper hot-spare still present in engineFamilies: %v", fam)
			}
			if name == "startpage" {
				sawStartpage = true
			}
		}
	}
	if !sawStartpage {
		t.Errorf("expected `startpage` hot-spare somewhere in engineFamilies, got %v", engineFamilies)
	}
	// Every engine referenced by a family must have a rate budget, or
	// selectEngines() silently treats it as nil and the family contributes
	// nothing when its primary is exhausted — the exact P4 bug with `google`.
	for _, fam := range engineFamilies {
		for _, name := range fam {
			if _, ok := engineRateDefaults[name]; !ok {
				t.Errorf("engineFamilies member %q has no engineRateDefaults budget -> dead weight", name)
			}
		}
	}
}
