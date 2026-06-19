package mcp

import (
	"container/list"
	"strconv"
	"testing"
)

func TestPPRWalkCacheLRU(t *testing.T) {
	c := newPPRWalkCache()
	c.cap = 2

	c.put("a", map[string]float64{"x": 1})
	c.put("b", map[string]float64{"x": 2})
	if _, ok := c.get("a"); !ok {
		t.Fatal("a should be present")
	}
	// Touch a (now MRU), then insert c -> b is LRU and evicted.
	c.put("c", map[string]float64{"x": 3})
	if _, ok := c.get("b"); ok {
		t.Fatal("b should have been evicted (LRU)")
	}
	if _, ok := c.get("a"); !ok {
		t.Fatal("a should survive (was recently used)")
	}
	if _, ok := c.get("c"); !ok {
		t.Fatal("c should be present")
	}
}

func TestPPRWalkCacheStats(t *testing.T) {
	c := newPPRWalkCache()
	c.put("k", map[string]float64{"x": 1})
	if _, ok := c.get("k"); !ok {
		t.Fatal("hit expected")
	}
	if _, ok := c.get("miss"); ok {
		t.Fatal("miss expected")
	}
	hits, misses, size, capacity, enabled := c.stats()
	if hits != 1 || misses != 1 {
		t.Fatalf("hits=%d misses=%d, want 1/1", hits, misses)
	}
	if size != 1 || capacity != 512 || !enabled {
		t.Fatalf("size=%d cap=%d enabled=%v", size, capacity, enabled)
	}
}

func TestPPRWalkCacheDisabled(t *testing.T) {
	t.Setenv("GORTEX_PPR_CACHE_DISABLE", "1")
	c := newPPRWalkCache()
	if c.enabled {
		t.Fatal("cache should be disabled")
	}
	c.put("k", map[string]float64{"x": 1})
	if _, ok := c.get("k"); ok {
		t.Fatal("disabled cache should never hit")
	}
}

func TestPPRWalkCacheEmptyKeyAndScores(t *testing.T) {
	c := newPPRWalkCache()
	c.put("", map[string]float64{"x": 1}) // empty key ignored
	if _, ok := c.get(""); ok {
		t.Fatal("empty key should never store")
	}
	c.put("k", nil) // empty scores ignored
	if _, ok := c.get("k"); ok {
		t.Fatal("empty scores should not be stored")
	}
}

func TestPPRWalkCacheDefaults(t *testing.T) {
	// Construction without overriding env carries the memory-bounding
	// defaults that keep the cache from ballooning on a large graph.
	c := newPPRWalkCache()
	if c.maxBytes != pprCacheDefaultMaxBytes {
		t.Errorf("default maxBytes=%d, want %d", c.maxBytes, pprCacheDefaultMaxBytes)
	}
	if c.topK != pprCacheDefaultTopK {
		t.Errorf("default topK=%d, want %d", c.topK, pprCacheDefaultTopK)
	}
}

func TestPPRWalkCacheEnvOverrides(t *testing.T) {
	t.Setenv("GORTEX_PPR_CACHE_MAX_MB", "8")
	t.Setenv("GORTEX_PPR_CACHE_TOPK", "0") // 0 = unbounded is a valid override
	c := newPPRWalkCache()
	if c.maxBytes != 8<<20 {
		t.Errorf("maxBytes=%d, want %d", c.maxBytes, 8<<20)
	}
	if c.topK != 0 {
		t.Errorf("topK=%d, want 0", c.topK)
	}
}

// newTestPPRCache builds a cache with explicit bounds, bypassing the
// environment so the test is hermetic.
func newTestPPRCache(cap int, maxBytes int64) *pprWalkCache {
	return &pprWalkCache{
		ll:       list.New(),
		m:        make(map[string]*list.Element),
		cap:      cap,
		maxBytes: maxBytes,
		enabled:  true,
	}
}

func scoresOfSize(n int) map[string]float64 {
	m := make(map[string]float64, n)
	for i := range n {
		m["id"+strconv.Itoa(i)] = float64(i + 1)
	}
	return m
}

func TestPPRCache_ByteBudgetEvicts(t *testing.T) {
	// Each 10-score entry costs 10*pprCacheBytesPerScore. Budget for two;
	// the entry-count ceiling is high so the byte budget is what governs.
	per := int64(10) * pprCacheBytesPerScore
	c := newTestPPRCache(1000, 2*per)

	for i := range 5 {
		c.put("k"+strconv.Itoa(i), scoresOfSize(10))
	}

	if _, _, size, _, _ := c.stats(); size != 2 {
		t.Fatalf("byte budget should cap the cache at 2 entries, got %d", size)
	}
	if c.curBytes > c.maxBytes {
		t.Fatalf("curBytes %d exceeds budget %d", c.curBytes, c.maxBytes)
	}
	if _, ok := c.get("k4"); !ok {
		t.Errorf("most-recent key k4 should survive")
	}
	if _, ok := c.get("k0"); ok {
		t.Errorf("oldest key k0 should have been evicted")
	}
}

func TestPPRCache_ReputSameSizeKeepsByteAccounting(t *testing.T) {
	c := newTestPPRCache(1000, 1<<30)
	c.put("a", scoresOfSize(10))
	before := c.curBytes
	// Re-putting an existing key replaces its scores and re-accounts its
	// bytes in place — no double counting.
	c.put("a", scoresOfSize(10))
	if c.curBytes != before {
		t.Errorf("re-put of same-size entry changed curBytes: before=%d after=%d", before, c.curBytes)
	}
	if _, _, size, _, _ := c.stats(); size != 1 {
		t.Errorf("re-put must not add a second entry, size=%d", size)
	}
}
