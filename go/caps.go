// caps.go — per-provider spend/usage caps for the paid search providers
// (Exa, Tavily, Brave, TinyFish), persisted atomically to disk so they
// survive a router restart. Owner decision (hard constraint, see the task
// doc): "Zero expenditure, ever." Paid providers are used ONLY inside their
// free allowance; this file makes it structurally impossible to bill past it.
//
// Design:
//   - Each provider has one or more CapWindowDef "windows" (e.g. TinyFish has
//     a daily, hourly AND per-minute window — all three must pass).
//   - Windows are either "cost" (actual dollars, e.g. Exa's costDollars) or
//     "count" (request count, e.g. Brave/Tavily/TinyFish).
//   - Allowed() must return true for EVERY window before a provider is
//     called; RecordCost/RecordCount persist the real usage immediately
//     after a successful call.
//   - If usage can't be determined (corrupt state file, or a cost-mode
//     provider that didn't report its own cost), the limiter FAILS CLOSED:
//     either every provider is blocked (corrupt state) or that one provider
//     is saturated for the rest of the period (RecordUnknownFailClosed).
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type Period string

const (
	PeriodMonthly Period = "monthly"
	PeriodDaily   Period = "daily"
	PeriodHourly  Period = "hourly"
	PeriodMinute  Period = "minute"
)

// periodKey returns the bucket identifier for t under period p. A window's
// usage resets automatically the instant this string changes (e.g. the
// monthly key flips on the 1st of the month, UTC).
func periodKey(p Period, t time.Time) string {
	t = t.UTC()
	switch p {
	case PeriodMonthly:
		return t.Format("2006-01")
	case PeriodHourly:
		return t.Format("2006-01-02T15")
	case PeriodMinute:
		return t.Format("2006-01-02T15:04")
	default: // PeriodDaily
		return t.Format("2006-01-02")
	}
}

type CapWindowDef struct {
	Provider string
	Key      string
	Mode     string // "cost" | "count"
	Period   Period
	HardCap  float64
}

// capWindowDefs enforces hard caps at ~90% of each provider's free allowance
// per period, so a bug or burst can NEVER cross into a billed overage.
// Numbers per the owner's task doc, verified against provider docs
// 2026-10-13 (using the LOWER number wherever docs disagreed):
//
//   - Exa: $10 credit/month, resets on the 1st, no card -> cap $9.00.
//     Cost-tracked via the API's own `costDollars` response field. A response
//     missing that field means actual cost is UNKNOWN -> fail closed
//     (RecordUnknownFailClosed), never assumed to be zero.
//   - Tavily: 1,000 credits/month, no card (basic search = 1 credit) -> cap
//     900.
//   - Brave: $5 credit/month ~= 1,000 requests, but this account HAS a card
//     on file so overage WOULD be billed -> cap 900/month, and any unknown
//     state (corrupt cap file, missing window) is treated as a hard stop,
//     never "assume OK".
//   - TinyFish: free up to 12,000/day, 30/min, 500/hr -> daily cap 10,800
//     (90% of 12,000) PLUS the task's exact rate-limit numbers: 450/hr,
//     25/min. All three windows must pass.
var capWindowDefs = []CapWindowDef{
	{Provider: "exa", Key: "exa_monthly", Mode: "cost", Period: PeriodMonthly, HardCap: 9.00},
	{Provider: "tavily", Key: "tavily_monthly", Mode: "count", Period: PeriodMonthly, HardCap: 900},
	{Provider: "brave", Key: "brave_monthly", Mode: "count", Period: PeriodMonthly, HardCap: 900},
	{Provider: "tinyfish", Key: "tinyfish_daily", Mode: "count", Period: PeriodDaily, HardCap: 10800},
	{Provider: "tinyfish", Key: "tinyfish_hourly", Mode: "count", Period: PeriodHourly, HardCap: 450},
	{Provider: "tinyfish", Key: "tinyfish_minute", Mode: "count", Period: PeriodMinute, HardCap: 25},
}

type windowUsage struct {
	PeriodKey string  `json:"period_key"`
	Used      float64 `json:"used"`
}

// CapLimiter enforces the windows above, persisting atomically (write to a
// temp file, then rename) so usage survives a router restart.
type CapLimiter struct {
	mu        sync.Mutex
	path      string
	defs      []CapWindowDef
	usage     map[string]*windowUsage
	degraded  bool
	degReason string
	now       func() time.Time // injectable for tests
}

func NewCapLimiter(path string, defs []CapWindowDef) *CapLimiter {
	c := &CapLimiter{path: path, defs: defs, usage: map[string]*windowUsage{}, now: time.Now}
	c.load()
	return c
}

