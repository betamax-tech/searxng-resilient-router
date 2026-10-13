package main

import (
	"net/url"
	"testing"
	"time"
)

func TestDiskCache_SetThenGetWithinTTL(t *testing.T) {
	c := NewDiskCache(t.TempDir(), 24*time.Hour, 0)
	fixed := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return fixed }

	key := "abc123"
	c.Set(key, []byte(`{"results":[]}`))
	body, ok := c.Get(key)
	if !ok {
		t.Fatalf("expected cache hit")
	}
	if string(body) != `{"results":[]}` {
		t.Errorf("got body %s", body)
	}
}

func TestDiskCache_MissAfterTTLExpires(t *testing.T) {
	c := NewDiskCache(t.TempDir(), 24*time.Hour, 0)
	t0 := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return t0 }
	c.Set("k", []byte(`{"a":1}`))

	// 25 hours later -> expired.
	c.now = func() time.Time { return t0.Add(25 * time.Hour) }
	if _, ok := c.Get("k"); ok {
		t.Errorf("expected cache MISS after TTL expiry")
	}
}

func TestDiskCache_MissForUnknownKey(t *testing.T) {
	c := NewDiskCache(t.TempDir(), 24*time.Hour, 0)
	if _, ok := c.Get("never-set"); ok {
		t.Errorf("expected miss for a key that was never set")
	}
}

func TestBuildCacheKey_SameQueryTierParamsIsStable(t *testing.T) {
	p1 := url.Values{"q": {"Misconduct"}, "categories": {"general"}}
	p2 := url.Values{"q": {"misconduct"}, "categories": {"general"}} // case-insensitive query
	k1 := buildCacheKey("standard", p1)
	k2 := buildCacheKey("standard", p2)
	if k1 != k2 {
		t.Errorf("expected identical normalized cache keys, got %s vs %s", k1, k2)
	}
}

func TestBuildCacheKey_DifferentTierDifferentKey(t *testing.T) {
	p := url.Values{"q": {"misconduct"}}
	k1 := buildCacheKey("standard", p)
	k2 := buildCacheKey("crucial", p)
	if k1 == k2 {
		t.Errorf("expected different tiers to produce different cache keys")
	}
}

func TestBuildCacheKey_DifferentQueryDifferentKey(t *testing.T) {
	k1 := buildCacheKey("standard", url.Values{"q": {"misconduct"}})
	k2 := buildCacheKey("standard", url.Values{"q": {"malpractice"}})
	if k1 == k2 {
		t.Errorf("expected different queries to produce different cache keys")
	}
}

func TestDiskCache_SizeCapEvictsOldestFirst(t *testing.T) {
	dir := t.TempDir()
	c := NewDiskCache(dir, 24*time.Hour, 90) // fits ~2 of the 39-byte entries below, not 3
	base := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)

	c.now = func() time.Time { return base }
	c.Set("oldest", []byte(`{"x":1}`))
	c.now = func() time.Time { return base.Add(1 * time.Second) }
	c.Set("middle", []byte(`{"x":2}`))
	c.now = func() time.Time { return base.Add(2 * time.Second) }
	c.Set("newest", []byte(`{"x":3}`))

	if _, ok := c.Get("newest"); !ok {
		t.Errorf("expected the newest entry to survive eviction")
	}
	if _, ok := c.Get("oldest"); ok {
		t.Errorf("expected the oldest entry to have been evicted under the size cap")
	}
}
