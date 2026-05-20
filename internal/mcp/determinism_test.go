package mcp

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

// setupDeterminismServer indexes a fixture deliberately full of
// interchangeable symbols — 40 identically-shaped Handler functions
// that each call shared() — so BM25 scores and the structural rerank
// signals tie en masse. Any reliance on Go's randomised map iteration
// then surfaces as run-to-run variance.
func setupDeterminismServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	var src strings.Builder
	src.WriteString("package handlers\n\nfunc shared() {}\n\n")
	for i := 0; i < 40; i++ {
		fmt.Fprintf(&src, "func Handler%02d() { shared() }\n", i)
	}
	src.WriteString("\nfunc dispatch() {\n")
	for i := 0; i < 40; i++ {
		fmt.Fprintf(&src, "\tHandler%02d()\n", i)
	}
	src.WriteString("}\n")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "handlers.go"), []byte(src.String()), 0o644))

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
	return srv
}

func stableToolText(t *testing.T, srv *Server, tool string, args map[string]any) string {
	t.Helper()
	res := callTool(t, srv, tool, args)
	require.Falsef(t, res.IsError, "%s returned an error", tool)
	require.NotEmpty(t, res.Content)
	return res.Content[0].(mcplib.TextContent).Text
}

// TestDeterminism_ReadToolsByteIdentical asserts that repeated
// invocations of the result-set tools produce byte-identical output
// on a fixture engineered to tie BM25 and structural signals.
func TestDeterminism_ReadToolsByteIdentical(t *testing.T) {
	srv := setupDeterminismServer(t)

	const runs = 25
	assertStable := func(name, tool string, args map[string]any) {
		t.Run(name, func(t *testing.T) {
			first := stableToolText(t, srv, tool, args)
			for run := 1; run < runs; run++ {
				if got := stableToolText(t, srv, tool, args); got != first {
					t.Fatalf("%s run %d is not byte-identical to run 0", tool, run)
				}
			}
		})
	}

	assertStable("search_concept", "search_symbols", map[string]any{"query": "handler"})
	assertStable("search_identifier", "search_symbols", map[string]any{"query": "Handler17"})
	assertStable("get_repo_outline", "get_repo_outline", map[string]any{})
	assertStable("get_file_summary", "get_file_summary", map[string]any{"path": "handlers.go"})

	// find_usages over a 40-caller symbol exercises the usage-map
	// extraction order most heavily.
	sharedID := ""
	for _, id := range resultIDs(searchSymbolsResp(t, srv, "shared")) {
		if strings.HasSuffix(id, "::shared") {
			sharedID = id
		}
	}
	require.NotEmpty(t, sharedID, "could not resolve the shared() symbol ID")
	assertStable("find_usages", "find_usages", map[string]any{"id": sharedID})
}
