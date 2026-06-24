package indexer

import (
	"container/list"
	"os"
	"strconv"
	"strings"
	"sync"
)

// cppIncludeDirCacheT is a memory-bounded LRU over per-repo-root compile-DB
// include-dir sets. Unlike the resolver's per-pass maps (cleared between passes
// and intentionally uncapped — bounded by the pass work set), this cache is
// long-lived: one entry per tracked repo root, surviving across reindexes. It
// is bounded by GORTEX_RESOLVER_CACHE_MAX_MB; a zero / unset budget (the
// default) leaves it unbounded, preserving the prior plain-map behavior.
type cppIncludeDirCacheT struct {
	mu       sync.Mutex
	ll       *list.List // front = most-recently-used
	m        map[string]*list.Element
	maxBytes int64 // 0 = unbounded
	curBytes int64
}

type cppIncludeCacheEntry struct {
	key   string
	tus   map[string]cppTU
	bytes int64
	// mtime is the newest modtime (unix nanoseconds) across the compile_commands.json
	// files this entry was built from. A lookup whose on-disk files are newer than
	// this is treated as a miss, so an isolated edit to compile_commands.json is
	// picked up on the next load without a full reindex.
	mtime int64
}

// newCppIncludeDirCache builds the cache, reading GORTEX_RESOLVER_CACHE_MAX_MB
// (MiB; <= 0 / unset = unbounded) for its memory budget.
func newCppIncludeDirCache() *cppIncludeDirCacheT {
	c := &cppIncludeDirCacheT{ll: list.New(), m: map[string]*list.Element{}}
	if v := strings.TrimSpace(os.Getenv("GORTEX_RESOLVER_CACHE_MAX_MB")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c.maxBytes = int64(n) << 20
		}
	}
	return c
}

// get returns the cached include-dir set for root, promoting it to
// most-recently-used. diskMtime is the newest modtime currently on disk across
// the compile_commands.json files for root; an entry whose recorded mtime is
// older is treated as stale (a miss) so an edited compile_commands.json is
// re-read on the next load without a full reindex. The second return is false on
// a miss.
func (c *cppIncludeDirCacheT) get(root string, diskMtime int64) (map[string]cppTU, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.m[root]
	if !ok {
		return nil, false
	}
	e := el.Value.(*cppIncludeCacheEntry)
	if diskMtime > e.mtime {
		return nil, false
	}
	c.ll.MoveToFront(el)
	return e.tus, true
}

// put stores the include-dir set for root along with the modtime it was built
// from, evicting least-recently-used entries until within the memory budget (at
// least the just-stored entry is retained).
func (c *cppIncludeDirCacheT) put(root string, tus map[string]cppTU, mtime int64) {
	sz := cppTUMapBytes(tus)
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.m[root]; ok {
		e := el.Value.(*cppIncludeCacheEntry)
		c.curBytes += sz - e.bytes
		e.tus = tus
		e.bytes = sz
		e.mtime = mtime
		c.ll.MoveToFront(el)
		c.evictLocked()
		return
	}
	el := c.ll.PushFront(&cppIncludeCacheEntry{key: root, tus: tus, bytes: sz, mtime: mtime})
	c.m[root] = el
	c.curBytes += sz
	c.evictLocked()
}

// clear drops the cached include-dir set for root so the next load re-reads
// compile_commands.json.
func (c *cppIncludeDirCacheT) clear(root string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.m[root]; ok {
		c.ll.Remove(el)
		delete(c.m, root)
		c.curBytes -= el.Value.(*cppIncludeCacheEntry).bytes
	}
}

// evictLocked drops least-recently-used entries until within the byte budget,
// always retaining the most-recently-used entry (so a freshly-loaded set stays
// available to its caller). A zero budget disables eviction. Caller holds c.mu.
func (c *cppIncludeDirCacheT) evictLocked() {
	if c.maxBytes <= 0 {
		return
	}
	for c.curBytes > c.maxBytes && c.ll.Len() > 1 {
		back := c.ll.Back()
		e := back.Value.(*cppIncludeCacheEntry)
		c.ll.Remove(back)
		delete(c.m, e.key)
		c.curBytes -= e.bytes
	}
}

// cppTUMapBytes estimates the retained size of one repo's include-dir set for
// the cache's byte accounting (string content plus per-entry map overhead).
func cppTUMapBytes(tus map[string]cppTU) int64 {
	var n int64
	for k, tu := range tus {
		n += int64(len(k)) + int64(len(tu.file)) + 64
		for _, d := range tu.includeDirs {
			n += int64(len(d)) + 16
		}
	}
	return n
}
