package mcp

import (
	"context"
	"encoding/json"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/zzet/gortex/internal/graph"
)

func callAnalyzeErrorSurface(t *testing.T, srv *Server, args map[string]any) map[string]any {
	t.Helper()
	args["kind"] = "error_surface"
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

func addThrowsEdge(g graph.Store, from, to, file string, line int) {
	g.AddEdge(&graph.Edge{
		From:     from,
		To:       to,
		Kind:     graph.EdgeThrows,
		FilePath: file,
		Line:     line,
	})
}

func TestAnalyzeErrorSurface_GroupsByThrower(t *testing.T) {
	srv, _ := setupTestServer(t)
	addThrowsEdge(srv.graph, "f.go::DoThing", "external::error", "f.go", 10)
	addThrowsEdge(srv.graph, "f.go::DoThing", "unresolved::ParseError", "f.go", 11)
	addThrowsEdge(srv.graph, "g.go::Other", "external::error", "g.go", 5)

	out := callAnalyzeErrorSurface(t, srv, map[string]any{})
	rows, _ := out["throwers"].([]any)
	if len(rows) != 2 {
		t.Fatalf("expected 2 throwers, got %d", len(rows))
	}
	first := rows[0].(map[string]any)
	if first["symbol"] != "f.go::DoThing" {
		t.Errorf("expected DoThing first (2 distinct error types), got %v", first["symbol"])
	}
	errs, _ := first["errors"].([]any)
	if len(errs) != 2 {
		t.Errorf("expected 2 errors, got %d", len(errs))
	}
}

func TestAnalyzeErrorSurface_PathPrefixFilter(t *testing.T) {
	srv, _ := setupTestServer(t)
	addThrowsEdge(srv.graph, "internal/in.go::A", "external::error", "internal/in.go", 1)
	addThrowsEdge(srv.graph, "cmd/out.go::B", "external::error", "cmd/out.go", 1)

	out := callAnalyzeErrorSurface(t, srv, map[string]any{
		"path_prefix": "internal/",
	})
	rows, _ := out["throwers"].([]any)
	if len(rows) != 1 {
		t.Fatalf("expected 1 thrower under internal/, got %d", len(rows))
	}
}

func TestAnalyzeErrorSurface_DedupesErrorTargets(t *testing.T) {
	// Two throws to the same error type from one function should
	// surface a single distinct error in the row but throws=2.
	srv, _ := setupTestServer(t)
	addThrowsEdge(srv.graph, "f.go::F", "external::error", "f.go", 1)
	addThrowsEdge(srv.graph, "f.go::F", "external::error", "f.go", 5)

	out := callAnalyzeErrorSurface(t, srv, map[string]any{})
	rows, _ := out["throwers"].([]any)
	row := rows[0].(map[string]any)
	if got, _ := row["throws"].(float64); got != 2 {
		t.Errorf("throws = %v, want 2", got)
	}
	errs, _ := row["errors"].([]any)
	if len(errs) != 1 {
		t.Errorf("expected 1 distinct error, got %d", len(errs))
	}
}
