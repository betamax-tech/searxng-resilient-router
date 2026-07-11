// analyze — read the router's JSONL analytics and report what most likely
// triggers engine suspensions. Build: go build -o analyze ./analyze
//
// For each engine it prints: total requests, suspension count, and — the key
// diagnostic — the request rate to that engine in the 5 minutes BEFORE each
// suspension event (your empirical rate limit, vs. the researched guess).
//
// Usage:
//   ./analyze                      # today's log
//   ./analyze /path/events-*.jsonl # specific file(s)
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const analyticsDir = "/home/cmark/server/searxng-rotation/cache/analytics"
const windowBeforeSuspend = 5 * time.Minute

type event struct {
	TS           string `json:"ts"`
	Query        string `json:"query"`
	Tier         string `json:"tier"`
	Engines      string `json:"engines"`
	Results      int    `json:"results"`
	Status       int    `json:"status"`
	LatencyMS    int64  `json:"latency_ms"`
	Unresponsive []struct {
		Engine string `json:"engine"`
		Reason string `json:"reason"`
	} `json:"unresponsive"`
	Outcome string `json:"outcome"`
	t       time.Time
}

func isSuspension(reason string) bool {
	r := strings.ToLower(reason)
	for _, s := range []string{"suspend", "too many", "captcha", "access denied", "429", "403"} {
		if strings.Contains(r, s) {
			return true
		}
	}
	return false
}

func main() {
	files := os.Args[1:]
	if len(files) == 0 {
		files = []string{filepath.Join(analyticsDir, "events-"+time.Now().UTC().Format("2006-01-02")+".jsonl")}
	}

	var events []event
	for _, f := range files {
		fh, err := os.Open(f)
		if err != nil {
			fmt.Fprintf(os.Stderr, "skip %s: %v\n", f, err)
			continue
		}
		sc := bufio.NewScanner(fh)
		sc.Buffer(make([]byte, 1024*1024), 1024*1024)
		for sc.Scan() {
			var e event
			if json.Unmarshal(sc.Bytes(), &e) != nil {
				continue
			}
			e.t, _ = time.Parse(time.RFC3339Nano, e.TS)
			events = append(events, e)
		}
		fh.Close()
	}
	if len(events) == 0 {
		fmt.Println("no events found.")
		return
	}
	sort.Slice(events, func(i, j int) bool { return events[i].t.Before(events[j].t) })

	// per-engine: total times requested (engines= contained it)
	reqCount := map[string]int{}
	for _, e := range events {
		for _, name := range splitCSV(e.Engines) {
			reqCount[name]++
		}
	}

	// suspension events: (engine, time) — first time each engine flips to suspended
	type susp struct {
		engine string
		at     time.Time
		reason string
	}
	var suspensions []susp
	prevSuspended := map[string]bool{}
	for _, e := range events {
		nowSusp := map[string]bool{}
		for _, u := range e.Unresponsive {
			if isSuspension(u.Reason) {
				nowSusp[u.Engine] = true
				if !prevSuspended[u.Engine] {
					suspensions = append(suspensions, susp{u.Engine, e.t, u.Reason})
				}
			}
		}
		prevSuspended = nowSusp
	}

	// report
	span := events[len(events)-1].t.Sub(events[0].t)
	fmt.Printf("=== Router analytics ===\n")
	fmt.Printf("events: %d | span: %s | overall rate: %.1f req/min\n\n",
		len(events), span.Round(time.Second), float64(len(events))/(span.Minutes()+0.001))

	fmt.Printf("--- per-engine request counts (times included in a query) ---\n")
	type kv struct {
		k string
		v int
	}
	var rc []kv
	for k, v := range reqCount {
		rc = append(rc, kv{k, v})
	}
	sort.Slice(rc, func(i, j int) bool { return rc[i].v > rc[j].v })
	for _, p := range rc {
		fmt.Printf("  %-12s %5d requests\n", p.k, p.v)
	}

	fmt.Printf("\n--- SUSPENSION events + request rate in the %s before each ---\n", windowBeforeSuspend)
	if len(suspensions) == 0 {
		fmt.Println("  none recorded 🎉")
	}
	for _, s := range suspensions {
		// count requests to THIS engine in the window before the suspension
		n := 0
		for _, e := range events {
			if e.t.After(s.at.Add(-windowBeforeSuspend)) && e.t.Before(s.at) {
				for _, name := range splitCSV(e.Engines) {
					if name == s.engine {
						n++
					}
				}
			}
		}
		ratePerMin := float64(n) / windowBeforeSuspend.Minutes()
		fmt.Printf("  %s  %-11s suspended (%s)\n", s.at.Format("15:04:05"), s.engine, s.reason)
		fmt.Printf("        -> %d requests to %s in prior %s = %.1f req/min (empirical trigger point)\n",
			n, s.engine, windowBeforeSuspend, ratePerMin)
	}
}

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	cur := ""
	for _, r := range s {
		if r == ',' {
			if cur != "" {
				out = append(out, cur)
			}
			cur = ""
		} else {
			cur += string(r)
		}
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}
