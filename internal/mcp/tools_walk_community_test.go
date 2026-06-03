package mcp

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/graph"
)

// seedWalkCommunityGraph injects a two-community call chain:
//   x.go::X (comm "alpha") -> y.go::Y (comm "alpha") -> z.go::Z (comm "beta")
func seedWalkCommunityGraph(t *testing.T, srv *Server) {
	t.Helper()
	g := srv.graph
	add := func(id, name, file string) {
		g.AddNode(&graph.Node{ID: id, Kind: graph.KindFunction, Name: name, FilePath: file, Language: "go"})
	}
	add("x.go::X", "X", "x.go")
	add("y.go::Y", "Y", "y.go")
	add("z.go::Z", "Z", "z.go")
	g.AddEdge(&graph.Edge{From: "x.go::X", To: "y.go::Y", Kind: graph.EdgeCalls, FilePath: "x.go", Line: 1})
	g.AddEdge(&graph.Edge{From: "y.go::Y", To: "z.go::Z", Kind: graph.EdgeCalls, FilePath: "y.go", Line: 1})

	srv.analysisMu.Lock()
	srv.communities = &analysis.CommunityResult{
		Communities: []analysis.Community{
			{ID: "c-alpha", Label: "alpha"},
			{ID: "c-beta", Label: "beta"},
		},
		NodeToComm: map[string]string{
			"x.go::X": "c-alpha",
			"y.go::Y": "c-alpha",
			"z.go::Z": "c-beta",
		},
	}
	srv.analysisMu.Unlock()
}

func walkResultIDs(t *testing.T, out map[string]any) map[string]bool {
	t.Helper()
	ids := map[string]bool{}
	nodes, ok := out["nodes"].([]any)
	require.True(t, ok, "walk result must carry nodes")
	for _, n := range nodes {
		row := n.(map[string]any)
		if id, ok := row["id"].(string); ok {
			ids[id] = true
		}
	}
	return ids
}

func TestWalkGraph_CommunityFilter_ByLabel(t *testing.T) {
	srv, _ := setupTestServer(t)
	seedWalkCommunityGraph(t, srv)

	out := extractTextResult(t, callTool(t, srv, "walk_graph", map[string]any{
		"id":         "x.go::X",
		"edge_kinds": "calls",
		"direction":  "out",
		"community":  "alpha", // label, resolved to c-alpha
	}))
	ids := walkResultIDs(t, out)
	assert.True(t, ids["x.go::X"])
	assert.True(t, ids["y.go::Y"])
	// z is in c-beta -> dropped.
	assert.False(t, ids["z.go::Z"], "cross-community node must be dropped")
}

func TestWalkGraph_CommunityFilter_ByID(t *testing.T) {
	srv, _ := setupTestServer(t)
	seedWalkCommunityGraph(t, srv)

	out := extractTextResult(t, callTool(t, srv, "walk_graph", map[string]any{
		"id":         "x.go::X",
		"edge_kinds": "calls",
		"direction":  "out",
		"community":  "c-alpha", // raw ID
	}))
	ids := walkResultIDs(t, out)
	assert.True(t, ids["y.go::Y"])
	assert.False(t, ids["z.go::Z"])
}

func TestWalkGraph_CommunityFilter_NoArgIncludesAll(t *testing.T) {
	srv, _ := setupTestServer(t)
	seedWalkCommunityGraph(t, srv)

	out := extractTextResult(t, callTool(t, srv, "walk_graph", map[string]any{
		"id":         "x.go::X",
		"edge_kinds": "calls",
		"direction":  "out",
	}))
	ids := walkResultIDs(t, out)
	// No community filter -> the whole reachable chain is returned.
	assert.True(t, ids["y.go::Y"])
	assert.True(t, ids["z.go::Z"])
}

func TestWalkGraph_CommunityFilter_NilCommunitiesNoOp(t *testing.T) {
	srv, _ := setupTestServer(t)
	// Seed the graph but NOT the communities — getCommunities() is nil.
	g := srv.graph
	g.AddNode(&graph.Node{ID: "x.go::X", Kind: graph.KindFunction, Name: "X", FilePath: "x.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "y.go::Y", Kind: graph.KindFunction, Name: "Y", FilePath: "y.go", Language: "go"})
	g.AddEdge(&graph.Edge{From: "x.go::X", To: "y.go::Y", Kind: graph.EdgeCalls, FilePath: "x.go", Line: 1})
	srv.analysisMu.Lock()
	srv.communities = nil
	srv.analysisMu.Unlock()

	out := extractTextResult(t, callTool(t, srv, "walk_graph", map[string]any{
		"id":         "x.go::X",
		"edge_kinds": "calls",
		"direction":  "out",
		"community":  "alpha",
	}))
	ids := walkResultIDs(t, out)
	// Filter no-ops when communities are nil -> Y still present.
	assert.True(t, ids["y.go::Y"])
}
