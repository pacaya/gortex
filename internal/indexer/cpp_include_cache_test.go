package indexer

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNewCppIncludeDirCache_EnvBudget(t *testing.T) {
	t.Setenv("GORTEX_RESOLVER_CACHE_MAX_MB", "7")
	assert.Equal(t, int64(7)<<20, newCppIncludeDirCache().maxBytes)

	t.Setenv("GORTEX_RESOLVER_CACHE_MAX_MB", "")
	assert.Equal(t, int64(0), newCppIncludeDirCache().maxBytes, "unset = unbounded")

	t.Setenv("GORTEX_RESOLVER_CACHE_MAX_MB", "0")
	assert.Equal(t, int64(0), newCppIncludeDirCache().maxBytes, "0 = unbounded")
}

// TestCppIncludeDirCache_Eviction pins that a tiny memory budget evicts the
// least-recently-used per-repo include-dir set.
func TestCppIncludeDirCache_Eviction(t *testing.T) {
	c := newCppIncludeDirCache()
	c.maxBytes = 200 // tiny budget; each entry below is ~89 bytes

	mk := func(f string) map[string]cppTU {
		return map[string]cppTU{f: {file: f, includeDirs: []string{"inc"}}}
	}
	c.put("repoA", mk("a.c"), 0)
	c.put("repoB", mk("b.c"), 0)
	c.put("repoC", mk("c.c"), 0)

	_, okA := c.get("repoA", 0)
	assert.False(t, okA, "least-recently-used repoA evicted under the budget")
	_, okB := c.get("repoB", 0)
	assert.True(t, okB, "repoB retained")
	_, okC := c.get("repoC", 0)
	assert.True(t, okC, "most-recently-used repoC retained")
}

// TestCppIncludeDirCache_GetPromotes pins that a get refreshes recency so the
// promoted entry survives a later eviction.
func TestCppIncludeDirCache_GetPromotes(t *testing.T) {
	c := newCppIncludeDirCache()
	c.maxBytes = 200

	mk := func(f string) map[string]cppTU {
		return map[string]cppTU{f: {file: f, includeDirs: []string{"inc"}}}
	}
	c.put("repoA", mk("a.c"), 0)
	c.put("repoB", mk("b.c"), 0)
	c.get("repoA", 0)            // promote A ahead of B
	c.put("repoC", mk("c.c"), 0) // evicts the now-LRU repoB

	_, okA := c.get("repoA", 0)
	assert.True(t, okA, "promoted repoA survives")
	_, okB := c.get("repoB", 0)
	assert.False(t, okB, "repoB evicted as least-recently-used")
}

// TestCppIncludeDirCache_UnboundedKeepsAll pins that the default (no budget)
// retains every entry.
func TestCppIncludeDirCache_UnboundedKeepsAll(t *testing.T) {
	c := newCppIncludeDirCache() // maxBytes 0
	for _, r := range []string{"r1", "r2", "r3"} {
		c.put(r, map[string]cppTU{r: {file: r}}, 0)
	}
	for _, r := range []string{"r1", "r2", "r3"} {
		_, ok := c.get(r, 0)
		assert.True(t, ok, "unbounded cache retains %s", r)
	}
}

// TestCppIncludeDirCache_MtimeStaleMiss pins the freshness check: a get whose
// disk modtime exceeds the entry's recorded mtime is a miss, while an equal or
// older modtime is a hit.
func TestCppIncludeDirCache_MtimeStaleMiss(t *testing.T) {
	c := newCppIncludeDirCache()
	c.put("repo", map[string]cppTU{"a.c": {file: "a.c"}}, 100)

	_, ok := c.get("repo", 100)
	assert.True(t, ok, "equal modtime is a hit")
	_, ok = c.get("repo", 50)
	assert.True(t, ok, "older modtime is a hit")
	_, ok = c.get("repo", 200)
	assert.False(t, ok, "newer modtime is a stale miss")
}
