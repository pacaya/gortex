package mcp

import (
	"context"
	"encoding/json"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/zzet/gortex/internal/graph"
)

func callAnalyzeConfigReaders(t *testing.T, srv *Server, args map[string]any) map[string]any {
	t.Helper()
	args["kind"] = "config_readers"
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

func addConfigKeyNode(g graph.Store, id, name, source string) {
	g.AddNode(&graph.Node{
		ID:   id,
		Kind: graph.KindConfigKey,
		Name: name,
		Meta: map[string]any{"key": name, "source": source},
	})
}

func addReadConfigEdge(g graph.Store, from, to string) {
	g.AddEdge(&graph.Edge{From: from, To: to, Kind: graph.EdgeReadsConfig})
}

func TestAnalyzeConfigReaders_GroupsByKey(t *testing.T) {
	srv, _ := setupTestServer(t)
	addConfigKeyNode(srv.graph, "cfg::env::DATABASE_URL", "DATABASE_URL", "env")
	addConfigKeyNode(srv.graph, "cfg::env::PORT", "PORT", "env")
	addReadConfigEdge(srv.graph, "f.go::A", "cfg::env::DATABASE_URL")
	addReadConfigEdge(srv.graph, "f.go::B", "cfg::env::DATABASE_URL")
	addReadConfigEdge(srv.graph, "f.go::C", "cfg::env::PORT")

	out := callAnalyzeConfigReaders(t, srv, map[string]any{})
	rows, _ := out["config_keys"].([]any)
	if len(rows) != 2 {
		t.Fatalf("expected 2 config keys, got %d", len(rows))
	}
	first := rows[0].(map[string]any)
	if first["name"] != "DATABASE_URL" {
		t.Errorf("expected DATABASE_URL first (more reads), got %v", first["name"])
	}
}

func TestAnalyzeConfigReaders_NameFilter(t *testing.T) {
	srv, _ := setupTestServer(t)
	addConfigKeyNode(srv.graph, "cfg::env::DATABASE_URL", "DATABASE_URL", "env")
	addConfigKeyNode(srv.graph, "cfg::env::PORT", "PORT", "env")
	addReadConfigEdge(srv.graph, "f.go::A", "cfg::env::DATABASE_URL")
	addReadConfigEdge(srv.graph, "f.go::B", "cfg::env::PORT")

	out := callAnalyzeConfigReaders(t, srv, map[string]any{
		"name": "database_url",
	})
	if got, _ := out["total"].(float64); got != 1 {
		t.Errorf("expected 1 key after name filter, got %v", got)
	}
}

func TestAnalyzeConfigReaders_LimitTruncates(t *testing.T) {
	srv, _ := setupTestServer(t)
	for i := 0; i < 5; i++ {
		id := "cfg::env::K" + string(rune('A'+i))
		addConfigKeyNode(srv.graph, id, "K"+string(rune('A'+i)), "env")
		addReadConfigEdge(srv.graph, "f.go::R", id)
	}
	out := callAnalyzeConfigReaders(t, srv, map[string]any{
		"limit": float64(2),
	})
	if got, _ := out["truncated"].(bool); !got {
		t.Errorf("expected truncated=true with limit=2 over 5 keys")
	}
}
