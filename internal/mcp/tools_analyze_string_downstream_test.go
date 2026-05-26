package mcp

import (
	"context"
	"encoding/json"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/zzet/gortex/internal/graph"
)

func callAnalyze(t *testing.T, srv *Server, kind string, extra map[string]any) map[string]any {
	t.Helper()
	args := map[string]any{"kind": kind}
	for k, v := range extra {
		args[k] = v
	}
	req := mcplib.CallToolRequest{}
	req.Params.Name = "analyze"
	req.Params.Arguments = args
	res, err := srv.handleAnalyze(context.Background(), req)
	if err != nil {
		t.Fatalf("handleAnalyze(%s): %v", kind, err)
	}
	if res.IsError {
		t.Fatalf("analyze(%s) returned error: %+v", kind, res.Content)
	}
	textBlock := res.Content[0].(mcplib.TextContent)
	var out map[string]any
	if err := json.Unmarshal([]byte(textBlock.Text), &out); err != nil {
		t.Fatalf("analyze(%s) json: %v\n%s", kind, err, textBlock.Text)
	}
	return out
}

// addEmitToKindString builds a (caller, KindString) emit pair with
// the given context and meta. Used by the registry-downstream
// analyzers' tests.
func addEmitToKindString(g graph.Store, caller, strID, value, ctx string, nodeMeta, edgeMeta map[string]any) {
	meta := map[string]any{
		"context": ctx,
		"value":   value,
	}
	for k, v := range nodeMeta {
		meta[k] = v
	}
	g.AddNode(&graph.Node{
		ID:       strID,
		Kind:     graph.KindString,
		Name:     value,
		Language: "go",
		Meta:     meta,
	})
	em := map[string]any{"context": ctx}
	for k, v := range edgeMeta {
		em[k] = v
	}
	g.AddEdge(&graph.Edge{
		From: caller,
		To:   strID,
		Kind: graph.EdgeEmits,
		Meta: em,
	})
}

// analyze kind=log_events aggregates EdgeEmits → KindString
// context="log_message" by literal value.
func TestAnalyzeLogEvents_AggregatesByValue(t *testing.T) {
	srv, _ := setupTestServer(t)
	g := srv.graph
	addEmitToKindString(g, "f.go::A", "string::log_message::user.signup", "user.signup", "log_message",
		map[string]any{"level": "log", "event": "event::log::user.signup"},
		map[string]any{"method": "Info", "level": "log"})
	addEmitToKindString(g, "f.go::B", "string::log_message::user.signup", "user.signup", "log_message",
		map[string]any{"level": "log"},
		map[string]any{"method": "Info", "level": "log"})

	out := callAnalyze(t, srv, "log_events", nil)
	events, _ := out["events"].([]any)
	if len(events) != 1 {
		t.Fatalf("expected 1 log_event row, got %d", len(events))
	}
	row := events[0].(map[string]any)
	if row["value"] != "user.signup" {
		t.Errorf("value = %v", row["value"])
	}
	if emits, _ := row["emits"].(float64); emits != 2 {
		t.Errorf("emits = %v, want 2", emits)
	}
	if level, _ := row["level"].(string); level != "log" {
		t.Errorf("level = %q", level)
	}
}

func TestAnalyzeLogEvents_IgnoresNonLogContexts(t *testing.T) {
	srv, _ := setupTestServer(t)
	g := srv.graph
	addEmitToKindString(g, "f.go::A", "string::error_msg::bad", "bad", "error_msg", nil, nil)

	out := callAnalyze(t, srv, "log_events", nil)
	events, _ := out["events"].([]any)
	if len(events) != 0 {
		t.Errorf("expected 0 rows (no log_message strings), got %d", len(events))
	}
}