// load reads the persisted usage file. A missing file is a fresh install
// (zero usage everywhere — fine). A file that EXISTS but can't be read or
// parsed means we cannot determine real usage, so per the owner's "fail
// closed" decision the limiter blocks every provider until the state is
// restored (never silently assumes zero and starts spending again).
func (c *CapLimiter) load() {
	b, err := os.ReadFile(c.path)
	if err != nil {
		if os.IsNotExist(err) {
			return
		}
		c.degraded = true
		c.degReason = "cap_state_unreadable: " + err.Error()
		log.Printf("CAP LIMITER DEGRADED (fail-closed, all providers blocked): %s", c.degReason)
		return
	}
	var m map[string]*windowUsage
	if err := json.Unmarshal(b, &m); err != nil {
		c.degraded = true
		c.degReason = "cap_state_corrupt: " + err.Error()
		log.Printf("CAP LIMITER DEGRADED (fail-closed, all providers blocked): %s", c.degReason)
		return
	}
	c.usage = m
}

func (c *CapLimiter) persistLocked() {
	b, err := json.Marshal(c.usage)
	if err != nil {
		log.Printf("CAP LIMITER: failed to marshal usage: %v", err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(c.path), 0o755); err != nil {
		log.Printf("CAP LIMITER: failed to mkdir %s: %v", filepath.Dir(c.path), err)
		return
	}
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		log.Printf("CAP LIMITER: failed to write %s: %v", tmp, err)
		return
	}
	if err := os.Rename(tmp, c.path); err != nil {
		log.Printf("CAP LIMITER: failed to rename %s -> %s: %v", tmp, c.path, err)
	}
}

func (c *CapLimiter) defsFor(provider string) []CapWindowDef {
	var out []CapWindowDef
	for _, d := range c.defs {
		if d.Provider == provider {
			out = append(out, d)
		}
	}
	return out
}

// rolledLocked returns the in-memory usage record for a window, resetting it
// to zero the instant the period's key changes (e.g. a new month). Caller
// must hold c.mu.
func (c *CapLimiter) rolledLocked(d CapWindowDef) *windowUsage {
	key := periodKey(d.Period, c.now())
	u := c.usage[d.Key]
	if u == nil {
		u = &windowUsage{PeriodKey: key}
		c.usage[d.Key] = u
		return u
	}
	if u.PeriodKey != key {
		u.PeriodKey = key
		u.Used = 0
	}
	return u
}

// Allowed reports whether `provider` may be called right now, and if not,
// the reason (useful for failover logging). A provider with NO configured
// cap window fails closed too: an uncapped paid provider is exactly the
// mistake this system exists to prevent.
func (c *CapLimiter) Allowed(provider string) (bool, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.degraded {
		return false, c.degReason
	}
	defs := c.defsFor(provider)
	if len(defs) == 0 {
		return false, "no_cap_configured_fail_closed"
	}
	for _, d := range defs {
		u := c.rolledLocked(d)
		if u.Used >= d.HardCap {
			return false, fmt.Sprintf("cap_reached:%s:%.4f/%.4f", d.Key, u.Used, d.HardCap)
		}
	}
	return true, ""
}

// RecordCount adds n units to every COUNT-mode window configured for
// provider (e.g. +1 request), and persists immediately.
func (c *CapLimiter) RecordCount(provider string, n float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	changed := false
	for _, d := range c.defsFor(provider) {
		if d.Mode != "count" {
			continue
		}
		u := c.rolledLocked(d)
		u.Used += n
		changed = true
	}
	if changed {
		c.persistLocked()
	}
}

// RecordCost adds a KNOWN dollar amount to every COST-mode window configured
// for provider, and persists immediately.
func (c *CapLimiter) RecordCost(provider string, dollars float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	changed := false
	for _, d := range c.defsFor(provider) {
		if d.Mode != "cost" {
			continue
		}
		u := c.rolledLocked(d)
		u.Used += dollars
		changed = true
	}
	if changed {
		c.persistLocked()
	}
}

// RecordUnknownFailClosed is called when a provider's actual usage for a
// call that DID happen cannot be determined (e.g. a cost-mode API that did
// not report its own cost field in the response). Per the owner's "fail
// closed" decision, this immediately saturates every window for the
// provider so no further calls happen this period — erring toward spending
// nothing over spending un-metered. Logged loudly; never silent.
func (c *CapLimiter) RecordUnknownFailClosed(provider, reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	log.Printf("CAP LIMITER: %s usage UNKNOWN (%s) -> fail-closed for the rest of this period", provider, reason)
	for _, d := range c.defsFor(provider) {
		u := c.rolledLocked(d)
		if u.Used < d.HardCap {
			u.Used = d.HardCap
		}
	}
	c.persistLocked()
}

// Snapshot returns a /stats-ready view of every window's usage vs cap.
func (c *CapLimiter) Snapshot() map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[string]any{"degraded": c.degraded}
	if c.degraded {
		out["degraded_reason"] = c.degReason
	}
	windows := map[string]any{}
	for _, d := range c.defs {
		u := c.rolledLocked(d)
		windows[d.Key] = map[string]any{
			"provider":   d.Provider,
			"mode":       d.Mode,
			"period":     string(d.Period),
			"period_key": u.PeriodKey,
			"used":       u.Used,
			"cap":        d.HardCap,
			"remaining":  d.HardCap - u.Used,
			"blocked":    u.Used >= d.HardCap,
		}
	}
	out["windows"] = windows
	return out
}
