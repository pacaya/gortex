package mcp

import (
	"context"
	"encoding/json"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/zzet/gortex/internal/graph"
)

func callAnalyzeFieldWriters(t *testing.T, srv *Server, args map[string]any) map[string]any {
	t.Helper()
	args["kind"] = "field_writers"
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

func addFieldNode(g graph.Store, id, name string) {
	g.AddNode(&graph.Node{ID: id, Kind: graph.KindField, Name: name})
}

func addWriteEdge(g graph.Store, from, to string) {
	g.AddEdge(&graph.Edge{From: from, To: to, Kind: graph.EdgeWrites})
}

func TestAnalyzeFieldWriters_GroupsByTargetField(t *testing.T) {
	srv, _ := setupTestServer(t)
	addFieldNode(srv.graph, "f.go::Cfg.port", "port")
	addFieldNode(srv.graph, "f.go::Cfg.addr", "addr")
	addWriteEdge(srv.graph, "f.go::Set1", "f.go::Cfg.port")
	addWriteEdge(srv.graph, "f.go::Set2", "f.go::Cfg.port")
	addWriteEdge(srv.graph, "f.go::Set3", "f.go::Cfg.addr")

	out := callAnalyzeFieldWriters(t, srv, map[string]any{})
	rows, _ := out["fields"].([]any)
	if len(rows) != 2 {
		t.Fatalf("expected 2 fields, got %d", len(rows))
	}
	first := rows[0].(map[string]any)
	if first["field"] != "f.go::Cfg.port" {
		t.Errorf("expected port first (most writes), got %v", first["field"])
	}
}

func TestAnalyzeFieldWriters_ScopedToOneField(t *testing.T) {
	srv, _ := setupTestServer(t)
	addFieldNode(srv.graph, "f.go::Cfg.port", "port")
	addFieldNode(srv.graph, "f.go::Cfg.addr", "addr")
	addWriteEdge(srv.graph, "f.go::Set1", "f.go::Cfg.port")
	addWriteEdge(srv.graph, "f.go::Set2", "f.go::Cfg.addr")

	out := callAnalyzeFieldWriters(t, srv, map[string]any{
		"id": "f.go::Cfg.port",
	})
	rows, _ := out["fields"].([]any)
	if len(rows) != 1 {
		t.Fatalf("expected 1 field, got %d", len(rows))
	}
}

func TestAnalyzeFieldWriters_SkipsUnresolvedTargets(t *testing.T) {
	// Without an `id` filter the analyzer should ignore writes that
	// haven't been resolved onto a real field node — we don't want
	// to surface `unresolved::*.foo` rows in the rollup.
	srv, _ := setupTestServer(t)
	addFieldNode(srv.graph, "f.go::Cfg.port", "port")
	addWriteEdge(srv.graph, "f.go::Set1", "f.go::Cfg.port")
	addWriteEdge(srv.graph, "f.go::SetX", "unresolved::*.unknown")

	out := callAnalyzeFieldWriters(t, srv, map[string]any{})
	rows, _ := out["fields"].([]any)
	if len(rows) != 1 {
		t.Fatalf("expected only the resolved field to surface, got %d rows: %+v", len(rows), rows)
	}
}
