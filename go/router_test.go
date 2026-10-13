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

// R1 (2026-10-13, anti-blocking investigation doc): engineFamilies and
// independentPool must contain ONLY the six engines verified live against
// our egress IP (bing, yandex, "google cse", mwmbl, yep, wiby). The scrapers
// measured 84-96% broken across 94 public SearXNG instances on searx.space
// (duckduckgo, startpage, qwant, yahoo, mojeek, brave, and the dead `google`
// HTML scraper) must never appear here — including `startpage`, which R1
// itself retires (superseding the earlier P4 fix that had relied on it as a
// hot-spare). Every referenced engine must also carry a rate budget, or
// selectEngines() silently treats it as nil and the family/pool contributes
// nothing when exhausted — the exact P4 regression this guards against.
func TestEngineFamiliesAndIndependentsOnlyLiveR1Engines(t *testing.T) {
	deadEngines := map[string]bool{
		"google": true, "duckduckgo": true, "startpage": true,
		"qwant": true, "yahoo": true, "mojeek": true, "brave": true,
	}
	liveEngines := map[string]bool{
		"google cse": true, "bing": true, "yandex": true,
		"mwmbl": true, "yep": true, "wiby": true,
	}

	var all []string
	for _, fam := range engineFamilies {
		all = append(all, fam...)
	}
	all = append(all, independentPool...)

	for _, name := range all {
		if deadEngines[name] {
			t.Errorf("globally-broken engine %q still referenced in engineFamilies/independentPool", name)
		}
		if !liveEngines[name] {
			t.Errorf("engine %q is not one of the R1 verified-live engines", name)
		}
		if _, ok := engineRateDefaults[name]; !ok {
			t.Errorf("engine %q has no engineRateDefaults budget -> dead weight", name)
		}
	}

	// bing and "google cse" must still be the two primary families (each its
	// own index); yandex/mwmbl/yep/wiby rotate as independents.
	foundBing, foundGoogleCSE := false, false
	for _, fam := range engineFamilies {
		for _, name := range fam {
			if name == "bing" {
				foundBing = true
			}
			if name == "google cse" {
				foundGoogleCSE = true
			}
		}
	}
	if !foundBing || !foundGoogleCSE {
		t.Errorf("expected bing and `google cse` as engineFamilies primaries, got %v", engineFamilies)
	}
}

func TestIndependentsPerQueryMatchesPoolDiversity(t *testing.T) {
	if independentsPerQuery < 1 || independentsPerQuery > len(independentPool) {
		t.Errorf("independentsPerQuery=%d out of range for pool size %d", independentsPerQuery, len(independentPool))
	}
}
