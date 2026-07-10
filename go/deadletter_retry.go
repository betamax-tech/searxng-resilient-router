//go:build ignore

// deadletter_retry.go — reprocesses queries that failed ALL router tiers.
// Build: go build -o deadletter-retry deadletter_retry.go
//
// For each *.json in the dead-letter dir, re-issue it through the router. On
// success (results > 0) the entry is deleted (query recovered). On repeated
// failure the attempt count increments; after maxAttempts the entry is moved to
// a 'failed/' subdir so it's preserved but no longer retried — guaranteeing no
// query is ever silently dropped.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

const (
	routerSearch = "http://127.0.0.1:8899/search"
	maxAttempts  = 5
	perReqPause  = 3 * time.Second // gentle: avoid hammering the router
)

var (
	dlDir     = "/home/cmark/server/searxng-rotation/cache/deadletter"
	failedDir = filepath.Join("/home/cmark/server/searxng-rotation/cache/deadletter", "failed")
)

type record struct {
	Query    string `json:"query"`
	QueuedAt string `json:"queued_at"`
	Attempts int    `json:"attempts"`
}

func logf(f string, a ...any) {
	fmt.Printf("["+time.Now().UTC().Format(time.RFC3339)+"] "+f+"\n", a...)
}

func routerResults(query string) int {
	u := routerSearch + "?" + url.Values{"q": {query}, "format": {"json"}}.Encode()
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Get(u)
	if err != nil {
		return -1
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var env struct {
		Results []json.RawMessage `json:"results"`
	}
	if json.Unmarshal(b, &env) != nil {
		return -1
	}
	return len(env.Results)
}

func main() {
	entries, err := os.ReadDir(dlDir)
	if err != nil {
		logf("no dead-letter dir: %v", err)
		return
	}
	var pending int
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		pending++
		fp := filepath.Join(dlDir, e.Name())
		raw, err := os.ReadFile(fp)
		if err != nil {
			continue
		}
		var rec record
		if json.Unmarshal(raw, &rec) != nil {
			continue
		}
		n := routerResults(rec.Query)
		if n > 0 {
			os.Remove(fp)
			logf("RECOVERED (%d results, deleted): %q", n, rec.Query)
		} else {
			rec.Attempts++
			if rec.Attempts >= maxAttempts {
				os.MkdirAll(failedDir, 0o755)
				os.Rename(fp, filepath.Join(failedDir, e.Name()))
				logf("GIVE-UP after %d attempts (moved to failed/): %q", rec.Attempts, rec.Query)
			} else {
				b, _ := json.MarshalIndent(rec, "", "  ")
				os.WriteFile(fp, b, 0o644)
				logf("still failing (attempt %d/%d): %q", rec.Attempts, maxAttempts, rec.Query)
			}
		}
		time.Sleep(perReqPause)
	}
	if pending == 0 {
		logf("dead-letter queue empty — nothing to retry.")
	}
}
