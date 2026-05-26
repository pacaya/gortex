package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/query"
)

// setupNavServer indexes a Go source with a deeper call graph and a type
// carrying several methods, so the nav tool's into / up / sibling moves
// have real candidates to choose between.
func setupNavServer(t *testing.T) (*Server, graph.Store) {
	t.Helper()
	dir := t.TempDir()
	src := `package svc

type Service struct {
	name string
}

func (s *Service) Start() {
	s.boot()
	s.warm()
}

func (s *Service) Stop() {}

func (s *Service) boot() {}

func (s *Service) warm() {}

func run() {
	svc := &Service{}
	svc.Start()
}
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "svc.go"), []byte(src), 0o644))

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
	return srv, g
}

// navResult unmarshals a nav response payload.
func navResult(t *testing.T, result *mcplib.CallToolResult) map[string]any {
	t.Helper()
	require.False(t, result.IsError, "nav errored: %s", toolResultText(result))
	var payload map[string]any
	require.NoError(t, json.Unmarshal([]byte(result.Content[0].(mcplib.TextContent).Text), &payload))
	return payload
}

// navFindMethod returns the graph ID of a method named `name`.
func navFindMethod(t *testing.T, g graph.Store, name string) string {
	t.Helper()
	for _, n := range g.AllNodes() {
		if n.Name == name && (n.Kind == graph.KindMethod || n.Kind == graph.KindFunction) {
			return n.ID
		}
	}
	t.Fatalf("no method/function named %q in the test graph", name)
	return ""
}

func navCursorCurrent(payload map[string]any) string {
	cursor, _ := payload["cursor"].(map[string]any)
	if cursor == nil {
		return ""
	}
	s, _ := cursor["current"].(string)
	return s
}

func TestNav_RequiresAction(t *testing.T) {
	srv, _ := setupNavServer(t)
	result := callTool(t, srv, "nav", map[string]any{})
	require.True(t, result.IsError)
	assert.Contains(t, toolResultText(result), "action is required")
}

func TestNav_GotoSetsCursor(t *testing.T) {
	srv, g := setupNavServer(t)
	start := navFindMethod(t, g, "Start")

	payload := navResult(t, callTool(t, srv, "nav", map[string]any{
		"action": "goto", "id": start,
	}))
	assert.Equal(t, start, navCursorCurrent(payload))
	assert.Equal(t, "goto", payload["action"])
}

func TestNav_GotoMissingID(t *testing.T) {
	srv, _ := setupNavServer(t)
	result := callTool(t, srv, "nav", map[string]any{"action": "goto"})
	require.True(t, result.IsError)
	assert.Contains(t, toolResultText(result), "id is required")
}

func TestNav_GotoUnknownSymbol(t *testing.T) {
	srv, _ := setupNavServer(t)
	result := callTool(t, srv, "nav", map[string]any{
		"action": "goto", "id": "no/such::Symbol",
	})
	require.True(t, result.IsError)
	assert.Contains(t, toolResultText(result), "symbol not found")
}

func TestNav_IntoMovesToCallee(t *testing.T) {
	srv, g := setupNavServer(t)
	start := navFindMethod(t, g, "Start")

	callTool(t, srv, "nav", map[string]any{"action": "goto", "id": start})
	payload := navResult(t, callTool(t, srv, "nav", map[string]any{"action": "into"}))

	// Start calls boot and warm; into with no select picks the first.
	cur := navCursorCurrent(payload)
	assert.NotEqual(t, start, cur, "into must advance the cursor")
	node := g.GetNode(cur)
	require.NotNil(t, node)
	assert.Contains(t, []string{"boot", "warm"}, node.Name)
}

func TestNav_IntoSelectByName(t *testing.T) {
	srv, g := setupNavServer(t)
	start := navFindMethod(t, g, "Start")

	callTool(t, srv, "nav", map[string]any{"action": "goto", "id": start})
	payload := navResult(t, callTool(t, srv, "nav", map[string]any{
		"action": "into", "select": "warm",
	}))
	node := g.GetNode(navCursorCurrent(payload))
	require.NotNil(t, node)
	assert.Equal(t, "warm", node.Name, "select by name substring must target warm")
}

func TestNav_IntoSelectByIndex(t *testing.T) {
	srv, g := setupNavServer(t)
	start := navFindMethod(t, g, "Start")

	callTool(t, srv, "nav", map[string]any{"action": "goto", "id": start})
	payload := navResult(t, callTool(t, srv, "nav", map[string]any{
		"action": "into", "select": "1",
	}))
	node := g.GetNode(navCursorCurrent(payload))
	require.NotNil(t, node)
	assert.Contains(t, []string{"boot", "warm"}, node.Name)
}

func TestNav_IntoSelectOutOfRange(t *testing.T) {
	srv, g := setupNavServer(t)
	start := navFindMethod(t, g, "Start")
	callTool(t, srv, "nav", map[string]any{"action": "goto", "id": start})
	result := callTool(t, srv, "nav", map[string]any{
		"action": "into", "select": "99",
	})
	require.True(t, result.IsError)
	assert.Contains(t, toolResultText(result), "out of range")
}

func TestNav_UpMovesToCaller(t *testing.T) {
	srv, g := setupNavServer(t)
	boot := navFindMethod(t, g, "boot")

	callTool(t, srv, "nav", map[string]any{"action": "goto", "id": boot})
	payload := navResult(t, callTool(t, srv, "nav", map[string]any{"action": "up"}))
	node := g.GetNode(navCursorCurrent(payload))
	require.NotNil(t, node)
	assert.Equal(t, "Start", node.Name, "up from boot must reach its caller Start")
}

func TestNav_SiblingMovesAcrossParent(t *testing.T) {
	srv, g := setupNavServer(t)
	boot := navFindMethod(t, g, "boot")

	callTool(t, srv, "nav", map[string]any{"action": "goto", "id": boot})
	payload := navResult(t, callTool(t, srv, "nav", map[string]any{"action": "sibling"}))
	node := g.GetNode(navCursorCurrent(payload))
	require.NotNil(t, node)
	// boot is a method of Service; its siblings are the other members
	// of Service — the Start / Stop / warm methods and the name field.
	assert.Contains(t, []string{"Start", "Stop", "warm", "name"}, node.Name)
	assert.NotEqual(t, "boot", node.Name)
}

// TestNav_SiblingSelectByName targets a specific sibling by name.
func TestNav_SiblingSelectByName(t *testing.T) {
	srv, g := setupNavServer(t)
	boot := navFindMethod(t, g, "boot")

	callTool(t, srv, "nav", map[string]any{"action": "goto", "id": boot})
	payload := navResult(t, callTool(t, srv, "nav", map[string]any{
		"action": "sibling", "select": "Stop",
	}))
	node := g.GetNode(navCursorCurrent(payload))
	require.NotNil(t, node)
	assert.Equal(t, "Stop", node.Name, "sibling select by name must target Stop")
}

func TestNav_BackPopsHistory(t *testing.T) {
	srv, g := setupNavServer(t)
	start := navFindMethod(t, g, "Start")

	callTool(t, srv, "nav", map[string]any{"action": "goto", "id": start})
	callTool(t, srv, "nav", map[string]any{"action": "into"}) // moved to a callee
	payload := navResult(t, callTool(t, srv, "nav", map[string]any{"action": "back"}))
	assert.Equal(t, start, navCursorCurrent(payload), "back must return to Start")
}

func TestNav_BackEmptyHistory(t *testing.T) {
	srv, g := setupNavServer(t)
	start := navFindMethod(t, g, "Start")
	callTool(t, srv, "nav", map[string]any{"action": "goto", "id": start})
	result := callTool(t, srv, "nav", map[string]any{"action": "back"})
	require.True(t, result.IsError)
	assert.Contains(t, toolResultText(result), "history is empty")
}

func TestNav_WhereReportsCursor(t *testing.T) {
	srv, g := setupNavServer(t)
	start := navFindMethod(t, g, "Start")

	callTool(t, srv, "nav", map[string]any{"action": "goto", "id": start})
	payload := navResult(t, callTool(t, srv, "nav", map[string]any{"action": "where"}))
	assert.Equal(t, start, navCursorCurrent(payload))
	assert.Equal(t, "where", payload["action"])

	adj, ok := payload["adjacency"].(map[string]any)
	require.True(t, ok, "where must carry an adjacency preview")
	// Start calls boot and warm.
	assert.Equal(t, float64(2), adj["callees"])
}

func TestNav_WhereUnsetCursor(t *testing.T) {
	srv, _ := setupNavServer(t)
	result := callTool(t, srv, "nav", map[string]any{"action": "where"})
	require.True(t, result.IsError)
	assert.Contains(t, toolResultText(result), "cursor is unset")
}

func TestNav_ReadReturnsSource(t *testing.T) {
	srv, g := setupNavServer(t)
	start := navFindMethod(t, g, "Start")

	callTool(t, srv, "nav", map[string]any{"action": "goto", "id": start})
	payload := navResult(t, callTool(t, srv, "nav", map[string]any{"action": "read"}))
	src, _ := payload["source"].(string)
	assert.Contains(t, src, "func (s *Service) Start()", "read must return the symbol source")
}

func TestNav_ReadUnsetCursor(t *testing.T) {
	srv, _ := setupNavServer(t)
	result := callTool(t, srv, "nav", map[string]any{"action": "read"})
	require.True(t, result.IsError)
	assert.Contains(t, toolResultText(result), "cursor is unset")
}

func TestNav_UnknownAction(t *testing.T) {
	srv, _ := setupNavServer(t)
	result := callTool(t, srv, "nav", map[string]any{"action": "teleport"})
	require.True(t, result.IsError)
	assert.Contains(t, toolResultText(result), "unknown action")
}

// TestNav_StaleCursorReset verifies that when a re-index removes the
// node the cursor points at, the next nav call resets gracefully.
func TestNav_StaleCursorReset(t *testing.T) {
	srv, g := setupNavServer(t)
	start := navFindMethod(t, g, "Start")

	// Position the cursor, then evict the file out from under it so
	// the cursor node no longer exists in the graph.
	callTool(t, srv, "nav", map[string]any{"action": "goto", "id": start})
	node := g.GetNode(start)
	require.NotNil(t, node)
	g.EvictFile(node.FilePath)

	result := callTool(t, srv, "nav", map[string]any{"action": "where"})
	// where on a now-empty cursor is an error, but it must report the
	// reset rather than operate on the dangling ID.
	require.True(t, result.IsError)
	assert.Contains(t, toolResultText(result), "cursor is unset")
}
