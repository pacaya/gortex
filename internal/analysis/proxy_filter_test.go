package analysis

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// TestCentrality_ExcludesProxyNodes asserts federation proxy nodes (and
// their edges) never enter PageRank / HITS / Betweenness: a proxy node
// gets no score, and the real nodes' scores are identical to a graph that
// never had the proxy.
func TestCentrality_ExcludesProxyNodes(t *testing.T) {
	build := func(withProxy bool) *graph.Graph {
		g := graph.New()
		for _, id := range []string{"r/a.go::A", "r/b.go::B", "r/c.go::C"} {
			g.AddNode(&graph.Node{ID: id, Kind: graph.KindFunction, Name: id})
		}
		g.AddEdge(&graph.Edge{From: "r/a.go::A", To: "r/b.go::B", Kind: graph.EdgeCalls})
		g.AddEdge(&graph.Edge{From: "r/b.go::B", To: "r/c.go::C", Kind: graph.EdgeCalls})
		g.AddEdge(&graph.Edge{From: "r/c.go::C", To: "r/a.go::A", Kind: graph.EdgeReferences})
		if withProxy {
			pid := graph.ProxyNodeID("remoteX", "x/z.go::Z")
			g.AddNode(&graph.Node{ID: pid, Kind: graph.KindFunction, Name: "Z", Origin: "remote:remoteX", Stub: true})
			// Edges between a real node and the proxy must not distort A.
			g.AddEdge(&graph.Edge{From: "r/a.go::A", To: pid, Kind: graph.EdgeCalls})
			g.AddEdge(&graph.Edge{From: pid, To: "r/b.go::B", Kind: graph.EdgeCalls})
		}
		return g
	}

	clean := build(false)
	withProxy := build(true)
	proxyID := graph.ProxyNodeID("remoteX", "x/z.go::Z")

	approxEq := func(a, b float64) bool {
		d := a - b
		return d < 1e-9 && d > -1e-9
	}

	t.Run("pagerank", func(t *testing.T) {
		pr := ComputePageRank(withProxy)
		if _, ok := pr.Scores[proxyID]; ok {
			t.Error("proxy node must not receive a PageRank score")
		}
		base := ComputePageRank(clean)
		for id, want := range base.Scores {
			if !approxEq(pr.Scores[id], want) {
				t.Errorf("PageRank[%s] = %v with proxy, want %v (proxy distorted the real ranks)", id, pr.Scores[id], want)
			}
		}
	})

	t.Run("hits", func(t *testing.T) {
		h := ComputeHITS(withProxy)
		if _, ok := h.Authorities[proxyID]; ok {
			t.Error("proxy node must not receive a HITS authority score")
		}
		if _, ok := h.Hubs[proxyID]; ok {
			t.Error("proxy node must not receive a HITS hub score")
		}
	})

	t.Run("betweenness", func(t *testing.T) {
		b := ComputeBetweenness(withProxy)
		if _, ok := b.Scores[proxyID]; ok {
			t.Error("proxy node must not be a betweenness pivot")
		}
	})
}
