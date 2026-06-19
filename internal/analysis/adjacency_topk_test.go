package analysis

import (
	"sort"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// chainSnapshot builds a single call chain seed -> n1 -> n2 -> ... so the
// seeded walk produces strictly decreasing, easy-to-order scores.
func chainSnapshot(t *testing.T, ids ...string) *AdjacencySnapshot {
	t.Helper()
	g := graph.New()
	for _, id := range ids {
		g.AddNode(&graph.Node{ID: id, Kind: graph.KindFunction, Name: id})
	}
	for i := 0; i+1 < len(ids); i++ {
		g.AddEdge(&graph.Edge{From: ids[i], To: ids[i+1], Kind: graph.EdgeCalls})
	}
	return BuildAdjacencySnapshot(g)
}

func TestPersonalizedPageRankTopK_KZeroIsDense(t *testing.T) {
	snap := chainSnapshot(t, "s", "a", "b", "c", "d")
	dense := snap.PersonalizedPageRank([]string{"s"}, 0)
	// k <= 0 must be byte-for-byte identical to the dense walk.
	if topk := snap.PersonalizedPageRankTopK([]string{"s"}, 0, 0); !sameScores(dense, topk) {
		t.Fatalf("k=0 should equal dense walk\n dense=%v\n topk =%v", dense, topk)
	}
	// k >= reachable count also returns the full dense map.
	if topk := snap.PersonalizedPageRankTopK([]string{"s"}, 0, 100); !sameScores(dense, topk) {
		t.Fatalf("k>=len should equal dense walk\n dense=%v\n topk =%v", dense, topk)
	}
}

func TestPersonalizedPageRankTopK_KeepsHighestScorers(t *testing.T) {
	snap := chainSnapshot(t, "s", "a", "b", "c", "d")
	dense := snap.PersonalizedPageRank([]string{"s"}, 0)
	if len(dense) <= 3 {
		t.Fatalf("test precondition: need >3 reachable nodes, got %d", len(dense))
	}

	// Expected top-3 IDs, computed from the dense walk.
	type kv struct {
		id string
		v  float64
	}
	pairs := make([]kv, 0, len(dense))
	for id, v := range dense {
		pairs = append(pairs, kv{id, v})
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].v > pairs[j].v })
	wantTop := map[string]bool{pairs[0].id: true, pairs[1].id: true, pairs[2].id: true}

	got := snap.PersonalizedPageRankTopK([]string{"s"}, 0, 3)
	if len(got) != 3 {
		t.Fatalf("expected exactly 3 entries, got %d: %v", len(got), got)
	}
	for id, v := range got {
		if !wantTop[id] {
			t.Errorf("top-3 retained an out-of-top-3 node %q", id)
		}
		// Truncation must not alter the surviving values.
		if v != dense[id] {
			t.Errorf("score for %q changed under truncation: dense=%f topk=%f", id, dense[id], v)
		}
	}
	// The max-scoring node (used for normalisation by the rerank consumer)
	// must always survive truncation.
	if _, ok := got[pairs[0].id]; !ok {
		t.Errorf("the max-scoring node %q must be retained", pairs[0].id)
	}
}

func TestPersonalizedPageRankTopK_NoSeedIsEmpty(t *testing.T) {
	snap := chainSnapshot(t, "s", "a")
	if got := snap.PersonalizedPageRankTopK([]string{"absent"}, 0, 4); len(got) != 0 {
		t.Errorf("absent seed should yield empty result, got %v", got)
	}
}

func sameScores(a, b map[string]float64) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}
