package mcp

import (
	"context"
	"encoding/json"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/zzet/gortex/internal/graph"
)

func TestAnalyzeWasmUsers_ListsFlaggedFiles(t *testing.T) {
	srv, _ := setupTestServer(t)
	srv.graph.AddNode(&graph.Node{
		ID:       "pkg/wasm.rs",
		Kind:     graph.KindFile,
		FilePath: "pkg/wasm.rs",
		Meta:     map[string]any{"uses_wasm_bindgen": true},
	})
	srv.graph.AddNode(&graph.Node{
		ID:       "pkg/regular.rs",
		Kind:     graph.KindFile,
		FilePath: "pkg/regular.rs",
	})

	args := map[string]any{"kind": "wasm_users"}
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
	rows, _ := out["wasm_users"].([]any)
	if len(rows) != 1 {
		t.Fatalf("expected 1 wasm file, got %d", len(rows))
	}
	first, _ := rows[0].(map[string]any)
	if first["file"] != "pkg/wasm.rs" {
		t.Errorf("file = %v", first["file"])
	}
}

func TestAnalyzeCgoUsers_AndWasmUsers_AreSiblings(t *testing.T) {
	// Confirms the dispatcher routes both kinds through the same
	// handler — flag toggling produces independent results.
	srv, _ := setupTestServer(t)
	srv.graph.AddNode(&graph.Node{
		ID:       "pkg/c.go",
		Kind:     graph.KindFile,
		FilePath: "pkg/c.go",
		Meta:     map[string]any{"uses_cgo": true},
	})
	srv.graph.AddNode(&graph.Node{
		ID:       "pkg/w.rs",
		Kind:     graph.KindFile,
		FilePath: "pkg/w.rs",
		Meta:     map[string]any{"uses_wasm_bindgen": true},
	})

	for _, tc := range []struct {
		kind   string
		key    string
		wantID string
	}{
		{"cgo_users", "cgo_users", "pkg/c.go"},
		{"wasm_users", "wasm_users", "pkg/w.rs"},
	} {
		req := mcplib.CallToolRequest{}
		req.Params.Name = "analyze"
		req.Params.Arguments = map[string]any{"kind": tc.kind}
		res, err := srv.handleAnalyze(context.Background(), req)
		if err != nil {
			t.Fatalf("%s: %v", tc.kind, err)
		}
		var out map[string]any
		_ = json.Unmarshal([]byte(res.Content[0].(mcplib.TextContent).Text), &out)
		rows, _ := out[tc.key].([]any)
		if len(rows) != 1 {
			t.Fatalf("%s rows = %d, want 1", tc.kind, len(rows))
		}
		first, _ := rows[0].(map[string]any)
		if first["id"] != tc.wantID {
			t.Errorf("%s id = %v, want %v", tc.kind, first["id"], tc.wantID)
		}
	}
}
