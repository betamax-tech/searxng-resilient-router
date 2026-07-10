//go:build ignore

// proxypool.go — incremental, self-healing rotating proxy pool for the proxied
// SearXNG instance. Build: go build -o proxypool proxypool.go
//
// Design (incremental, NOT full-scan):
//   1. Re-verify ONLY the existing active pool (cheap, ~20 checks).
//   2. Drop failures.
//   3. If below target, refill from a cached candidate backlog in SMALL BATCHES
//      until full; only fetch public source lists when the backlog runs dry.
//   4. Write the healthy pool into settings.proxied.yml's managed block, where
//      SearXNG round-robins across them.
//
// Health check = real request to Google (our only working engine from this
// host), so a proxy only counts if it can actually serve the search use case.
//
// No paid services, no API keys — only public/open-source lists.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	target        = 20
	batch         = 60
	maxRefill     = 8
	checkURL      = "https://www.google.com/"
	checkTimeout  = 8 * time.Second
	checkWorkers  = 80
	fetchTimeout  = 20 * time.Second
	settingsPath  = "/home/cmark/server/searxng-rotation/searxng/settings.proxied.yml"
	beginMarker   = "# >>> MANAGED PROXIES (proxy_pool) — do not edit by hand >>>"
	endMarker     = "# <<< MANAGED PROXIES (proxy_pool) <<<"
)

var (
	cacheDir  = "/home/cmark/server/searxng-rotation/cache"
	activeFile = filepath.Join(cacheDir, "active_pool.json")
	candFile   = filepath.Join(cacheDir, "candidates.json")

	hostPortRE = regexp.MustCompile(`^(?:(https?|socks5h?)://)?(\d{1,3}(?:\.\d{1,3}){3}):(\d{2,5})$`)

	sources = []struct{ hint, url string }{
		{"http", "https://raw.githubusercontent.com/TheSpeedX/PROXY-List/master/http.txt"},
		{"http", "https://raw.githubusercontent.com/jetkai/proxy-list/main/online-proxies/txt/proxies-http.txt"},
		{"auto", "https://raw.githubusercontent.com/proxifly/free-proxy-list/main/proxies/protocols/http/data.txt"},
		{"http", "https://raw.githubusercontent.com/monosans/proxy-list/main/proxies/http.txt"},
		{"http", "https://raw.githubusercontent.com/clarketm/proxy-list/master/proxy-list-raw.txt"},
		{"socks5", "https://raw.githubusercontent.com/hookzof/socks5_list/master/proxy.txt"},
		{"geonode", "https://proxylist.geonode.com/api/proxy-list?limit=100&page=1&sort_by=lastChecked&sort_type=desc&protocols=http%2Chttps"},
	}
)

func logf(f string, a ...any) {
	fmt.Printf("["+time.Now().UTC().Format(time.RFC3339)+"] "+f+"\n", a...)
}

type proxyEntry struct {
	URL       string `json:"url"`
	LatencyMS int64  `json:"latency_ms"`
}

func toURL(proto, host string, port int) string {
	scheme := "http"
	switch proto {
	case "socks5", "socks5h":
		scheme = "socks5h"
	case "https":
		scheme = "http"
	}
	return fmt.Sprintf("%s://%s:%d", scheme, host, port)
}

func fetch(u string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	req.Header.Set("User-Agent", "searxng-proxy-pool/1.0")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return string(b), err
}

func fetchSources() []string {
	seen := map[string]bool{}
	var out []string
	add := func(u string) {
		if !seen[u] {
			seen[u] = true
			out = append(out, u)
		}
	}
	for _, s := range sources {
		body, err := fetch(s.url)
		if err != nil {
			logf("  source FAILED (%s): %v", s.url, err)
			continue
		}
		n0 := len(out)
		if s.hint == "geonode" {
			var doc struct {
				Data []struct {
					IP        string   `json:"ip"`
					Port      string   `json:"port"`
					Protocols []string `json:"protocols"`
				} `json:"data"`
			}
			if json.Unmarshal([]byte(body), &doc) == nil {
				for _, r := range doc.Data {
					p, _ := strconv.Atoi(r.Port)
					proto := "http"
					if len(r.Protocols) > 0 {
						proto = r.Protocols[0]
					}
					if r.IP != "" && p > 0 {
						add(toURL(proto, r.IP, p))
					}
				}
			}
		} else {
			sc := bufio.NewScanner(strings.NewReader(body))
			for sc.Scan() {
				line := strings.TrimSpace(sc.Text())
				if line == "" || strings.HasPrefix(line, "#") {
					continue
				}
				m := hostPortRE.FindStringSubmatch(line)
				if m == nil {
					continue
				}
				proto := m[1]
				if proto == "" {
					if s.hint != "auto" {
						proto = s.hint
					} else {
						proto = "http"
					}
				}
				p, _ := strconv.Atoi(m[3])
				add(toURL(proto, m[2], p))
			}
		}
		logf("  +%5d from %s", len(out)-n0, s.url)
	}
	return out
}

func check(purl string) (int64, bool) {
	pu, err := url.Parse(purl)
	if err != nil {
		return 0, false
	}
	tr := &http.Transport{Proxy: http.ProxyURL(pu)}
	client := &http.Client{Transport: tr, Timeout: checkTimeout}
	req, _ := http.NewRequest(http.MethodGet, checkURL, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; searxng-proxy-pool/1.0)")
	t0 := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return 0, false
	}
	defer resp.Body.Close()
	io.CopyN(io.Discard, resp.Body, 2048)
	if resp.StatusCode == 200 {
		return time.Since(t0).Milliseconds(), true
	}
	return 0, false
}

