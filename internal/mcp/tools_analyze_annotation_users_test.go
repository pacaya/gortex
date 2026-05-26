package mcp

import (
	"context"
	"encoding/json"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/zzet/gortex/internal/graph"
)

func callAnalyzeAnnotationUsers(t *testing.T, srv *Server, args map[string]any) map[string]any {
	t.Helper()
	args["kind"] = "annotation_users"
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

func addAnnotationNode(g graph.Store, id, name string) {
	g.AddNode(&graph.Node{
		ID:   id,
		Kind: graph.KindType,
		Name: name,
		Meta: map[string]any{"kind": "annotation", "synthetic": true},
	})
}

func addAnnotatedEdge(g graph.Store, from, to, args string) {
	e := &graph.Edge{From: from, To: to, Kind: graph.EdgeAnnotated, FilePath: "x.go", Line: 1}
	if args != "" {
		e.Meta = map[string]any{"args": args}
	}
	g.AddEdge(e)
}

func TestAnalyzeAnnotationUsers_NoIDListsAllAnnotations(t *testing.T) {
	srv, _ := setupTestServer(t)
	addAnnotationNode(srv.graph, "annotation::java::Deprecated", "Deprecated")
	addAnnotationNode(srv.graph, "annotation::java::Override", "Override")
	addAnnotatedEdge(srv.graph, "Foo.java::A", "annotation::java::Deprecated", "")
	addAnnotatedEdge(srv.graph, "Foo.java::B", "annotation::java::Deprecated", "")
	addAnnotatedEdge(srv.graph, "Foo.java::C", "annotation::java::Override", "")

	out := callAnalyzeAnnotationUsers(t, srv, map[string]any{})
	rows, _ := out["annotations"].([]any)
	if len(rows) != 2 {
		t.Fatalf("expected 2 annotations grouped, got %d", len(rows))
	}
	first := rows[0].(map[string]any)
	if first["name"] != "Deprecated" {
		t.Errorf("expected Deprecated first (more users), got %v", first["name"])
	}
}

func TestAnalyzeAnnotationUsers_ScopedToOneAnnotationByID(t *testing.T) {
	srv, _ := setupTestServer(t)
	addAnnotationNode(srv.graph, "annotation::java::Deprecated", "Deprecated")
	addAnnotatedEdge(srv.graph, "Foo.java::A", "annotation::java::Deprecated", "since=1.4")
	addAnnotatedEdge(srv.graph, "Foo.java::B", "annotation::java::Deprecated", "")

	out := callAnalyzeAnnotationUsers(t, srv, map[string]any{
		"id": "annotation::java::Deprecated",
	})
	users, _ := out["users"].([]any)
	if len(users) != 2 {
		t.Fatalf("expected 2 users, got %d", len(users))
	}
	// At least one row should carry the args meta so callers can
	// distinguish parameterised from bare annotations.
	hasArgs := false
	for _, raw := range users {
		row := raw.(map[string]any)
		if a, _ := row["args"].(string); a == "since=1.4" {
			hasArgs = true
		}
	}
	if !hasArgs {
		t.Fatalf("expected args meta to surface on at least one user row, got %+v", users)
	}
}

func TestAnalyzeAnnotationUsers_NameFilter(t *testing.T) {
	srv, _ := setupTestServer(t)
	addAnnotationNode(srv.graph, "annotation::java::Deprecated", "Deprecated")
	addAnnotationNode(srv.graph, "annotation::java::Override", "Override")
	addAnnotatedEdge(srv.graph, "Foo.java::A", "annotation::java::Deprecated", "")
	addAnnotatedEdge(srv.graph, "Foo.java::B", "annotation::java::Override", "")

	out := callAnalyzeAnnotationUsers(t, srv, map[string]any{
		"name": "deprecated", // case-insensitive
	})
	if got, _ := out["total"].(float64); got != 1 {
		t.Errorf("expected 1 annotation matched by name, got %v", got)
	}
}
