package mcp

import (
	"github.com/zzet/gortex/internal/analysis"
)

// personalizedPageRank runs a Random-Walk-with-Restart (Personalized
// PageRank) from the given seed node IDs over the adjacency snapshot
// and returns each reachable node's proximity score. It is the seam the
// rerank pipeline's ProximitySignal (and context_closure's proximity
// mode) reach centrality through.
//
// Walks flow through a Merkle-keyed cache (see ppr_cache.go) so repeated
// walks on an unchanged graph — or on packages that did not change
// between snapshots — return instantly instead of re-iterating the whole
// CSR. The cache is bypassed when disabled (GORTEX_PPR_CACHE_DISABLE) or
// when the snapshot has no package roots.
func (s *Server) personalizedPageRank(snap *analysis.AdjacencySnapshot, seeds []string) map[string]float64 {
	if snap == nil || len(seeds) == 0 {
		return nil
	}
	cache := s.pprCache
	// topK caps the walk to its highest-scoring nodes before it is cached,
	// so a single retained entry stays a few hundred KB instead of a full-
	// graph-sized map. Applied on both paths so a cached and an uncached
	// walk return the same result.
	topK := pprCacheDefaultTopK
	if cache != nil {
		topK = cache.topK
	}
	if cache == nil || !cache.enabled {
		return snap.PersonalizedPageRankTopK(seeds, 0, topK)
	}
	// Merkle-keyed walk cache: the key embeds the per-package content
	// roots the walk depends on, so an unchanged walk hits even across
	// a snapshot rebuild, and only a walk touching a changed package
	// recomputes. An empty key (no package roots, or no seed resolves)
	// falls through to an uncached walk.
	key := snap.WalkCacheKey(seeds, 0)
	if key != "" {
		if scores, ok := cache.get(key); ok {
			return scores
		}
	}
	scores := snap.PersonalizedPageRankTopK(seeds, 0, topK)
	cache.put(key, scores)
	return scores
}