func checkMany(urls []string) map[string]int64 {
	out := map[string]int64{}
	var mu sync.Mutex
	sem := make(chan struct{}, checkWorkers)
	var wg sync.WaitGroup
	for _, u := range urls {
		wg.Add(1)
		sem <- struct{}{}
		go func(u string) {
			defer wg.Done()
			defer func() { <-sem }()
			if ms, ok := check(u); ok {
				mu.Lock()
				out[u] = ms
				mu.Unlock()
			}
		}(u)
	}
	wg.Wait()
	return out
}

func loadActive() []proxyEntry {
	b, err := os.ReadFile(activeFile)
	if err != nil {
		return nil
	}
	var doc struct {
		Proxies []proxyEntry `json:"proxies"`
	}
	json.Unmarshal(b, &doc)
	return doc.Proxies
}

func loadCandidates() []string {
	b, err := os.ReadFile(candFile)
	if err != nil {
		return nil
	}
	var c []string
	json.Unmarshal(b, &c)
	return c
}

func saveJSON(path string, v any) {
	b, _ := json.MarshalIndent(v, "", "  ")
	os.WriteFile(path, b, 0o644)
}

func main() {
	stamp := time.Now().UTC().Format(time.RFC3339)

	// 1. Re-verify active pool only.
	active := loadActive()
	var activeURLs []string
	for _, p := range active {
		activeURLs = append(activeURLs, p.URL)
	}
	logf("Re-verifying %d active proxies…", len(activeURLs))
	pool := checkMany(activeURLs)
	logf("  survived: %d/%d", len(pool), len(activeURLs))

	// 2. Refill from backlog if below target.
	if len(pool) < target {
		cands := loadCandidates()
		if len(cands) == 0 {
			logf("Candidate backlog empty — fetching public source lists…")
			cands = fetchSources()
			logf("Fetched %d candidates.", len(cands))
		}
		idx, batches := 0, 0
		for len(pool) < target && idx < len(cands) && batches < maxRefill {
			end := idx + batch
			if end > len(cands) {
				end = len(cands)
			}
			var b []string
			for _, c := range cands[idx:end] {
				if _, ok := pool[c]; !ok {
					b = append(b, c)
				}
			}
			idx = end
			batches++
			found := checkMany(b)
			for u, ms := range found {
				pool[u] = ms
			}
			logf("  refill batch %d: tested %d, +%d healthy (pool now %d/%d)", batches, len(b), len(found), len(pool), target)
		}
		remaining := cands[idx:]
		saveJSON(candFile, remaining)
		logf("  candidate backlog remaining: %d", len(remaining))
	} else {
		logf("  pool at/above target (%d/%d); no refill.", len(pool), target)
	}

	// 3. Rank + cap + persist.
	type kv struct {
		u  string
		ms int64
	}
	var ranked []kv
	for u, ms := range pool {
		ranked = append(ranked, kv{u, ms})
	}
	sort.Slice(ranked, func(i, j int) bool { return ranked[i].ms < ranked[j].ms })
	if len(ranked) > target {
		ranked = ranked[:target]
	}
	var entries []proxyEntry
	var urls []string
	for _, k := range ranked {
		entries = append(entries, proxyEntry{URL: k.u, LatencyMS: k.ms})
		urls = append(urls, k.u)
	}
	saveJSON(activeFile, map[string]any{"generated": stamp, "count": len(entries), "proxies": entries})
	logf("Active pool: %d proxies", len(entries))
	for i, k := range ranked {
		if i >= 10 {
			break
		}
		logf("    %6dms  %s", k.ms, k.u)
	}

	// 4. Write settings.
	writeSettings(urls, stamp)
	if len(urls) == 0 {
		os.Exit(2)
	}
}

func writeSettings(proxies []string, stamp string) {
	raw, err := os.ReadFile(settingsPath)
	if err != nil {
		logf("cannot read settings: %v", err)
		os.Exit(1)
	}
	text := string(raw)
	var b strings.Builder
	b.WriteString(beginMarker + "\n")
	desc := "direct connection"
	if len(proxies) > 0 {
		desc = "SearXNG round-robins these"
	}
	fmt.Fprintf(&b, "# regenerated: %s | %d healthy proxies | %s\n", stamp, len(proxies), desc)
	b.WriteString("outgoing:\n")
	b.WriteString("  request_timeout: 6.0\n")
	b.WriteString("  max_request_timeout: 15.0\n")
	b.WriteString("  retries: 2\n")
	b.WriteString("  pool_connections: 100\n")
	b.WriteString("  pool_maxsize: 20\n")
	if len(proxies) > 0 {
		b.WriteString("  extra_proxy_timeout: 10\n")
		b.WriteString("  proxies:\n")
		b.WriteString("    all://:\n")
		for _, p := range proxies {
			fmt.Fprintf(&b, "      - %s\n", p)
		}
	}
	b.WriteString(endMarker)
	block := b.String()

	re := regexp.MustCompile(`(?s)` + regexp.QuoteMeta(beginMarker) + `.*?` + regexp.QuoteMeta(endMarker))
	if re.MatchString(text) {
		text = re.ReplaceAllString(text, block)
	} else {
		text = strings.TrimRight(text, "\n") + "\n\n" + block + "\n"
	}
	os.WriteFile(settingsPath, []byte(text), 0o644)
	logf("settings.proxied.yml managed proxy block updated (%d proxies).", len(proxies))
}
