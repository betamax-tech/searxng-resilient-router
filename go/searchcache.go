// searchcache.go — disk cache for the tiered search router. Identical
// (normalized query, tier, params) requests within 24h are served from
// disk, capped in total size (oldest entries evicted first).
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const cacheTTL = 24 * time.Hour
const cacheMaxBytes = int64(256 * 1024 * 1024) // 256MB disk cap

// DiskCache stores one JSON file per cache key under dir.
type DiskCache struct {
	dir      string
	ttl      time.Duration
	maxBytes int64
	mu       sync.Mutex
	now      func() time.Time // injectable for tests
}

func NewDiskCache(dir string, ttl time.Duration, maxBytes int64) *DiskCache {
	_ = os.MkdirAll(dir, 0o755)
	return &DiskCache{dir: dir, ttl: ttl, maxBytes: maxBytes, now: time.Now}
}

type cacheFile struct {
	StoredAt int64           `json:"stored_at"` // unix seconds
	Body     json.RawMessage `json:"body"`
}

// buildCacheKey normalizes (query, tier, a fixed small set of params that
// affect result content) into a stable cache key. Router-only meta params
// (strict, direct, tier itself) are deliberately excluded — they are
// consumed before this is ever called.
func buildCacheKey(tier string, params url.Values) string {
	q := strings.ToLower(strings.TrimSpace(params.Get("q")))
	parts := []string{"tier=" + tier, "q=" + q}
	for _, k := range []string{"categories", "language", "safesearch", "pageno"} {
		if v := params.Get(k); v != "" {
			parts = append(parts, k+"="+strings.ToLower(v))
		}
	}
	sort.Strings(parts)
	h := sha256.Sum256([]byte(strings.Join(parts, "&")))
	return hex.EncodeToString(h[:])
}

func (c *DiskCache) path(key string) string {
	return filepath.Join(c.dir, key+".json")
}

// Get returns the cached body and true on a fresh (<ttl) hit. Expired
// entries are removed and reported as a miss.
func (c *DiskCache) Get(key string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	b, err := os.ReadFile(c.path(key))
	if err != nil {
		return nil, false
	}
	var cf cacheFile
	if json.Unmarshal(b, &cf) != nil {
		return nil, false
	}
	if c.now().Unix()-cf.StoredAt > int64(c.ttl.Seconds()) {
		_ = os.Remove(c.path(key))
		return nil, false
	}
	return []byte(cf.Body), true
}

// Set writes body under key (atomic: temp file + rename), then enforces the
// total size cap by evicting the oldest-modified files first.
func (c *DiskCache) Set(key string, body []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cf := cacheFile{StoredAt: c.now().Unix(), Body: json.RawMessage(body)}
	b, err := json.Marshal(cf)
	if err != nil {
		return
	}
	tmp := c.path(key) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, c.path(key))
	c.enforceSizeCapLocked()
}

// enforceSizeCapLocked evicts the oldest entries until the directory is
// under maxBytes. "Oldest" is read from each entry's own stored_at content
// field (set by Set() via c.now()), NOT the filesystem mtime — this makes
// eviction order deterministic under the injectable clock used by tests,
// and immune to filesystem mtime-resolution ties on rapid successive writes.
func (c *DiskCache) enforceSizeCapLocked() {
	if c.maxBytes <= 0 {
		return
	}
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		return
	}
	type fi struct {
		path     string
		size     int64
		storedAt int64
	}
	var files []fi
	var total int64
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		p := filepath.Join(c.dir, e.Name())
		info, err := e.Info()
		if err != nil {
			continue
		}
		storedAt := info.ModTime().Unix() // fallback if content can't be read
		if b, err := os.ReadFile(p); err == nil {
			var cf cacheFile
			if json.Unmarshal(b, &cf) == nil {
				storedAt = cf.StoredAt
			}
		}
		files = append(files, fi{p, info.Size(), storedAt})
		total += info.Size()
	}
	if total <= c.maxBytes {
		return
	}
	sort.Slice(files, func(i, j int) bool { return files[i].storedAt < files[j].storedAt })
	for _, f := range files {
		if total <= c.maxBytes {
			break
		}
		if os.Remove(f.path) == nil {
			total -= f.size
		}
	}
}
