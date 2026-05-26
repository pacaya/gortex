package mcp

import (
	"context"
	"encoding/json"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/zzet/gortex/internal/graph"
)

// addTodoNode is a small helper for these tests — wires a KindTodo
// node directly into the graph without going through the indexer's
// per-file pipeline.
func addTodoNode(g graph.Store, id string, line int, meta map[string]any) {
	g.AddNode(&graph.Node{
		ID:        id,
		Kind:      graph.KindTodo,
		Name:      "todo:" + id,
		FilePath:  "pkg/foo.go",
		StartLine: line,
		EndLine:   line,
		Language:  "go",
		Meta:      meta,
	})
}

// callAnalyzeTodos drives the handler with the given args and
// returns the parsed JSON payload — matches how the real MCP
// transport surfaces the tool's output.
func callAnalyzeTodos(t *testing.T, srv *Server, args map[string]any) map[string]any {
	t.Helper()
	args["kind"] = "todos"
	req := mcplib.CallToolRequest{}
	req.Params.Name = "analyze"
	req.Params.Arguments = args

	res, err := srv.handleAnalyze(context.Background(), req)
	if err != nil {
		t.Fatalf("handleAnalyze: %v", err)
	}
	if res.IsError {
		t.Fatalf("handler returned error: %+v", res.Content)
	}
	if len(res.Content) == 0 {
		t.Fatal("empty result content")
	}
	textBlock, ok := res.Content[0].(mcplib.TextContent)
	if !ok {
		t.Fatalf("expected TextContent, got %T", res.Content[0])
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(textBlock.Text), &out); err != nil {
		t.Fatalf("json: %v\n%s", err, textBlock.Text)
	}
	return out
}

func TestAnalyzeTodos_NoFilter(t *testing.T) {
	srv, _ := setupTestServer(t)
	addTodoNode(srv.graph, "pkg/foo.go::todo:10", 10,
		map[string]any{"tag": "TODO", "text": "fix it"})
	addTodoNode(srv.graph, "pkg/foo.go::todo:20", 20,
		map[string]any{"tag": "FIXME", "text": "broken", "assignee": "alice"})

	out := callAnalyzeTodos(t, srv, map[string]any{})
	rows, _ := out["todos"].([]any)
	if len(rows) != 2 {
		t.Errorf("expected 2 todos, got %d: %+v", len(rows), out)
	}
	if total, _ := out["total"].(float64); int(total) != 2 {
		t.Errorf("total = %v", total)
	}
}

func TestAnalyzeTodos_TagFilter(t *testing.T) {
	srv, _ := setupTestServer(t)
	addTodoNode(srv.graph, "f::a", 1, map[string]any{"tag": "TODO"})
	addTodoNode(srv.graph, "f::b", 2, map[string]any{"tag": "FIXME"})
	addTodoNode(srv.graph, "f::c", 3, map[string]any{"tag": "TODO"})

	out := callAnalyzeTodos(t, srv, map[string]any{"tag": "TODO"})
	rows, _ := out["todos"].([]any)
	if len(rows) != 2 {
		t.Errorf("expected 2 TODO rows, got %d", len(rows))
	}
}

func TestAnalyzeTodos_AssigneeFilter(t *testing.T) {
	srv, _ := setupTestServer(t)
	addTodoNode(srv.graph, "f::a", 1,
		map[string]any{"tag": "TODO", "assignee": "alice"})
	addTodoNode(srv.graph, "f::b", 2,
		map[string]any{"tag": "TODO", "assignee": "bob"})
	addTodoNode(srv.graph, "f::c", 3,
		map[string]any{"tag": "TODO"})

	out := callAnalyzeTodos(t, srv, map[string]any{"assignee": "alice"})
	rows, _ := out["todos"].([]any)
	if len(rows) != 1 {
		t.Errorf("expected 1 alice row, got %d", len(rows))
	}
}

func TestAnalyzeTodos_HasAssigneeFilter(t *testing.T) {
	srv, _ := setupTestServer(t)
	addTodoNode(srv.graph, "f::a", 1,
		map[string]any{"tag": "TODO", "assignee": "alice"})
	addTodoNode(srv.graph, "f::b", 2,
		map[string]any{"tag": "TODO"}) // no assignee

	out := callAnalyzeTodos(t, srv, map[string]any{"has_assignee": true})
	rows, _ := out["todos"].([]any)
	if len(rows) != 1 {
		t.Errorf("expected 1 row with assignee, got %d", len(rows))
	}
}

func TestAnalyzeTodos_TicketFilter(t *testing.T) {
	srv, _ := setupTestServer(t)
	addTodoNode(srv.graph, "f::a", 1,
		map[string]any{"tag": "TODO", "ticket": "PROJ-42"})
	addTodoNode(srv.graph, "f::b", 2,
		map[string]any{"tag": "TODO", "ticket": "PROJ-99"})

	out := callAnalyzeTodos(t, srv, map[string]any{"ticket": "PROJ-42"})
	rows, _ := out["todos"].([]any)
	if len(rows) != 1 {
		t.Errorf("expected 1 PROJ-42 row, got %d", len(rows))
	}
}

func TestAnalyzeTodos_StableSortByFileLine(t *testing.T) {
	srv, _ := setupTestServer(t)
	// Insertion order intentionally jumbled.
	srv.graph.AddNode(&graph.Node{
		ID: "x", Kind: graph.KindTodo, FilePath: "b/y.go", StartLine: 5,
		Meta: map[string]any{"tag": "TODO"},
	})
	srv.graph.AddNode(&graph.Node{
		ID: "y", Kind: graph.KindTodo, FilePath: "a/x.go", StartLine: 10,
		Meta: map[string]any{"tag": "TODO"},
	})
	srv.graph.AddNode(&graph.Node{
		ID: "z", Kind: graph.KindTodo, FilePath: "a/x.go", StartLine: 3,
		Meta: map[string]any{"tag": "TODO"},
	})

	out := callAnalyzeTodos(t, srv, map[string]any{})
	rows, _ := out["todos"].([]any)
	if len(rows) != 3 {
		t.Fatalf("expected 3 rows, got %d", len(rows))
	}
	// Expected order: a/x.go:3, a/x.go:10, b/y.go:5
	files := make([]string, 0, len(rows))
	for _, r := range rows {
		m, _ := r.(map[string]any)
		files = append(files, m["file"].(string))
	}
	if files[0] != "a/x.go" || files[1] != "a/x.go" || files[2] != "b/y.go" {
		t.Errorf("sort order wrong: %v", files)
	}
}
