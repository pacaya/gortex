package mcp

import (
	"context"
	"encoding/json"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/zzet/gortex/internal/graph"
)

func callAnalyzeEventEmitters(t *testing.T, srv *Server, args map[string]any) map[string]any {
	t.Helper()
	args["kind"] = "event_emitters"
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

func addEventNode(g graph.Store, id, name, kind string) {
	g.AddNode(&graph.Node{
		ID:   id,
		Kind: graph.KindEvent,
		Name: name,
		Meta: map[string]any{"event_kind": kind, "name": name},
	})
}

func addEmitsEdge(g graph.Store, from, to, method string) {
	e := &graph.Edge{From: from, To: to, Kind: graph.EdgeEmits}
	if method != "" {
		e.Meta = map[string]any{"method": method}
	}
	g.AddEdge(e)
}

func TestAnalyzeEventEmitters_GroupsByEvent(t *testing.T) {
	srv, _ := setupTestServer(t)
	addEventNode(srv.graph, "event::user.login", "user.login", "log")
	addEventNode(srv.graph, "event::request.error", "request.error", "log")
	addEmitsEdge(srv.graph, "f.go::Auth", "event::user.login", "Info")
	addEmitsEdge(srv.graph, "f.go::Track", "event::user.login", "Info")
	addEmitsEdge(srv.graph, "f.go::Handle", "event::request.error", "Errorf")

	out := callAnalyzeEventEmitters(t, srv, map[string]any{})
	rows, _ := out["events"].([]any)
	if len(rows) != 2 {
		t.Fatalf("expected 2 events, got %d", len(rows))
	}
	first := rows[0].(map[string]any)
	if first["name"] != "user.login" {
		t.Errorf("expected user.login first (more emits), got %v", first["name"])
	}
}

func TestAnalyzeEventEmitters_LevelFilterByMethod(t *testing.T) {
	srv, _ := setupTestServer(t)
	addEventNode(srv.graph, "event::a", "a", "log")
	addEventNode(srv.graph, "event::b", "b", "log")
	addEmitsEdge(srv.graph, "f.go::A", "event::a", "Errorf")
	addEmitsEdge(srv.graph, "f.go::B", "event::b", "Info")

	out := callAnalyzeEventEmitters(t, srv, map[string]any{
		"level": "error",
	})
	rows, _ := out["events"].([]any)
	if len(rows) != 1 {
		t.Fatalf("expected 1 event after level=error filter, got %d: %+v", len(rows), rows)
	}
}

func TestAnalyzeEventEmitters_NameFilter(t *testing.T) {
	srv, _ := setupTestServer(t)
	addEventNode(srv.graph, "event::a", "a", "log")
	addEventNode(srv.graph, "event::b", "b", "log")
	addEmitsEdge(srv.graph, "f.go::A", "event::a", "Info")
	addEmitsEdge(srv.graph, "f.go::B", "event::b", "Info")

	out := callAnalyzeEventEmitters(t, srv, map[string]any{
		"name": "a",
	})
	if got, _ := out["total"].(float64); got != 1 {
		t.Errorf("expected 1 event after name filter, got %v", got)
	}
}
