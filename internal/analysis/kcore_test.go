package analysis

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

func TestComputeKCore_KnownStructure(t *testing.T) {
	// 4-clique + leaf attached to one of its members:
	//   a -- b
	//   |  / |
	//   | /  |
	//   c -- d
	//   |
	//   leaf
	// Every clique node has k-degree 3 (the 4-clique is a 3-core);
	// leaf has k-degree 1.
	g := graph.New()
	for _, id := range []string{"a", "b", "c", "d", "leaf"} {
		g.AddNode(&graph.Node{ID: id, Kind: graph.KindFunction, Name: id, FilePath: "x.go"})
	}
	for _, e := range [][2]string{
		{"a", "b"}, {"a", "c"}, {"a", "d"},
		{"b", "c"}, {"b", "d"},
		{"c", "d"}, {"c", "leaf"},
	} {
		g.AddEdge(&graph.Edge{From: e[0], To: e[1], Kind: graph.EdgeCalls, FilePath: "x.go"})
	}

	hits := ComputeKCore(g, KCoreOptions{})
	require.Len(t, hits, 5)
	byID := map[string]int{}
	for _, h := range hits {
		byID[h.NodeID] = h.KDegree
	}
	for _, id := range []string{"a", "b", "c", "d"} {
		assert.Equal(t, 3, byID[id],
			"4-clique members should have k-degree 3; got %v", byID)
	}
	assert.Equal(t, 1, byID["leaf"],
		"leaf should have k-degree 1; got %v", byID)
}

func TestComputeKCore_LineGraph(t *testing.T) {
	// 1 -- 2 -- 3 -- 4: every node has at most 2 neighbours,
	// and after peeling the two endpoints the remaining pair
	// drops below k=2, so k-degree is 1 across the board.
	g := graph.New()
	for _, id := range []string{"1", "2", "3", "4"} {
		g.AddNode(&graph.Node{ID: id, Kind: graph.KindFunction, Name: id, FilePath: "x.go"})
	}
	for _, e := range [][2]string{
		{"1", "2"}, {"2", "3"}, {"3", "4"},
	} {
		g.AddEdge(&graph.Edge{From: e[0], To: e[1], Kind: graph.EdgeCalls, FilePath: "x.go"})
	}
	hits := ComputeKCore(g, KCoreOptions{})
	for _, h := range hits {
		assert.Equal(t, 1, h.KDegree,
			"line graph nodes all have k-degree 1; got %v", hits)
	}
}

func TestComputeKCore_EmptyGraph(t *testing.T) {
	g := graph.New()
	hits := ComputeKCore(g, KCoreOptions{})
	assert.Empty(t, hits)
}

func TestComputeKCore_EdgeFilter(t *testing.T) {
	g := graph.New()
	for _, id := range []string{"a", "b", "c"} {
		g.AddNode(&graph.Node{ID: id, Kind: graph.KindFunction, Name: id, FilePath: "x.go"})
	}
	g.AddEdge(&graph.Edge{From: "a", To: "b", Kind: graph.EdgeCalls, FilePath: "x.go"})
	g.AddEdge(&graph.Edge{From: "b", To: "c", Kind: graph.EdgeReferences, FilePath: "x.go"})

	// Only call edges survive — a-b stays, b-c drops.
	hits := ComputeKCore(g, KCoreOptions{
		EdgeKinds: []graph.EdgeKind{graph.EdgeCalls},
	})
	byID := map[string]int{}
	for _, h := range hits {
		byID[h.NodeID] = h.KDegree
	}
	assert.Equal(t, 1, byID["a"])
	assert.Equal(t, 1, byID["b"])
	assert.Equal(t, 0, byID["c"], "c is isolated under the filter")
}
