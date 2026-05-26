package mcp

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/zzet/gortex/internal/graph"
)

func callAnalyzeOwnership(t *testing.T, srv *Server, args map[string]any) map[string]any {
	t.Helper()
	args["kind"] = "ownership"
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

// addBlameNode wires a function node with synthetic last_authored
// meta keyed off email + timestamp.
func addBlameNode(g graph.Store, id, file, email string, ts int64) {
	g.AddNode(&graph.Node{
		ID:        id,
		Kind:      graph.KindFunction,
		Name:      id,
		FilePath:  file,
		StartLine: 1,
		EndLine:   1,
		Meta: map[string]any{
			"last_authored": map[string]any{
				"email":     email,
				"timestamp": ts,
			},
		},
	})
}

func TestAnalyzeOwnership_GroupsByEmail(t *testing.T) {
	srv, _ := setupTestServer(t)
	now := time.Now().Unix()
	addBlameNode(srv.graph, "f.go::A", "f.go", "alice@x", now-100)
	addBlameNode(srv.graph, "f.go::B", "f.go", "alice@x", now-200)
	addBlameNode(srv.graph, "g.go::C", "g.go", "alice@x", now-300)
	addBlameNode(srv.graph, "g.go::D", "g.go", "bob@x", now-400)

	out := callAnalyzeOwnership(t, srv, map[string]any{})
	owners, _ := out["owners"].([]any)
	if len(owners) != 2 {
		t.Fatalf("expected 2 owners, got %d", len(owners))
	}
	// alice has 3 symbols across 2 files → first row.
	first, _ := owners[0].(map[string]any)
	if first["email"] != "alice@x" {
		t.Errorf("expected alice first (most symbols), got %v", first["email"])
	}
	if got, _ := first["symbols"].(float64); got != 3 {
		t.Errorf("alice symbols = %v, want 3", got)
	}
	if got, _ := first["files"].(float64); got != 2 {
		t.Errorf("alice files = %v, want 2", got)
	}
}

func TestAnalyzeOwnership_MinSymbolsFilter(t *testing.T) {
	srv, _ := setupTestServer(t)
	now := time.Now().Unix()
	addBlameNode(srv.graph, "f.go::A", "f.go", "alice@x", now)
	addBlameNode(srv.graph, "f.go::B", "f.go", "alice@x", now)
	addBlameNode(srv.graph, "f.go::C", "f.go", "carol@x", now) // single

	out := callAnalyzeOwnership(t, srv, map[string]any{"min_symbols": 2.0})
	owners, _ := out["owners"].([]any)
	if len(owners) != 1 {
		t.Errorf("expected only alice (>=2 symbols), got %d", len(owners))
	}
}

func TestAnalyzeOwnership_PathPrefixFilter(t *testing.T) {
	srv, _ := setupTestServer(t)
	now := time.Now().Unix()
	addBlameNode(srv.graph, "internal/auth/jwt.go::Verify", "internal/auth/jwt.go", "alice@x", now)
	addBlameNode(srv.graph, "internal/db/store.go::Save", "internal/db/store.go", "bob@x", now)

	out := callAnalyzeOwnership(t, srv, map[string]any{
		"path_prefix": "internal/auth/",
	})
	owners, _ := out["owners"].([]any)
	if len(owners) != 1 {
		t.Errorf("expected only alice (auth scope), got %d", len(owners))
	}
	first, _ := owners[0].(map[string]any)
	if first["email"] != "alice@x" {
		t.Errorf("got %v", first["email"])
	}
}

func TestAnalyzeOwnership_SkipsUnenriched(t *testing.T) {
	srv, _ := setupTestServer(t)
	addBlameNode(srv.graph, "f.go::A", "f.go", "alice@x", time.Now().Unix())
	srv.graph.AddNode(&graph.Node{
		ID:       "f.go::B",
		Kind:     graph.KindFunction,
		Name:     "B",
		FilePath: "f.go",
	})

	out := callAnalyzeOwnership(t, srv, map[string]any{})
	owners, _ := out["owners"].([]any)
	if len(owners) != 1 {
		t.Errorf("unenriched node should be skipped, got %d owners", len(owners))
	}
}

func TestAnalyzeOwnership_OldestNewestSpan(t *testing.T) {
	srv, _ := setupTestServer(t)
	addBlameNode(srv.graph, "f.go::A", "f.go", "alice@x", 100)
	addBlameNode(srv.graph, "f.go::B", "f.go", "alice@x", 500)
	addBlameNode(srv.graph, "f.go::C", "f.go", "alice@x", 300)

	out := callAnalyzeOwnership(t, srv, map[string]any{})
	owners, _ := out["owners"].([]any)
	first := owners[0].(map[string]any)
	if got, _ := first["oldest_timestamp"].(float64); int64(got) != 100 {
		t.Errorf("oldest = %v, want 100", got)
	}
	if got, _ := first["newest_timestamp"].(float64); int64(got) != 500 {
		t.Errorf("newest = %v, want 500", got)
	}
}
