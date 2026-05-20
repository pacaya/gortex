package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/query"
)

func TestSuggestQueries(t *testing.T) {
	dir := t.TempDir()
	src := `package app

func shared() {}

func alpha() { shared() }
func beta()  { shared() }
func gamma() { shared() }
func delta() { shared() }

func main() {
	alpha()
	beta()
	gamma()
	delta()
}
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"), []byte(src), 0o644))

	g := graph.New()
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	cfg := config.Default()
	idx := indexer.New(g, reg, cfg.Index, zap.NewNop())
	_, err := idx.Index(dir)
	require.NoError(t, err)
	eng := query.NewEngine(g)
	srv := NewServer(eng, g, idx, nil, zap.NewNop(), nil)
	srv.RunAnalysis()

	decode := func(res *mcplib.CallToolResult) struct {
		Suggestions []suggestedQuery `json:"suggestions"`
		Count       int              `json:"count"`
	} {
		require.False(t, res.IsError)
		var out struct {
			Suggestions []suggestedQuery `json:"suggestions"`
			Count       int              `json:"count"`
		}
		require.NoError(t, json.Unmarshal([]byte(res.Content[0].(mcplib.TextContent).Text), &out))
		return out
	}

	resp := decode(callTool(t, srv, "suggest_queries", map[string]any{}))
	require.GreaterOrEqual(t, resp.Count, 2, "expected an entry point and a load-bearing symbol")
	require.Len(t, resp.Suggestions, resp.Count)

	cats := map[string]bool{}
	queries := map[string]bool{}
	for _, sg := range resp.Suggestions {
		require.NotEmpty(t, sg.Query, "suggestion query must be non-empty")
		require.NotEmpty(t, sg.Why, "suggestion rationale must be non-empty")
		require.NotEmpty(t, sg.Category, "suggestion category must be non-empty")
		cats[sg.Category] = true
		queries[sg.Query] = true
	}
	require.True(t, cats["entry_point"], "should suggest an entry point")
	require.True(t, cats["hub"] || cats["bridge"],
		"should suggest a load-bearing symbol (hub or bridge)")
	require.True(t, queries["shared"],
		"shared() — the fan-in-4 symbol — should be among the suggestions")

	// The limit argument is honoured.
	limited := decode(callTool(t, srv, "suggest_queries", map[string]any{"limit": 1}))
	require.LessOrEqual(t, limited.Count, 1, "limit=1 must cap the suggestion list")
}
