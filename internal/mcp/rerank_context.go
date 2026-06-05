package mcp

import (
	"context"

	"github.com/zzet/gortex/internal/search/rerank"
)

// buildRerankContext assembles the per-request rerank.Context with
// every session-aware data source the server holds: locality, combo,
// frecency, feedback, and churn. Pure structural signals (BM25 rank,
// fan-in / fan-out, MinHash, signature match, recency, community) do
// not depend on session state and read from the graph directly via
// the Context.Graph pointer set by the pipeline call site.
//
// Returned Context is safe to reuse for the lifetime of the request
// but should not be cached across requests — the combo boost map is
// query-specific and the locality fields are session-specific.
func (s *Server) buildRerankContext(ctx context.Context, query string) *rerank.Context {
	repo, project := s.sessionLocality(ctx)
	rctx := &rerank.Context{
		Graph:      s.graph,
		RepoPrefix: repo,
		ProjectID:  project,
	}

	if cr := s.getCommunities(); cr != nil && cr.NodeToComm != nil {
		nodeToComm := cr.NodeToComm
		rctx.CommunityOf = func(id string) string { return nodeToComm[id] }
	}

	if s.combo != nil {
		// The combo boost fuses two stores: the exact whole-query
		// index (BoostMap) and the per-keyword association index
		// (KeywordBoostMap). They are max()-merged so a symbol picks
		// up the stronger of "this exact query led here before" and
		// "queries sharing these keywords led here before" -- the
		// exact-query boost is capped higher, so it dominates a
		// keyword-only boost whenever both fire. FeedbackSignal reads
		// the merged closure unchanged.
		boosts := s.combo.BoostMap(query)
		kwBoosts := s.combo.KeywordBoostMap(query)
		if len(boosts) > 0 || len(kwBoosts) > 0 {
			merged := make(map[string]float64, len(boosts)+len(kwBoosts))
			for id, v := range kwBoosts {
				merged[id] = v
			}
			for id, v := range boosts {
				if existing, ok := merged[id]; !ok || v > existing {
					merged[id] = v
				}
			}
			rctx.ComboBoostOf = func(id string) float64 {
				if v, ok := merged[id]; ok {
					return v
				}
				return 1.0
			}
		}
	}

	if s.frecency != nil && s.frecency.HasData() {
		ft := s.frecency
		rctx.FrecencyBoostOf = func(id string) float64 { return ft.BoostFor(id) }
	}

	if s.feedback != nil && s.feedback.HasData() {
		fb := s.feedback
		rctx.FeedbackOf = func(id string) float64 { return fb.GetSymbolScoreForQuery(id, query) }
	}

	if s.symHistory != nil {
		churn := s.churnCounts()
		if len(churn) > 0 {
			rctx.ChurnOf = func(id string) int { return churn[id] }
		}
	}

	// HITS authority / hub scores feed the authority rerank signal.
	// Both closures normalise against the graph maxima so the signal
	// receives values already in [0, 1].
	if h := s.getHITS(); h != nil {
		hits := h
		maxAuth, maxHub := hits.MaxAuth, hits.MaxHub
		rctx.AuthorityOf = func(id string) float64 {
			if maxAuth <= 0 {
				return 0
			}
			return hits.AuthorityOf(id) / maxAuth
		}
		rctx.HubOf = func(id string) float64 {
			if maxHub <= 0 {
				return 0
			}
			return hits.HubOf(id) / maxHub
		}
	}

	// Centrality: a per-query Random-Walk-with-Restart (Personalized
	// PageRank) over the call/reference graph, seeded from the query's
	// strongest candidate matches, feeds ProximitySignal — the graph-
	// centrality spine of retrieval. The walk runs against the
	// immutable adjacency snapshot built by RunAnalysis; nil until then
	// (the signal then sits at 0). The actual walk fires once per
	// Rerank inside Context.prepare, after the seeds are chosen.
	if snap := s.getAdjacency(); snap != nil {
		rctx.Centrality = func(seeds []string) map[string]float64 {
			return s.personalizedPageRank(snap, seeds)
		}
	}

	// Co-change feeds the rerank pipeline once the git-history mine
	// has run (lazily, on the first find_co_changing_symbols call, or
	// from an enriched snapshot). Until then the signal sits at 0.
	if s.hasCoChangeData() {
		rctx.CoChangeOf = s.coChangeScores
	}

	return rctx
}
