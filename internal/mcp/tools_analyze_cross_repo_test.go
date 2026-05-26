package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/zzet/gortex/internal/graph"
)

func callAnalyzeCrossRepo(t *testing.T, srv *Server, args map[string]any) map[string]any {
	t.Helper()
	args["kind"] = "cross_repo"
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

// seedCrossRepoGraph wires three repos with a handful of cross-repo
// edges so the analyzer has something to group.
func seedCrossRepoGraph(g graph.Store) {
	add := func(id, repo string) {
		g.AddNode(&graph.Node{ID: id, Kind: graph.KindFunction, Name: id, FilePath: id, RepoPrefix: repo})
	}
	add("repoA/a.go::A1", "repoA")
	add("repoA/a.go::A2", "repoA")
	add("repoB/b.go::B1", "repoB")
	add("repoC/c.go::C1", "repoC")

	edge := func(from, to string, kind graph.EdgeKind, file string) {
		g.AddEdge(&graph.Edge{From: from, To: to, Kind: kind, FilePath: file, Line: 1})
	}
	// repoA -> repoB calls (x2), one repoA -> repoC call, one repoA -> repoB implements.
	edge("repoA/a.go::A1", "repoB/b.go::B1", graph.EdgeCrossRepoCalls, "repoA/a.go")
	edge("repoA/a.go::A2", "repoB/b.go::B1", graph.EdgeCrossRepoCalls, "repoA/a.go")
	edge("repoA/a.go::A1", "repoC/c.go::C1", graph.EdgeCrossRepoCalls, "repoA/a.go")
	edge("repoA/a.go::A1", "repoB/b.go::B1", graph.EdgeCrossRepoImplements, "repoA/a.go")
}

func TestAnalyzeCrossRepo_GroupsByRepoPairAndKind(t *testing.T) {
	srv, _ := setupTestServer(t)
	seedCrossRepoGraph(srv.graph)

	out := callAnalyzeCrossRepo(t, srv, map[string]any{})
	rows, _ := out["dependencies"].([]any)
	// (repoA->repoB, calls), (repoA->repoC, calls), (repoA->repoB, implements)
	if len(rows) != 3 {
		t.Fatalf("expected 3 dependency rows, got %d: %+v", len(rows), rows)
	}
	// Highest-count row first: repoA->repoB calls = 2.
	top := rows[0].(map[string]any)
	if top["from_repo"] != "repoA" || top["to_repo"] != "repoB" || top["kind"] != "calls" {
		t.Errorf("unexpected top row: %+v", top)
	}
	if c, _ := top["count"].(float64); c != 2 {
		t.Errorf("top count = %v, want 2", c)
	}
}

func TestAnalyzeCrossRepo_RepoFilterMatchesEitherSide(t *testing.T) {
	srv, _ := setupTestServer(t)
	seedCrossRepoGraph(srv.graph)

	out := callAnalyzeCrossRepo(t, srv, map[string]any{"repo": "repoC"})
	rows, _ := out["dependencies"].([]any)
	if len(rows) != 1 {
		t.Fatalf("expected 1 row touching repoC, got %d: %+v", len(rows), rows)
	}
	row := rows[0].(map[string]any)
	if row["to_repo"] != "repoC" {
		t.Errorf("expected the repoC row, got %+v", row)
	}
}

func TestAnalyzeCrossRepo_BaseKindFilter(t *testing.T) {
	srv, _ := setupTestServer(t)
	seedCrossRepoGraph(srv.graph)

	out := callAnalyzeCrossRepo(t, srv, map[string]any{"base_kind": "implements"})
	rows, _ := out["dependencies"].([]any)
	if len(rows) != 1 {
		t.Fatalf("expected 1 implements row, got %d: %+v", len(rows), rows)
	}
	if rows[0].(map[string]any)["kind"] != "implements" {
		t.Errorf("expected implements row, got %+v", rows[0])
	}
}

func TestAnalyzeCrossRepo_PathPrefixFilter(t *testing.T) {
	srv, _ := setupTestServer(t)
	g := srv.graph
	g.AddNode(&graph.Node{ID: "repoA/in.go::A", Kind: graph.KindFunction, Name: "A", FilePath: "repoA/in.go", RepoPrefix: "repoA"})
	g.AddNode(&graph.Node{ID: "repoB/b.go::B", Kind: graph.KindFunction, Name: "B", FilePath: "repoB/b.go", RepoPrefix: "repoB"})
	g.AddEdge(&graph.Edge{From: "repoA/in.go::A", To: "repoB/b.go::B", Kind: graph.EdgeCrossRepoCalls, FilePath: "repoA/internal/in.go", Line: 1})
	g.AddEdge(&graph.Edge{From: "repoA/in.go::A", To: "repoB/b.go::B", Kind: graph.EdgeCrossRepoImplements, FilePath: "repoA/cmd/in.go", Line: 1})

	out := callAnalyzeCrossRepo(t, srv, map[string]any{"path_prefix": "repoA/internal/"})
	if got, _ := out["total"].(float64); got != 1 {
		t.Errorf("total = %v, want 1 (path_prefix should drop cmd/)", got)
	}
}

func TestAnalyzeCrossRepo_Empty(t *testing.T) {
	srv, _ := setupTestServer(t)
	out := callAnalyzeCrossRepo(t, srv, map[string]any{})
	if got, _ := out["total"].(float64); got != 0 {
		t.Errorf("total = %v, want 0 on an empty graph", got)
	}
}

func TestAnalyzeCrossRepo_GCXFormat(t *testing.T) {
	srv, _ := setupTestServer(t)
	seedCrossRepoGraph(srv.graph)

	req := mcplib.CallToolRequest{}
	req.Params.Name = "analyze"
	req.Params.Arguments = map[string]any{"kind": "cross_repo", "format": "gcx"}
	res, err := srv.handleAnalyze(context.Background(), req)
	if err != nil {
		t.Fatalf("handleAnalyze: %v", err)
	}
	if res.IsError {
		t.Fatalf("error: %+v", res.Content)
	}
	text := res.Content[0].(mcplib.TextContent).Text
	if !strings.Contains(text, "analyze.cross_repo") {
		t.Errorf("expected GCX1 analyze.cross_repo header, got:\n%s", text)
	}
	if !strings.Contains(text, "repoA") || !strings.Contains(text, "repoB") {
		t.Errorf("expected repo names in GCX rows, got:\n%s", text)
	}
}
