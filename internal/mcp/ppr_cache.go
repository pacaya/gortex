package mcp

import (
	"container/list"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// pprWalkCache is a bounded LRU of seeded random-walk (Personalized
// PageRank) results, keyed by the content-addressed walk key derived
// from sorted seeds + restart + per-package Merkle roots (see
// analysis.AdjacencySnapshot.WalkCacheKey).
//
// It is the incremental-RWR cache: because the key embeds the per-
// package content roots, invalidation is implicit. When a package the
// walk depends on changes, the next analysis pass produces a different
// root → a different key → a miss → recompute; unchanged-package walks
// reproduce the same key and hit, even across a snapshot rebuild or a
// daemon restart of the in-memory graph. Stale entries for changed
// packages become unreachable and age out via LRU eviction.
//
// Cached score maps are treated as read-only by every consumer (the
// rerank pipeline rescales into a fresh map; context_closure only reads
// values), so sharing one map across calls is safe without copying.
type pprWalkCache struct {
	mu       sync.Mutex
	ll       *list.List // front = most-recently-used
	m        map[string]*list.Element
	cap      int   // max distinct walks retained (secondary ceiling)
	maxBytes int64 // memory budget across all retained score maps
	curBytes int64 // running sum of entry.bytes
	topK     int   // nodes kept per cached walk (0 = unbounded)
	enabled  bool

	hits   atomic.Int64
	misses atomic.Int64
}

type pprCacheEntry struct {
	key    string
	scores map[string]float64
	bytes  int64 // estimated retained size, for the cache's byte budget
}

const (
	// pprCacheDefaultMaxBytes bounds the total memory the walk cache may
	// retain across all cached score maps. An entry-count ceiling alone is
	// unsafe: each entry is a per-walk score map whose size scales with the
	// graph, so on a large graph 512 entries can reach several GB.
	pprCacheDefaultMaxBytes = 256 << 20 // 256 MiB

	// pprCacheDefaultTopK caps how many of the highest-scoring nodes each
	// cached walk retains. A seeded walk concentrates its mass near the
	// seeds, so a few thousand nodes hold all the ranking signal every
	// consumer reads; the tail is dropped before caching. 0 disables the
	// cap (full dense map).
	pprCacheDefaultTopK = 4096

	// pprCacheBytesPerScore estimates the retained bytes of one
	// map[string]float64 entry: a 16-byte string header + 8-byte value +
	// Go map bucket / load-factor overhead. The key's backing bytes are
	// shared with the graph and not counted. Deliberately conservative.
	pprCacheBytesPerScore = 48
)

// pprEntryBytes estimates the retained size of a cached score map for the
// cache's byte accounting.
func pprEntryBytes(scores map[string]float64) int64 {
	return int64(len(scores)) * pprCacheBytesPerScore
}

// newPPRWalkCache constructs the cache from the environment:
//   - GORTEX_PPR_CACHE_DISABLE=1   turn the cache off (always recompute)
//   - GORTEX_PPR_CACHE_SIZE=<n>    max distinct walks retained (default 512)
//   - GORTEX_PPR_CACHE_MAX_MB=<n>  total memory budget in MiB (default 256)
//   - GORTEX_PPR_CACHE_TOPK=<n>    nodes kept per walk, 0=unbounded (default 4096)
func newPPRWalkCache() *pprWalkCache {
	c := &pprWalkCache{
		ll:       list.New(),
		m:        make(map[string]*list.Element),
		cap:      512,
		maxBytes: pprCacheDefaultMaxBytes,
		topK:     pprCacheDefaultTopK,
		enabled:  true,
	}
	if isTruthyEnv(os.Getenv("GORTEX_PPR_CACHE_DISABLE")) {
		c.enabled = false
	}
	if v := strings.TrimSpace(os.Getenv("GORTEX_PPR_CACHE_SIZE")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c.cap = n
		}
	}
	if v := strings.TrimSpace(os.Getenv("GORTEX_PPR_CACHE_MAX_MB")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c.maxBytes = int64(n) << 20
		}
	}
	if v := strings.TrimSpace(os.Getenv("GORTEX_PPR_CACHE_TOPK")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			c.topK = n
		}
	}
	return c
}

// get returns the cached scores for key, promoting it to most-recently-
// used. The second return is false on a miss.
func (c *pprWalkCache) get(key string) (map[string]float64, bool) {
	if c == nil || !c.enabled || key == "" {
		return nil, false
	}
	c.mu.Lock()
	el, ok := c.m[key]
	if ok {
		c.ll.MoveToFront(el)
	}
	c.mu.Unlock()
	if !ok {
		c.misses.Add(1)
		return nil, false
	}
	c.hits.Add(1)
	return el.Value.(*pprCacheEntry).scores, true
}

// put stores scores under key, evicting least-recently-used entries until
// the cache is within both its memory budget and its entry-count ceiling.
func (c *pprWalkCache) put(key string, scores map[string]float64) {
	if c == nil || !c.enabled || key == "" || len(scores) == 0 {
		return
	}
	sz := pprEntryBytes(scores)
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.m[key]; ok {
		e := el.Value.(*pprCacheEntry)
		c.curBytes += sz - e.bytes
		e.scores = scores
		e.bytes = sz
		c.ll.MoveToFront(el)
		c.evictLocked()
		return
	}
	el := c.ll.PushFront(&pprCacheEntry{key: key, scores: scores, bytes: sz})
	c.m[key] = el
	c.curBytes += sz
	c.evictLocked()
}

// evictLocked drops least-recently-used entries until the cache satisfies
// both its byte budget and its entry-count ceiling. The caller holds c.mu.
func (c *pprWalkCache) evictLocked() {
	for c.ll.Len() > 0 && (c.curBytes > c.maxBytes || c.ll.Len() > c.cap) {
		back := c.ll.Back()
		if back == nil {
			break
		}
		e := back.Value.(*pprCacheEntry)
		c.ll.Remove(back)
		delete(c.m, e.key)
		c.curBytes -= e.bytes
	}
}

// stats returns a snapshot of cache performance for diagnostics.
func (c *pprWalkCache) stats() (hits, misses int64, size, capacity int, enabled bool) {
	if c == nil {
		return 0, 0, 0, 0, false
	}
	c.mu.Lock()
	size = c.ll.Len()
	c.mu.Unlock()
	return c.hits.Load(), c.misses.Load(), size, c.cap, c.enabled
}
