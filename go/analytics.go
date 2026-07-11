package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// analytics captures per-request telemetry so suspensions can be diagnosed
// empirically ("engine X got suspended after ~N req/min") instead of guessed.
// Two outputs:
//   - a daily-rotated JSONL event log (cache/analytics/events-YYYY-MM-DD.jsonl)
//   - live in-memory counters exposed at /stats
//
// The richest signal is SearXNG's `unresponsive_engines` in each response, which
// names the exact engine + reason (e.g. "Suspended: too many requests").

var analyticsDir = envOr("ANALYTICS_DIR", "/home/cmark/server/searxng-rotation/cache/analytics")

// ---- event record (one JSON line per request) ----
type searchEvent struct {
	TS          string              `json:"ts"`
	Query       string              `json:"query"`
	Tier        string              `json:"tier"`   // direct | proxied | none
	Engines     string              `json:"engines"` // engines= we sent (rotation)
	Results     int                 `json:"results"`
	Status      int                 `json:"status"` // upstream HTTP status
	LatencyMS   int64               `json:"latency_ms"`
	Unresponsive []unresponsiveEng  `json:"unresponsive,omitempty"`
	Outcome     string              `json:"outcome"` // ok | empty | deadletter
}

type unresponsiveEng struct {
	Engine string `json:"engine"`
	Reason string `json:"reason"`
}

// ---- in-memory counters (since boot) ----
type engineStat struct {
	Requests     int64  `json:"requests"`      // times we sent this engine
	Suspensions  int64  `json:"suspensions"`   // times it appeared in unresponsive
	LastSuspend  string `json:"last_suspend,omitempty"`
	LastReason   string `json:"last_reason,omitempty"`
}

var (
	statMu       sync.Mutex
	statStart    = time.Now()
	totalReqs    int64
	deadletters  int64
	engineStats  = map[string]*engineStat{}
	lastReqAt    time.Time
	gapSumMS     int64 // sum of inter-request gaps (for avg burst spacing)
	gapCount     int64

	evMu   sync.Mutex
	evFile *os.File
	evDay  string
)

// parseUnresponsive extracts SearXNG's unresponsive_engines array, which is a
// list of [engine, reason] pairs.
func parseUnresponsive(body []byte) []unresponsiveEng {
	var env struct {
		Unresponsive [][]json.RawMessage `json:"unresponsive_engines"`
	}
	if json.Unmarshal(body, &env) != nil {
		return nil
	}
	var out []unresponsiveEng
	for _, pair := range env.Unresponsive {
		var eng, reason string
		if len(pair) >= 1 {
			_ = json.Unmarshal(pair[0], &eng)
		}
		if len(pair) >= 2 {
			_ = json.Unmarshal(pair[1], &reason)
		}
		if eng != "" {
			out = append(out, unresponsiveEng{Engine: eng, Reason: reason})
		}
	}
	return out
}

// recordEvent updates counters and appends the event to the JSONL log.
func recordEvent(ev searchEvent) {
	now := time.Now()
	statMu.Lock()
	totalReqs++
	if ev.Outcome == "deadletter" {
		deadletters++
	}
	// inter-request gap (burst spacing)
	if !lastReqAt.IsZero() {
		gapSumMS += now.Sub(lastReqAt).Milliseconds()
		gapCount++
	}
	lastReqAt = now
	// per-engine request counts (engines we asked for)
	for _, name := range splitCSV(ev.Engines) {
		s := engineStats[name]
		if s == nil {
			s = &engineStat{}
			engineStats[name] = s
		}
		s.Requests++
	}
	// per-engine suspensions (from response)
	for _, u := range ev.Unresponsive {
		s := engineStats[u.Engine]
		if s == nil {
			s = &engineStat{}
			engineStats[u.Engine] = s
		}
		if isSuspension(u.Reason) {
			s.Suspensions++
			s.LastSuspend = ev.TS
			s.LastReason = u.Reason
		}
	}
	statMu.Unlock()

	appendEvent(ev)
}

func isSuspension(reason string) bool {
	r := strings.ToLower(reason)
	return strings.Contains(r, "suspend") || strings.Contains(r, "too many") ||
		strings.Contains(r, "captcha") || strings.Contains(r, "access denied") ||
		strings.Contains(r, "429") || strings.Contains(r, "403")
}

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}

// appendEvent writes one JSON line, rotating the file daily.
func appendEvent(ev searchEvent) {
	evMu.Lock()
	defer evMu.Unlock()
	day := time.Now().UTC().Format("2006-01-02")
	if evFile == nil || day != evDay {
		if evFile != nil {
			evFile.Close()
		}
		_ = os.MkdirAll(analyticsDir, 0o755)
		f, err := os.OpenFile(filepath.Join(analyticsDir, "events-"+day+".jsonl"),
			os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return
		}
		evFile = f
		evDay = day
	}
	b, _ := json.Marshal(ev)
	evFile.Write(append(b, '\n'))
}

// statsSnapshot returns the live /stats payload.
func statsSnapshot() map[string]any {
	statMu.Lock()
	defer statMu.Unlock()
	uptime := time.Since(statStart).Seconds()
	reqPerMin := 0.0
	if uptime > 0 {
		reqPerMin = float64(totalReqs) / (uptime / 60)
	}
	avgGap := 0.0
	if gapCount > 0 {
		avgGap = float64(gapSumMS) / float64(gapCount)
	}
	engs := map[string]any{}
	for name, s := range engineStats {
		engs[name] = *s
	}
	return map[string]any{
		"uptime_seconds":      int(uptime),
		"total_requests":      totalReqs,
		"requests_per_min":    reqPerMin,
		"deadletters":         deadletters,
		"avg_request_gap_ms":  int(avgGap),
		"engines":             engs,
	}
}
