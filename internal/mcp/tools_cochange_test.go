package mcp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/cochange"
	"github.com/zzet/gortex/internal/graph"
)

// newCoChangeTestServer builds a server with a small graph and the
// co-change caches pre-populated. The cochangeOnce is consumed with a
// no-op so the handler's ensureCoChange() does not shell out to git.
func newCoChangeTestServer(t *testing.T) *Server {
	t.Helper()
	g := graph.New()
	for _, f := range []string{"a.go", "b.go", "c.go", "lonely.go"} {
		g.AddNode(&graph.Node{ID: f, Kind: graph.KindFile, Name: f, FilePath: f, Language: "go"})
	}
	g.AddNode(&graph.Node{ID: "a.go::Foo", Kind: graph.KindFunction, Name: "Foo", FilePath: "a.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "b.go::Bar", Kind: graph.KindFunction, Name: "Bar", FilePath: "b.go", Language: "go"})

	s := &Server{
		graph:      g,
		session:    newSessionState(),
		tokenStats: &tokenStats{},
		symHistory: &symbolHistory{entries: make(map[string][]SymbolModification)},
		sessions:   newSessionMap(),
		toolScopes: newScopeRegistry(),
	}
	s.storeCoChange(
		map[string]map[string]float64{
			"a.go": {"b.go": 0.9, "c.go": 0.3},
		},
		map[string]map[string]int{
			"a.go": {"b.go": 5, "c.go": 2},
		},
	)
	// Consume the once-guard so ensureCoChange becomes a no-op.
	s.cochangeOnce.Do(func() {})
	return s
}

func callFindCoChanging(t *testing.T, s *Server, args map[string]any) (map[string]any, bool) {
	t.Helper()
	req := mcp.CallToolRequest{}
	req.Params.Arguments = args
	res, err := s.handleFindCoChangingSymbols(context.Background(), req)
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

func TestFindCoChanging_ByFilePath(t *testing.T) {
	s := newCoChangeTestServer(t)
	out, isErr := callFindCoChanging(t, s, map[string]any{"file_path": "a.go"})
	require.False(t, isErr)

	rows, _ := out["co_changing"].([]any)
	require.Len(t, rows, 2)
	// Sorted by score descending: b.go (0.9) before c.go (0.3).
	first, _ := rows[0].(map[string]any)
	require.Equal(t, "b.go", first["file"])
	require.Equal(t, float64(5), first["count"])
	require.InDelta(t, 0.9, first["score"], 0.001)
	syms, _ := first["symbols"].([]any)
	require.Contains(t, syms, "Bar")
}

func TestFindCoChanging_BySymbolID(t *testing.T) {
	s := newCoChangeTestServer(t)
	out, isErr := callFindCoChanging(t, s, map[string]any{"symbol_id": "a.go::Foo"})
	require.False(t, isErr)
	require.Equal(t, "a.go", out["target_file"])
	require.Equal(t, "a.go::Foo", out["symbol_id"])
	rows, _ := out["co_changing"].([]any)
	require.Len(t, rows, 2)
}

func TestFindCoChanging_MinScoreFilter(t *testing.T) {
	s := newCoChangeTestServer(t)
	out, isErr := callFindCoChanging(t, s, map[string]any{"file_path": "a.go", "min_score": 0.5})
	require.False(t, isErr)
	rows, _ := out["co_changing"].([]any)
	require.Len(t, rows, 1)
	first, _ := rows[0].(map[string]any)
	require.Equal(t, "b.go", first["file"])
}

func TestFindCoChanging_NoData(t *testing.T) {
	s := newCoChangeTestServer(t)
	out, isErr := callFindCoChanging(t, s, map[string]any{"file_path": "lonely.go"})
	require.False(t, isErr)
	rows, _ := out["co_changing"].([]any)
	require.Empty(t, rows)
}

func TestFindCoChanging_MissingArgs(t *testing.T) {
	s := newCoChangeTestServer(t)
	_, isErr := callFindCoChanging(t, s, map[string]any{})
	require.True(t, isErr, "expected an error when neither symbol_id nor file_path is given")
}

func TestFindCoChanging_UnknownSymbol(t *testing.T) {
	s := newCoChangeTestServer(t)
	_, isErr := callFindCoChanging(t, s, map[string]any{"symbol_id": "does/not::Exist"})
	require.True(t, isErr)
}

// TestCoChange_PersistedEdgesTakeFastPath proves change B's mechanism:
// mineCoChange persists mined pairs as EdgeCoChange edges (via
// cochange.AddEdges), so a subsequent daemon start reads them back via
// coChangeFromEdges (the fast path) instead of re-mining git log.
func TestCoChange_PersistedEdgesTakeFastPath(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "a.go", Kind: graph.KindFile, Name: "a.go", FilePath: "a.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "b.go", Kind: graph.KindFile, Name: "b.go", FilePath: "b.go", Language: "go"})

	// What mineCoChange now does after a git mine: persist the pairs.
	n := cochange.AddEdges(g, []cochange.Pair{{FileA: "a.go", FileB: "b.go", Score: 0.9, Count: 5}}, "")
	require.Positive(t, n, "AddEdges must persist EdgeCoChange edges")

	// A fresh server over the same graph takes the coChangeFromEdges
	// fast path (no git mine) and surfaces the persisted co-change.
	s := &Server{
		graph:      g,
		session:    newSessionState(),
		tokenStats: &tokenStats{},
		symHistory: &symbolHistory{entries: make(map[string][]SymbolModification)},
		sessions:   newSessionMap(),
		toolScopes: newScopeRegistry(),
	}
	scores := map[string]map[string]float64{}
	counts := map[string]map[string]int{}
	require.True(t, s.coChangeFromEdges(scores, counts), "persisted edges must take the fast path")
	require.InDelta(t, 0.9, scores["a.go"]["b.go"], 1e-9)
	require.Equal(t, 5, counts["a.go"]["b.go"])
}
