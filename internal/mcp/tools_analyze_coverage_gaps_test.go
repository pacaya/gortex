package mcp

import (
	"context"
	"encoding/json"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/zzet/gortex/internal/graph"
)

// addCoveredNode wires a function node with synthetic
// coverage_pct meta — emulating coverage.EnrichGraph output.
func addCoveredNode(g graph.Store, id, file string, pct float64, numStmt, hit int) {
	g.AddNode(&graph.Node{
		ID:        id,
		Kind:      graph.KindFunction,
		Name:      id,
		FilePath:  file,
		StartLine: 1,
		EndLine:   1,
		Meta: map[string]any{
			"coverage_pct": pct,
			"coverage": map[string]any{
				"num_stmt": numStmt,
				"hit":      hit,
			},
		},
	})
}

func callAnalyzeCoverageGaps(t *testing.T, srv *Server, args map[string]any) map[string]any {
	t.Helper()
	args["kind"] = "coverage_gaps"
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

func TestAnalyzeCoverageGaps_DefaultIncludesAllUnder100(t *testing.T) {
	srv, _ := setupTestServer(t)
	addCoveredNode(srv.graph, "f.go::A", "f.go", 0, 10, 0)
	addCoveredNode(srv.graph, "f.go::B", "f.go", 50, 4, 2)
	addCoveredNode(srv.graph, "f.go::C", "f.go", 100, 5, 5)

	out := callAnalyzeCoverageGaps(t, srv, map[string]any{})
	rows, _ := out["gaps"].([]any)
	if len(rows) != 2 {
		t.Errorf("expected 2 (A + B; C is fully covered), got %d", len(rows))
	}
	// Lowest coverage first.
	first, _ := rows[0].(map[string]any)
	if first["id"] != "f.go::A" {
		t.Errorf("expected A first (0%%), got %v", first["id"])
	}
}

func TestAnalyzeCoverageGaps_BandFilter(t *testing.T) {
	srv, _ := setupTestServer(t)
	addCoveredNode(srv.graph, "f.go::A", "f.go", 10, 10, 1)
	addCoveredNode(srv.graph, "f.go::B", "f.go", 30, 10, 3)
	addCoveredNode(srv.graph, "f.go::C", "f.go", 60, 10, 6)
	addCoveredNode(srv.graph, "f.go::D", "f.go", 90, 10, 9)

	out := callAnalyzeCoverageGaps(t, srv, map[string]any{
		"min_pct": 20.0,
		"max_pct": 70.0,
	})
	rows, _ := out["gaps"].([]any)
	if len(rows) != 2 {
		t.Errorf("expected 2 (B + C in [20,70)), got %d", len(rows))
	}
}

func TestAnalyzeCoverageGaps_PathPrefix(t *testing.T) {
	srv, _ := setupTestServer(t)
	addCoveredNode(srv.graph, "internal/auth/jwt.go::Verify", "internal/auth/jwt.go", 50, 4, 2)
	addCoveredNode(srv.graph, "internal/db/store.go::Save", "internal/db/store.go", 50, 4, 2)

	out := callAnalyzeCoverageGaps(t, srv, map[string]any{
		"path_prefix": "internal/auth/",
	})
	rows, _ := out["gaps"].([]any)
	if len(rows) != 1 {
		t.Errorf("expected only auth (path_prefix), got %d", len(rows))
	}
}

func TestAnalyzeCoverageGaps_SkipsUnenriched(t *testing.T) {
	srv, _ := setupTestServer(t)
	addCoveredNode(srv.graph, "f.go::A", "f.go", 50, 4, 2)
	srv.graph.AddNode(&graph.Node{
		ID:       "f.go::B",
		Kind:     graph.KindFunction,
		FilePath: "f.go",
	})

	out := callAnalyzeCoverageGaps(t, srv, map[string]any{})
	rows, _ := out["gaps"].([]any)
	if len(rows) != 1 {
		t.Errorf("unenriched node should be skipped, got %d gaps", len(rows))
	}
}

func TestAnalyzeCoverageGaps_TieBreakBySize(t *testing.T) {
	srv, _ := setupTestServer(t)
	// Two functions at the same coverage % — bigger one first.
	addCoveredNode(srv.graph, "f.go::Small", "f.go", 50, 2, 1)
	addCoveredNode(srv.graph, "f.go::Big", "f.go", 50, 20, 10)

	out := callAnalyzeCoverageGaps(t, srv, map[string]any{})
	rows, _ := out["gaps"].([]any)
	if len(rows) != 2 {
		t.Fatalf("expected 2 gaps, got %d", len(rows))
	}
	first, _ := rows[0].(map[string]any)
	if first["id"] != "f.go::Big" {
		t.Errorf("expected larger function first at tied pct, got %v", first["id"])
	}
}
