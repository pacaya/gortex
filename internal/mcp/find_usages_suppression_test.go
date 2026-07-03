package mcp

import (
	"encoding/json"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
)

// buildSuppressionGraph returns a graph where Target has one resolver-verified
// (lsp_resolved) usage and two name-only (text_matched) usages — so the
// adaptive default suppresses the two text_matched edges and reports
// TextMatchedSuppressed == 2.
func buildSuppressionGraph() graph.Store {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "a.go", Kind: graph.KindFile, Name: "a.go", FilePath: "a.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "b.go", Kind: graph.KindFile, Name: "b.go", FilePath: "b.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "a.go::Target", Kind: graph.KindFunction, Name: "Target", FilePath: "a.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "a.go::Strong", Kind: graph.KindFunction, Name: "Strong", FilePath: "a.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "b.go::Weak1", Kind: graph.KindFunction, Name: "Weak1", FilePath: "b.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "b.go::Weak2", Kind: graph.KindFunction, Name: "Weak2", FilePath: "b.go", Language: "go"})

	g.AddEdge(&graph.Edge{From: "a.go::Strong", To: "a.go::Target", Kind: graph.EdgeCalls, FilePath: "a.go", Line: 3, Origin: graph.OriginLSPResolved})
	g.AddEdge(&graph.Edge{From: "b.go::Weak1", To: "a.go::Target", Kind: graph.EdgeCalls, FilePath: "b.go", Line: 5, Origin: graph.OriginTextMatched})
	g.AddEdge(&graph.Edge{From: "b.go::Weak2", To: "a.go::Target", Kind: graph.EdgeCalls, FilePath: "b.go", Line: 7, Origin: graph.OriginTextMatched})
	return g
}

func serverForGraph(t *testing.T, g graph.Store) *Server {
	t.Helper()
	return NewServer(query.NewEngine(g), g, nil, nil, zap.NewNop(), nil)
}

func findUsagesSubGraph(t *testing.T, srv *Server) query.SubGraph {
	t.Helper()
	result := callTool(t, srv, "find_usages", map[string]any{"id": "a.go::Target"})
	require.False(t, result.IsError)
	var resp query.SubGraph
	require.NoError(t, json.Unmarshal([]byte(result.Content[0].(mcplib.TextContent).Text), &resp))
	return resp
}

// TestFindUsages_SuppressionCaveat is the F2.2 regression: the diagnosable
// rider fires only when text_matched edges were suppressed AND the target's
// file is in the re-parsed-but-not-re-enriched window (the file node carries
// graph.MetaReparsePendingEnrichment). On a converged graph the suppressed
// count is still reported but no rider is attached.
func TestFindUsages_SuppressionCaveat(t *testing.T) {
	g := buildSuppressionGraph()

	// Converged file (no pending-enrichment marker): suppression happens but
	// the rider must stay absent so it can't cry wolf on a fresh graph.
	fresh := findUsagesSubGraph(t, serverForGraph(t, g))
	assert.Equal(t, 2, fresh.TextMatchedSuppressed, "two text_matched usages must be suppressed")
	assert.Empty(t, fresh.SuppressionCaveat, "no rider on a converged (non-stale) file")

	// Mark a.go re-parsed without re-enrichment, then re-query: the rider must
	// appear and name the hidden count.
	fn := g.GetNode("a.go")
	if fn.Meta == nil {
		fn.Meta = map[string]any{}
	}
	fn.Meta[graph.MetaReparsePendingEnrichment] = true
	g.AddNode(fn)

	stale := findUsagesSubGraph(t, serverForGraph(t, g))
	assert.Equal(t, 2, stale.TextMatchedSuppressed, "suppressed count unchanged by the marker")
	require.NotEmpty(t, stale.SuppressionCaveat, "rider must fire on a re-parsed-not-re-enriched file")
	assert.Contains(t, stale.SuppressionCaveat, "2 candidate")
	assert.Contains(t, stale.SuppressionCaveat, "min_tier")
}
