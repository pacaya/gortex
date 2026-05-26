package mcp

import (
	"context"
	"encoding/json"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/zzet/gortex/internal/graph"
)

func callAnalyzeGoroutineSpawns(t *testing.T, srv *Server, args map[string]any) map[string]any {
	t.Helper()
	args["kind"] = "goroutine_spawns"
	req := mcplib.CallToolRequest{}
	req.Params.Name = "analyze"
	req.Params.Arguments = args
	res, err := srv.handleAnalyze(context.Background(), req)
	if err != nil {
		t.Fatalf("handleAnalyze: %v", err)
	}
	if res.IsError {
		t.Fatalf("error: %+v", res.Content)
	}
	textBlock := res.Content[0].(mcplib.TextContent)
	var out map[string]any
	if err := json.Unmarshal([]byte(textBlock.Text), &out); err != nil {
		t.Fatalf("json: %v\n%s", err, textBlock.Text)
	}
	return out
}

// addSpawnEdge inserts a spawn edge keyed by (from, to, line) so each
// site is unique under the graph's edge-dedup key. Meta is dropped
// when mode is empty so the analyzer's "modeless spawn" path is
// exercisable.
func addSpawnEdge(g graph.Store, from, to, mode string, line int) {
	e := &graph.Edge{From: from, To: to, Kind: graph.EdgeSpawns, FilePath: "f.go", Line: line}
	if mode != "" {
		e.Meta = map[string]any{"mode": mode}
	}
	g.AddEdge(e)
}

func TestAnalyzeGoroutineSpawns_GroupsByTargetAndMode(t *testing.T) {
	srv, _ := setupTestServer(t)
	addSpawnEdge(srv.graph, "f.go::Run", "f.go::Worker", "goroutine", 10)
	addSpawnEdge(srv.graph, "f.go::Run", "f.go::Worker", "goroutine", 11)
	addSpawnEdge(srv.graph, "f.go::Tick", "f.go::Worker", "goroutine", 20)
	// Different mode for the same target lands as a separate row —
	// we don't conflate goroutine vs. async spawn shapes.
	addSpawnEdge(srv.graph, "f.go::Run", "f.go::Worker", "async", 30)

	out := callAnalyzeGoroutineSpawns(t, srv, map[string]any{})
	rows, _ := out["spawns"].([]any)
	if len(rows) != 2 {
		t.Fatalf("expected 2 grouped rows (goroutine+async), got %d", len(rows))
	}
	first := rows[0].(map[string]any)
	if first["mode"] != "goroutine" {
		t.Errorf("expected goroutine first (3 spawns), got %v", first)
	}
	if got, _ := first["spawns"].(float64); got != 3 {
		t.Errorf("spawns = %v, want 3", got)
	}
}

func TestAnalyzeGoroutineSpawns_EmptyWhenNoSpawnEdges(t *testing.T) {
	srv, _ := setupTestServer(t)
	out := callAnalyzeGoroutineSpawns(t, srv, map[string]any{})
	if got, _ := out["total"].(float64); got != 0 {
		t.Errorf("expected 0 spawns, got %v", got)
	}
}

func TestAnalyzeGoroutineSpawns_ModelessSpawnsAreReported(t *testing.T) {
	// A spawn edge missing meta.mode is still a spawn site — the
	// analyzer must surface it rather than silently drop it.
	srv, _ := setupTestServer(t)
	addSpawnEdge(srv.graph, "f.go::Run", "f.go::Worker", "", 5)

	out := callAnalyzeGoroutineSpawns(t, srv, map[string]any{})
	rows, _ := out["spawns"].([]any)
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
}
