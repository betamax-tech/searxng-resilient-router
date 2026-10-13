package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func testCapDefs() []CapWindowDef {
	return []CapWindowDef{
		{Provider: "exa", Key: "exa_monthly", Mode: "cost", Period: PeriodMonthly, HardCap: 9.00},
		{Provider: "brave", Key: "brave_monthly", Mode: "count", Period: PeriodMonthly, HardCap: 3},
		{Provider: "tinyfish", Key: "tinyfish_minute", Mode: "count", Period: PeriodMinute, HardCap: 2},
	}
}

func tmpCapsPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "provider_caps.json")
}

func TestCapLimiter_AllowsUnderCapBlocksAtCap(t *testing.T) {
	path := tmpCapsPath(t)
	c := NewCapLimiter(path, testCapDefs())
	fixed := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return fixed }

	for i := 0; i < 3; i++ {
		ok, reason := c.Allowed("brave")
		if !ok {
			t.Fatalf("call %d: expected allowed, got blocked: %s", i, reason)
		}
		c.RecordCount("brave", 1)
	}
	ok, reason := c.Allowed("brave")
	if ok {
		t.Fatalf("expected brave BLOCKED at cap (3/3), got allowed")
	}
	if reason == "" {
		t.Errorf("expected a non-empty block reason")
	}
}

// Brave hard stop: this account has a card on file, so ANY cap-reached or
// unknown state must refuse the call outright — never silently pass through.
func TestCapLimiter_BraveHardStopAtCap(t *testing.T) {
	path := tmpCapsPath(t)
	c := NewCapLimiter(path, testCapDefs())
	fixed := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return fixed }

	c.RecordCount("brave", 3) // exactly at the test cap of 3
	ok, _ := c.Allowed("brave")
	if ok {
		t.Fatalf("brave must be a HARD STOP once its monthly cap is reached")
	}

	// Simulate "unknown state": a provider with no configured window at all
	// must ALSO fail closed, never assumed OK.
	ok, reason := c.Allowed("totally_unconfigured_provider")
	if ok {
		t.Fatalf("an unconfigured provider must fail closed, got allowed")
	}
	if reason != "no_cap_configured_fail_closed" {
		t.Errorf("unexpected reason for unconfigured provider: %s", reason)
	}
}

func TestCapLimiter_RestartPersistence(t *testing.T) {
	path := tmpCapsPath(t)
	fixed := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)

	c1 := NewCapLimiter(path, testCapDefs())
	c1.now = func() time.Time { return fixed }
	c1.RecordCount("brave", 2)
	c1.RecordCost("exa", 4.50)

	// Simulate a process restart: brand-new limiter instance, same disk path.
	c2 := NewCapLimiter(path, testCapDefs())
	c2.now = func() time.Time { return fixed }

	snap := c2.Snapshot()
	windows := snap["windows"].(map[string]any)
	brave := windows["brave_monthly"].(map[string]any)
	if used := brave["used"].(float64); used != 2 {
		t.Errorf("expected brave usage to survive restart as 2, got %v", used)
	}
	exa := windows["exa_monthly"].(map[string]any)
	if used := exa["used"].(float64); used != 4.50 {
		t.Errorf("expected exa cost usage to survive restart as 4.50, got %v", used)
	}

	// And the on-disk file really exists and is valid JSON (atomic write landed).
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected cap state file to exist on disk: %v", err)
	}
}

func TestCapLimiter_PeriodReset(t *testing.T) {
	path := tmpCapsPath(t)
	c := NewCapLimiter(path, testCapDefs())

	monthA := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return monthA }
	c.RecordCount("brave", 3) // hits the cap of 3

	ok, _ := c.Allowed("brave")
	if ok {
		t.Fatalf("expected brave blocked within October")
	}

	// Roll the clock into November — the monthly window must reset.
	monthB := time.Date(2026, 11, 1, 0, 0, 1, 0, time.UTC)
	c.now = func() time.Time { return monthB }

	ok, reason := c.Allowed("brave")
	if !ok {
		t.Fatalf("expected brave ALLOWED after monthly period reset, got blocked: %s", reason)
	}
}

func TestCapLimiter_MinuteWindowResetsIndependently(t *testing.T) {
	path := tmpCapsPath(t)
	c := NewCapLimiter(path, testCapDefs())

	t0 := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return t0 }
	c.RecordCount("tinyfish", 2) // hits the per-minute cap of 2

	ok, _ := c.Allowed("tinyfish")
	if ok {
		t.Fatalf("expected tinyfish blocked within the same minute")
	}

	t1 := t0.Add(61 * time.Second) // next minute bucket
	c.now = func() time.Time { return t1 }
	ok, reason := c.Allowed("tinyfish")
	if !ok {
		t.Fatalf("expected tinyfish allowed in the next minute window, got blocked: %s", reason)
	}
}

// Fail-closed on unknown usage: Exa is cost-tracked. If the provider's
// response doesn't report costDollars, we cannot know the real spend, so the
// router must refuse further Exa calls for the rest of the period rather
// than silently assume $0.
func TestCapLimiter_RecordUnknownFailClosed(t *testing.T) {
	path := tmpCapsPath(t)
	c := NewCapLimiter(path, testCapDefs())
	fixed := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return fixed }

	ok, _ := c.Allowed("exa")
	if !ok {
		t.Fatalf("expected exa allowed before any unknown-usage event")
	}
	c.RecordUnknownFailClosed("exa", "provider_did_not_report_cost")
	ok, reason := c.Allowed("exa")
	if ok {
		t.Fatalf("expected exa FAIL CLOSED after unknown usage, got allowed")
	}
	if reason == "" {
		t.Errorf("expected a non-empty fail-closed reason")
	}
}

// Fail-closed on a corrupted on-disk state file: the limiter must refuse
// EVERY provider rather than assume zero usage and keep spending.
func TestCapLimiter_CorruptStateFileFailsClosedForAllProviders(t *testing.T) {
	path := tmpCapsPath(t)
	if err := os.WriteFile(path, []byte("{not valid json"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := NewCapLimiter(path, testCapDefs())

	for _, prov := range []string{"exa", "brave", "tinyfish"} {
		ok, reason := c.Allowed(prov)
		if ok {
			t.Errorf("provider %s: expected fail-closed due to corrupt state, got allowed", prov)
		}
		if reason == "" {
			t.Errorf("provider %s: expected a non-empty degraded reason", prov)
		}
	}
}

func TestCapLimiter_AtomicWriteLeavesNoTempFile(t *testing.T) {
	path := tmpCapsPath(t)
	c := NewCapLimiter(path, testCapDefs())
	c.RecordCount("brave", 1)
	if _, err := os.Stat(path + ".tmp"); err == nil {
		t.Errorf("expected no leftover .tmp file after an atomic write")
	}
}
