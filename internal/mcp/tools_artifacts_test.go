package mcp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/artifacts"
	"github.com/zzet/gortex/internal/graph"
)

func newArtifactsTestServer(t *testing.T) *Server {
	t.Helper()
	g := graph.New()
	g.AddNode(&graph.Node{ID: "models.go::User", Kind: graph.KindType, Name: "User", FilePath: "models.go"})
	g.AddNode(&graph.Node{ID: "artifact::db/schema.sql", Kind: graph.KindArtifact, Name: "schema.sql", FilePath: "db/schema.sql"})
	g.AddNode(&graph.Node{ID: "artifact::api/openapi.yaml", Kind: graph.KindArtifact, Name: "openapi.yaml", FilePath: "api/openapi.yaml"})

	s := &Server{
		graph:      g,
		session:    newSessionState(),
		tokenStats: &tokenStats{},
		symHistory: &symbolHistory{entries: make(map[string][]SymbolModification)},
		sessions:   newSessionMap(),
		toolScopes: newScopeRegistry(),
	}
	s.artifactsMu.Lock()
	s.artifactList = []artifacts.Artifact{
		{
			ID: "artifact::db/schema.sql", Path: "db/schema.sql", Name: "schema.sql",
			Kind: "schema", ContentHash: "abc123", Size: 200,
			References: []string{"models.go::User"},
		},
		{
			ID: "artifact::api/openapi.yaml", Path: "api/openapi.yaml", Name: "openapi.yaml",
			Kind: "api", ContentHash: "def456", Size: 900,
		},
	}
	s.artifactsMu.Unlock()
	s.artifactsOnce.Do(func() {}) // consume — ensureArtifacts becomes a no-op
	return s
}

func callArtifactTool(t *testing.T, s *Server, tool string, args map[string]any) (map[string]any, bool) {
	t.Helper()
	req := mcp.CallToolRequest{}
	req.Params.Arguments = args
	var (
		res *mcp.CallToolResult
		err error
	)
	switch tool {
	case "search":
		res, err = s.handleSearchArtifacts(context.Background(), req)
	case "get":
		res, err = s.handleGetArtifact(context.Background(), req)
	}
	require.NoError(t, err)
	require.NotNil(t, res)
	tc, ok := res.Content[0].(mcp.TextContent)
	require.True(t, ok)
	if res.IsError {
		return nil, true
	}
	var m map[string]any
	require.NoError(t, json.Unmarshal([]byte(tc.Text), &m))
	return m, false
}

func TestSearchArtifacts_All(t *testing.T) {
	s := newArtifactsTestServer(t)
	out, isErr := callArtifactTool(t, s, "search", map[string]any{})
	require.False(t, isErr)
	rows, _ := out["artifacts"].([]any)
	require.Len(t, rows, 2)
}

func TestSearchArtifacts_KindFilter(t *testing.T) {
	s := newArtifactsTestServer(t)
	out, _ := callArtifactTool(t, s, "search", map[string]any{"kind": "schema"})
	rows, _ := out["artifacts"].([]any)
	require.Len(t, rows, 1)
	first, _ := rows[0].(map[string]any)
	require.Equal(t, "db/schema.sql", first["path"])
	require.Equal(t, float64(1), first["reference_count"])
}

func TestSearchArtifacts_QueryFilter(t *testing.T) {
	s := newArtifactsTestServer(t)
	out, _ := callArtifactTool(t, s, "search", map[string]any{"query": "openapi"})
	rows, _ := out["artifacts"].([]any)
	require.Len(t, rows, 1)
	first, _ := rows[0].(map[string]any)
	require.Equal(t, "api", first["kind"])
}

func TestGetArtifact_ByID(t *testing.T) {
	s := newArtifactsTestServer(t)
	out, isErr := callArtifactTool(t, s, "get", map[string]any{"id": "artifact::db/schema.sql"})
	require.False(t, isErr)
	require.Equal(t, "schema", out["kind"])
	require.Equal(t, "abc123", out["content_hash"])
	refs, _ := out["references"].([]any)
	require.Len(t, refs, 1)
	first, _ := refs[0].(map[string]any)
	require.Equal(t, "User", first["name"])
}

func TestGetArtifact_ByPath(t *testing.T) {
	s := newArtifactsTestServer(t)
	out, isErr := callArtifactTool(t, s, "get", map[string]any{"path": "api/openapi.yaml"})
	require.False(t, isErr)
	require.Equal(t, "artifact::api/openapi.yaml", out["id"])
}

func TestGetArtifact_NotFound(t *testing.T) {
	s := newArtifactsTestServer(t)
	_, isErr := callArtifactTool(t, s, "get", map[string]any{"id": "artifact::nope"})
	require.True(t, isErr)
}

func TestGetArtifact_MissingArgs(t *testing.T) {
	s := newArtifactsTestServer(t)
	_, isErr := callArtifactTool(t, s, "get", map[string]any{})
	require.True(t, isErr)
}
