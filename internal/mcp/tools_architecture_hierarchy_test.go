package mcp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/graph"
)

// newArchHierarchyTestServer builds a server over a two-package fixture
// graph with cached communities, so get_architecture can be exercised
// with a resolution tier.
//
//	pkg/auth/login.go  :: Login, Logout   (community-0)
//	pkg/store/db.go    :: Save,  Load     (community-1)
//
// auth -> store carries 3 underlying call edges.
func newArchHierarchyTestServer(t *testing.T) *Server {
	t.Helper()
	g := graph.New()
	add := func(id, file string) {
		g.AddNode(&graph.Node{ID: id, Name: id, Kind: graph.KindFunction, FilePath: file, Language: "go", RepoPrefix: "demo"})
	}
	add("Login", "pkg/auth/login.go")
	add("Logout", "pkg/auth/login.go")
	add("Save", "pkg/store/db.go")
	add("Load", "pkg/store/db.go")
	g.AddEdge(&graph.Edge{From: "Login", To: "Logout", Kind: graph.EdgeCalls})
	g.AddEdge(&graph.Edge{From: "Login", To: "Save", Kind: graph.EdgeCalls})
	g.AddEdge(&graph.Edge{From: "Login", To: "Load", Kind: graph.EdgeCalls})
	g.AddEdge(&graph.Edge{From: "Logout", To: "Save", Kind: graph.EdgeCalls})
	g.AddEdge(&graph.Edge{From: "Save", To: "Load", Kind: graph.EdgeCalls})

	s := &Server{
		graph:      g,
		session:    newSessionState(),
		tokenStats: &tokenStats{},
		symHistory: &symbolHistory{entries: make(map[string][]SymbolModification)},
		sessions:   newSessionMap(),
		toolScopes: newScopeRegistry(),
	}
	s.analysisMu.Lock()
	s.communities = &analysis.CommunityResult{
		Communities: []analysis.Community{
			{ID: "community-0", Label: "auth", Members: []string{"Login", "Logout"}, Size: 2},
			{ID: "community-1", Label: "storage", Members: []string{"Save", "Load"}, Size: 2},
		},
		NodeToComm: map[string]string{
			"Login": "community-0", "Logout": "community-0",
			"Save": "community-1", "Load": "community-1",
		},
	}
	s.analysisMu.Unlock()
	return s
}

func callGetArchitecture(t *testing.T, s *Server, args map[string]any) map[string]any {
	t.Helper()
	req := mcp.CallToolRequest{}
	req.Params.Arguments = args
	res, err := s.handleGetArchitecture(context.Background(), req)
	require.NoError(t, err)
	require.NotNil(t, res)
	require.False(t, res.IsError)
	tc, ok := res.Content[0].(mcp.TextContent)
	require.True(t, ok)
	var m map[string]any
	require.NoError(t, json.Unmarshal([]byte(tc.Text), &m))
	return m
}

func TestArchitecture_NoResolutionOmitsHierarchy(t *testing.T) {
	s := newArchHierarchyTestServer(t)
	out := callGetArchitecture(t, s, map[string]any{})
	_, present := out["hierarchy"]
	assert.False(t, present, "hierarchy block must be absent when no resolution is requested")
}

func TestArchitecture_PackageResolution(t *testing.T) {
	s := newArchHierarchyTestServer(t)
	out := callGetArchitecture(t, s, map[string]any{"resolution": "package"})

	h, ok := out["hierarchy"].(map[string]any)
	require.True(t, ok, "hierarchy block must be present for resolution=package")
	assert.Equal(t, "package", h["level"])
	assert.EqualValues(t, 4, h["leaf_count"])
	assert.EqualValues(t, 2, h["node_count"])

	nodes, _ := h["nodes"].([]any)
	require.Len(t, nodes, 2)
	ids := map[string]int{}
	for _, n := range nodes {
		row := n.(map[string]any)
		ids[row["id"].(string)] = int(row["leaf_count"].(float64))
		// No function-leaf node IDs in the rollup.
		assert.Nil(t, s.graph.GetNode(row["id"].(string)),
			"rollup node %q must not be a base-graph leaf", row["id"])
	}
	assert.Equal(t, 2, ids["package:pkg/auth"])
	assert.Equal(t, 2, ids["package:pkg/store"])

	edges, _ := h["edges"].([]any)
	require.Len(t, edges, 1)
	edge := edges[0].(map[string]any)
	assert.Equal(t, "package:pkg/auth", edge["from"])
	assert.Equal(t, "package:pkg/store", edge["to"])
	// Weight = count of underlying function-level call edges crossing
	// the two packages (Login->Save, Login->Load, Logout->Save).
	assert.EqualValues(t, 3, edge["weight"])

	selfLoops, _ := h["self_loops"].(map[string]any)
	assert.EqualValues(t, 1, selfLoops["package:pkg/auth"])
	assert.EqualValues(t, 1, selfLoops["package:pkg/store"])
}

func TestArchitecture_ServiceResolution(t *testing.T) {
	s := newArchHierarchyTestServer(t)
	out := callGetArchitecture(t, s, map[string]any{"resolution": "service"})

	h, ok := out["hierarchy"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "service", h["level"])

	nodes, _ := h["nodes"].([]any)
	require.Len(t, nodes, 2, "service tier groups the two communities")
	for _, n := range nodes {
		row := n.(map[string]any)
		assert.Contains(t, []any{"service:community-0", "service:community-1"}, row["id"])
	}

	edges, _ := h["edges"].([]any)
	require.Len(t, edges, 1)
	edge := edges[0].(map[string]any)
	assert.EqualValues(t, 3, edge["weight"],
		"a module->module edge weight equals the count of underlying function-level call edges")
}

func TestArchitecture_SystemResolution(t *testing.T) {
	s := newArchHierarchyTestServer(t)
	out := callGetArchitecture(t, s, map[string]any{"resolution": "system"})
	h := out["hierarchy"].(map[string]any)
	assert.Equal(t, "system", h["level"])
	nodes, _ := h["nodes"].([]any)
	require.Len(t, nodes, 1, "single-repo fixture rolls up to one system node")
	assert.Equal(t, "system:demo", nodes[0].(map[string]any)["id"])
}

func TestArchitecture_UnknownResolutionIsError(t *testing.T) {
	s := newArchHierarchyTestServer(t)
	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{"resolution": "galaxy"}
	res, err := s.handleGetArchitecture(context.Background(), req)
	require.NoError(t, err)
	require.True(t, res.IsError, "an unknown resolution tier must return a clean error")
}

func TestArchitecture_HierarchyDeterministic(t *testing.T) {
	s := newArchHierarchyTestServer(t)
	first := callGetArchitecture(t, s, map[string]any{"resolution": "package"})
	firstJSON, _ := json.Marshal(first["hierarchy"])
	for i := 0; i < 5; i++ {
		again := callGetArchitecture(t, s, map[string]any{"resolution": "package"})
		againJSON, _ := json.Marshal(again["hierarchy"])
		assert.JSONEq(t, string(firstJSON), string(againJSON),
			"the hierarchy rollup must be stable across runs")
	}
}
