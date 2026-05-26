package mcp

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/zzet/gortex/internal/graph"
)

// addBlameEnrichedNode wires a function node with synthetic
// last_authored meta — emulating what blame.EnrichGraph would have
// produced after a real run.
func addBlameEnrichedNode(g graph.Store, id, file string, line int, email, commit string, ageDays int) {
	ts := time.Now().Add(-time.Duration(ageDays*24) * time.Hour).Unix()
	g.AddNode(&graph.Node{
		ID:        id,
		Kind:      graph.KindFunction,
		Name:      id,
		FilePath:  file,
		StartLine: line,
		EndLine:   line + 1,
		Meta: map[string]any{
			"last_authored": map[string]any{
				"email":     email,
				"commit":    commit,
				"timestamp": ts,
			},
		},
	})
}

func callAnalyzeStaleCode(t *testing.T, srv *Server, args map[string]any) map[string]any {
	t.Helper()
	args["kind"] = "stale_code"
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

func TestAnalyzeStaleCode_DefaultThreshold(t *testing.T) {
	srv, _ := setupTestServer(t)
	addBlameEnrichedNode(srv.graph, "f.go::Recent", "f.go", 1, "alice@x", "aaa", 30)
	addBlameEnrichedNode(srv.graph, "f.go::Stale", "f.go", 5, "bob@x", "bbb", 400)
	addBlameEnrichedNode(srv.graph, "f.go::Ancient", "f.go", 9, "alice@x", "ccc", 800)

	out := callAnalyzeStaleCode(t, srv, map[string]any{})
	rows, _ := out["stale"].([]any)
	if len(rows) != 2 {
		t.Errorf("expected 2 (Stale + Ancient, default 365 days), got %d", len(rows))
	}
	// Oldest first.
	first, _ := rows[0].(map[string]any)
	if first["id"] != "f.go::Ancient" {
		t.Errorf("expected oldest first, got %v", first["id"])
	}
}

func TestAnalyzeStaleCode_OlderThanCustom(t *testing.T) {
	srv, _ := setupTestServer(t)
	addBlameEnrichedNode(srv.graph, "f.go::Recent", "f.go", 1, "alice@x", "aaa", 30)
	addBlameEnrichedNode(srv.graph, "f.go::Stale", "f.go", 5, "bob@x", "bbb", 60)

	out := callAnalyzeStaleCode(t, srv, map[string]any{"older_than": 45.0})
	rows, _ := out["stale"].([]any)
	if len(rows) != 1 {
		t.Errorf("expected 1 (only Stale), got %d", len(rows))
	}
}

func TestAnalyzeStaleCode_EmailFilter(t *testing.T) {
	srv, _ := setupTestServer(t)
	addBlameEnrichedNode(srv.graph, "f.go::A", "f.go", 1, "alice@x", "aaa", 500)
	addBlameEnrichedNode(srv.graph, "f.go::B", "f.go", 5, "bob@x", "bbb", 500)
	addBlameEnrichedNode(srv.graph, "f.go::C", "f.go", 9, "alice@x", "ccc", 500)

	out := callAnalyzeStaleCode(t, srv, map[string]any{"email": "alice@x"})
	rows, _ := out["stale"].([]any)
	if len(rows) != 2 {
		t.Errorf("expected 2 alice rows, got %d", len(rows))
	}
}

func TestAnalyzeStaleCode_SkipsUnenrichedNodes(t *testing.T) {
	srv, _ := setupTestServer(t)
	addBlameEnrichedNode(srv.graph, "f.go::Stale", "f.go", 1, "alice@x", "aaa", 500)
	srv.graph.AddNode(&graph.Node{
		ID:       "f.go::NoBlame",
		Kind:     graph.KindFunction,
		Name:     "NoBlame",
		FilePath: "f.go",
	})

	out := callAnalyzeStaleCode(t, srv, map[string]any{})
	rows, _ := out["stale"].([]any)
	if len(rows) != 1 {
		t.Errorf("unenriched nodes should be skipped silently — got %d rows, want 1", len(rows))
	}
}

func TestAnalyzeStaleCode_KindsAll(t *testing.T) {
	srv, _ := setupTestServer(t)
	// Default kind list is function/method only — types are skipped.
	srv.graph.AddNode(&graph.Node{
		ID:       "f.go::T",
		Kind:     graph.KindType,
		Name:     "T",
		FilePath: "f.go",
		Meta: map[string]any{
			"last_authored": map[string]any{
				"email":     "alice@x",
				"timestamp": time.Now().Add(-500 * 24 * time.Hour).Unix(),
			},
		},
	})

	defaultOut := callAnalyzeStaleCode(t, srv, map[string]any{})
	if got, _ := defaultOut["total"].(float64); got != 0 {
		t.Errorf("default kinds should skip types, got total=%v", got)
	}

	allOut := callAnalyzeStaleCode(t, srv, map[string]any{"kinds": "all"})
	if got, _ := allOut["total"].(float64); got != 1 {
		t.Errorf("kinds=all should include types, got total=%v", got)
	}
}
