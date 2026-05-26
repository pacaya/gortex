package mcp

import (
	"context"
	"encoding/json"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/zzet/gortex/internal/graph"
)

func callAnalyzeExternalCalls(t *testing.T, srv *Server, args map[string]any) map[string]any {
	t.Helper()
	args["kind"] = "external_calls"
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

func addExternalModuleNode(g graph.Store, id, path, version, kind string) {
	g.AddNode(&graph.Node{
		ID:       id,
		Kind:     graph.KindModule,
		Name:     path,
		Language: "go",
		Meta: map[string]any{
			"path":        path,
			"version":     version,
			"module_kind": kind,
		},
	})
}

func addExternalSymbolNode(g graph.Store, id, name, importPath, moduleID string, kind graph.NodeKind) {
	g.AddNode(&graph.Node{
		ID:       id,
		Kind:     kind,
		Name:     name,
		Language: "go",
		Meta: map[string]any{
			"external":    true,
			"import_path": importPath,
			"module_id":   moduleID,
		},
	})
	g.AddEdge(&graph.Edge{
		From: id,
		To:   moduleID,
		Kind: graph.EdgeDependsOnModule,
	})
}

func addExternalCall(g graph.Store, from, to string) {
	g.AddEdge(&graph.Edge{
		From: from,
		To:   to,
		Kind: graph.EdgeCalls,
	})
}

func TestAnalyzeExternalCalls_GroupsByModule(t *testing.T) {
	srv, _ := setupTestServer(t)
	addExternalModuleNode(srv.graph, "module::go:stdlib", "stdlib", "", "stdlib")
	addExternalModuleNode(srv.graph, "module::go:github.com/foo/bar@v1.2.3", "github.com/foo/bar", "v1.2.3", "module_cache")

	addExternalSymbolNode(srv.graph, "ext::go:fmt::Println", "Println", "fmt",
		"module::go:stdlib", graph.KindFunction)
	addExternalSymbolNode(srv.graph, "ext::go:fmt::Sprintf", "Sprintf", "fmt",
		"module::go:stdlib", graph.KindFunction)
	addExternalSymbolNode(srv.graph, "ext::go:github.com/foo/bar::Do", "Do", "github.com/foo/bar",
		"module::go:github.com/foo/bar@v1.2.3", graph.KindFunction)

	addExternalCall(srv.graph, "main.go::A", "ext::go:fmt::Println")
	addExternalCall(srv.graph, "main.go::B", "ext::go:fmt::Println")
	addExternalCall(srv.graph, "main.go::C", "ext::go:fmt::Sprintf")
	addExternalCall(srv.graph, "main.go::D", "ext::go:github.com/foo/bar::Do")

	out := callAnalyzeExternalCalls(t, srv, map[string]any{})
	rows, _ := out["modules"].([]any)
	if len(rows) != 2 {
		t.Fatalf("expected 2 module rollups, got %d", len(rows))
	}
	first := rows[0].(map[string]any)
	if first["id"] != "module::go:stdlib" {
		t.Errorf("expected stdlib first (3 calls), got %v", first["id"])
	}
	if int(first["calls"].(float64)) != 3 {
		t.Errorf("expected 3 calls on stdlib, got %v", first["calls"])
	}
	if int(first["symbols"].(float64)) != 2 {
		t.Errorf("expected 2 symbols on stdlib, got %v", first["symbols"])
	}
	if first["module_kind"] != "stdlib" {
		t.Errorf("expected module_kind=stdlib, got %v", first["module_kind"])
	}
}

func TestAnalyzeExternalCalls_ModuleKindFilter(t *testing.T) {
	srv, _ := setupTestServer(t)
	addExternalModuleNode(srv.graph, "module::go:stdlib", "stdlib", "", "stdlib")
	addExternalModuleNode(srv.graph, "module::go:github.com/foo/bar@v1", "github.com/foo/bar", "v1", "module_cache")
	addExternalSymbolNode(srv.graph, "ext::go:fmt::Println", "Println", "fmt", "module::go:stdlib", graph.KindFunction)
	addExternalSymbolNode(srv.graph, "ext::go:github.com/foo/bar::X", "X", "github.com/foo/bar", "module::go:github.com/foo/bar@v1", graph.KindFunction)
	addExternalCall(srv.graph, "main.go::A", "ext::go:fmt::Println")
	addExternalCall(srv.graph, "main.go::B", "ext::go:github.com/foo/bar::X")

	out := callAnalyzeExternalCalls(t, srv, map[string]any{
		"module_kind": "module_cache",
	})
	rows, _ := out["modules"].([]any)
	if len(rows) != 1 {
		t.Fatalf("expected 1 module after module_kind=module_cache, got %d", len(rows))
	}
	first := rows[0].(map[string]any)
	if first["module_kind"] != "module_cache" {
		t.Errorf("filter leaked: %v", first["module_kind"])
	}
}

func TestAnalyzeExternalCalls_ModulePathFilter(t *testing.T) {
	srv, _ := setupTestServer(t)
	addExternalModuleNode(srv.graph, "module::go:stdlib", "stdlib", "", "stdlib")
	addExternalModuleNode(srv.graph, "module::go:github.com/foo/bar@v1", "github.com/foo/bar", "v1", "module_cache")
	addExternalSymbolNode(srv.graph, "ext::go:fmt::Println", "Println", "fmt", "module::go:stdlib", graph.KindFunction)
	addExternalSymbolNode(srv.graph, "ext::go:github.com/foo/bar::X", "X", "github.com/foo/bar", "module::go:github.com/foo/bar@v1", graph.KindFunction)
	addExternalCall(srv.graph, "main.go::A", "ext::go:fmt::Println")
	addExternalCall(srv.graph, "main.go::B", "ext::go:github.com/foo/bar::X")

	out := callAnalyzeExternalCalls(t, srv, map[string]any{
		"module_path": "github.com",
	})
	rows, _ := out["modules"].([]any)
	if len(rows) != 1 {
		t.Fatalf("expected 1 module after module_path=github.com, got %d", len(rows))
	}
	first := rows[0].(map[string]any)
	if first["path"] != "github.com/foo/bar" {
		t.Errorf("filter leaked: %v", first["path"])
	}
}

// TestAnalyzeExternalCalls_SkipsSyntheticNodes confirms the rollup
// composes with the resolver's external-call synthesis pass: a
// synthetic KindModule placeholder (it carries Meta["synthetic"] but no
// goanalysis attribution) must not surface as an empty 0/0 row.
func TestAnalyzeExternalCalls_SkipsSyntheticNodes(t *testing.T) {
	srv, _ := setupTestServer(t)
	addExternalModuleNode(srv.graph, "module::go:stdlib", "stdlib", "", "stdlib")
	addExternalSymbolNode(srv.graph, "ext::go:fmt::Println", "Println", "fmt", "module::go:stdlib", graph.KindFunction)
	addExternalCall(srv.graph, "main.go::A", "ext::go:fmt::Println")
	// A synthetic external-call node — the shape the resolver's
	// SynthesizeExternalCalls pass materialises for an un-indexed
	// package. It must be ignored by this rollup.
	srv.graph.AddNode(&graph.Node{
		ID:       "external-call::dep::github.com/acme/widget",
		Kind:     graph.KindModule,
		Name:     "github.com/acme/widget",
		Language: "go",
		Meta: map[string]any{
			"synthetic":     true,
			"external_call": true,
			"import_path":   "github.com/acme/widget",
			"ecosystem":     "dep",
		},
	})
	addExternalCall(srv.graph, "main.go::B", "external-call::dep::github.com/acme/widget")

	out := callAnalyzeExternalCalls(t, srv, map[string]any{})
	rows, _ := out["modules"].([]any)
	if len(rows) != 1 {
		t.Fatalf("expected 1 module (synthetic node skipped), got %d", len(rows))
	}
	first := rows[0].(map[string]any)
	if first["id"] != "module::go:stdlib" {
		t.Errorf("synthetic node leaked into the rollup: %v", first["id"])
	}
}

func TestAnalyzeExternalCalls_PerModuleSymbolDetail(t *testing.T) {
	srv, _ := setupTestServer(t)
	addExternalModuleNode(srv.graph, "module::go:stdlib", "stdlib", "", "stdlib")
	addExternalSymbolNode(srv.graph, "ext::go:fmt::Println", "Println", "fmt", "module::go:stdlib", graph.KindFunction)
	addExternalSymbolNode(srv.graph, "ext::go:fmt::Sprintf", "Sprintf", "fmt", "module::go:stdlib", graph.KindFunction)
	addExternalCall(srv.graph, "main.go::A", "ext::go:fmt::Println")
	addExternalCall(srv.graph, "main.go::B", "ext::go:fmt::Println") // 2 callers
	addExternalCall(srv.graph, "main.go::C", "ext::go:fmt::Sprintf")

	out := callAnalyzeExternalCalls(t, srv, map[string]any{
		"id": "module::go:stdlib",
	})
	if out["module"] != "module::go:stdlib" {
		t.Errorf("expected module=stdlib, got %v", out["module"])
	}
	rows, _ := out["symbols"].([]any)
	if len(rows) != 2 {
		t.Fatalf("expected 2 symbols, got %d", len(rows))
	}
	first := rows[0].(map[string]any)
	if first["name"] != "Println" {
		t.Errorf("expected Println first (2 calls), got %v", first["name"])
	}
	if int(first["calls"].(float64)) != 2 {
		t.Errorf("expected 2 calls, got %v", first["calls"])
	}
	if int(first["callers"].(float64)) != 2 {
		t.Errorf("expected 2 distinct callers, got %v", first["callers"])
	}
}

func TestAnalyzeExternalCalls_PerModuleNameFilter(t *testing.T) {
	srv, _ := setupTestServer(t)
	addExternalModuleNode(srv.graph, "module::go:stdlib", "stdlib", "", "stdlib")
	addExternalSymbolNode(srv.graph, "ext::go:fmt::Println", "Println", "fmt", "module::go:stdlib", graph.KindFunction)
	addExternalSymbolNode(srv.graph, "ext::go:fmt::Sprintf", "Sprintf", "fmt", "module::go:stdlib", graph.KindFunction)
	addExternalCall(srv.graph, "main.go::A", "ext::go:fmt::Println")
	addExternalCall(srv.graph, "main.go::B", "ext::go:fmt::Sprintf")

	out := callAnalyzeExternalCalls(t, srv, map[string]any{
		"id":   "module::go:stdlib",
		"name": "println",
	})
	rows, _ := out["symbols"].([]any)
	if len(rows) != 1 {
		t.Fatalf("expected 1 symbol after name=println filter, got %d", len(rows))
	}
	first := rows[0].(map[string]any)
	if first["name"] != "Println" {
		t.Errorf("expected Println, got %v", first["name"])
	}
}

func TestAnalyzeExternalCalls_DependsOnModuleEdgesIgnoredInCallCount(t *testing.T) {
	// EdgeDependsOnModule from external symbol → KindModule must NOT be
	// counted as a "call". Only true call/reference edges count.
	srv, _ := setupTestServer(t)
	addExternalModuleNode(srv.graph, "module::go:stdlib", "stdlib", "", "stdlib")
	addExternalSymbolNode(srv.graph, "ext::go:fmt::Println", "Println", "fmt", "module::go:stdlib", graph.KindFunction)

	// No real callers, just the structural EdgeDependsOnModule.
	out := callAnalyzeExternalCalls(t, srv, map[string]any{})
	rows, _ := out["modules"].([]any)
	if len(rows) != 1 {
		t.Fatalf("expected 1 module, got %d", len(rows))
	}
	first := rows[0].(map[string]any)
	if int(first["calls"].(float64)) != 0 {
		t.Errorf("EdgeDependsOnModule must not count as a call; got calls=%v", first["calls"])
	}
	if int(first["symbols"].(float64)) != 1 {
		t.Errorf("expected 1 symbol, got %v", first["symbols"])
	}
}

func TestAnalyzeExternalCalls_EmptyGraphReturnsZero(t *testing.T) {
	srv, _ := setupTestServer(t)
	out := callAnalyzeExternalCalls(t, srv, map[string]any{})
	rows, _ := out["modules"].([]any)
	if len(rows) != 0 {
		t.Fatalf("expected 0 modules on empty graph, got %d", len(rows))
	}
	if int(out["total"].(float64)) != 0 {
		t.Errorf("expected total=0, got %v", out["total"])
	}
}