func TestAnalyzeLogEvents_LevelFilter(t *testing.T) {
	srv, _ := setupTestServer(t)
	g := srv.graph
	addEmitToKindString(g, "f.go::A", "string::log_message::user.signup", "user.signup", "log_message",
		nil, map[string]any{"method": "Info", "level": "info"})
	addEmitToKindString(g, "f.go::B", "string::log_message::payment.failed", "payment.failed", "log_message",
		nil, map[string]any{"method": "Error", "level": "error"})

	out := callAnalyze(t, srv, "log_events", map[string]any{"level": "error"})
	events, _ := out["events"].([]any)
	if len(events) != 1 {
		t.Fatalf("expected 1 row for level=error, got %d", len(events))
	}
	row := events[0].(map[string]any)
	if row["value"] != "payment.failed" {
		t.Errorf("value = %v", row["value"])
	}
}

// analyze kind=sql_rebuild walks KindString context="sql" and
// derives KindTable / EdgeQueries.
func TestAnalyzeSQLRebuild_DerivesTables(t *testing.T) {
	srv, _ := setupTestServer(t)
	g := srv.graph
	g.AddNode(&graph.Node{ID: "f.go::Run", Kind: graph.KindFunction, Name: "Run"})
	addEmitToKindString(g, "f.go::Run", "string::sql::SELECT id FROM users", "SELECT id FROM users", "sql",
		map[string]any{"dialect": "postgres"},
		map[string]any{"dialect": "postgres"})

	out := callAnalyze(t, srv, "sql_rebuild", nil)
	if got, _ := out["strings_visited"].(float64); got != 1 {
		t.Errorf("strings_visited = %v, want 1", got)
	}
	if got, _ := out["tables_created"].(float64); got != 1 {
		t.Errorf("tables_created = %v, want 1", got)
	}
	if got, _ := out["query_edges_created"].(float64); got != 1 {
		t.Errorf("query_edges_created = %v, want 1", got)
	}
	// Verify the table actually landed in the graph.
	if n := g.GetNode("db::postgres::users"); n == nil {
		t.Errorf("expected db::postgres::users node to be created")
	}
}

// error_surface now includes error_msg KindString literals.
func TestAnalyzeErrorSurface_IncludesErrorMsgRegistry(t *testing.T) {
	srv, _ := setupTestServer(t)
	g := srv.graph
	addThrowsEdge(g, "f.go::DoThing", "external::error", "f.go", 10)
	addEmitToKindString(g, "f.go::DoThing", "string::error_msg::bad token",
		"bad token", "error_msg", nil, nil)
	addEmitToKindString(g, "f.go::DoThing", "string::error_msg::unauthorized",
		"unauthorized", "error_msg", nil, nil)
	// A non-error_msg KindString shouldn't leak into the registry.
	addEmitToKindString(g, "f.go::DoThing", "string::metric::api.calls",
		"api.calls", "metric", nil, nil)

	out := callAnalyzeErrorSurface(t, srv, map[string]any{})
	throwers, _ := out["throwers"].([]any)
	if len(throwers) != 1 {
		t.Fatalf("expected 1 thrower, got %d", len(throwers))
	}
	row := throwers[0].(map[string]any)
	msgs, _ := row["error_msgs"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("expected 2 error_msgs, got %d (%v)", len(msgs), msgs)
	}
	// Sorted alphabetically.
	if msgs[0] != "bad token" || msgs[1] != "unauthorized" {
		t.Errorf("error_msgs = %v, want [bad token, unauthorized] sorted", msgs)
	}
}

func TestAnalyzeErrorSurface_ErrorMsgsOmittedWhenEmpty(t *testing.T) {
	srv, _ := setupTestServer(t)
	g := srv.graph
	addThrowsEdge(g, "f.go::DoThing", "external::error", "f.go", 10)

	out := callAnalyzeErrorSurface(t, srv, map[string]any{})
	throwers, _ := out["throwers"].([]any)
	if len(throwers) != 1 {
		t.Fatalf("expected 1 thrower, got %d", len(throwers))
	}
	row := throwers[0].(map[string]any)
	if _, present := row["error_msgs"]; present {
		t.Errorf("error_msgs should be omitted when empty, got %v", row["error_msgs"])
	}
}
