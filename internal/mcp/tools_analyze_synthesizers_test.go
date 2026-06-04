package mcp

import (
	"context"
	"encoding/json"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/zzet/gortex/internal/graph"
)

func callAnalyzeSynthesizers(t *testing.T, srv *Server, args map[string]any) map[string]any {
	t.Helper()
	args["kind"] = "synthesizers"
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

func addSynthEdge(g graph.Store, from, to, by, via string) {
	g.AddEdge(&graph.Edge{
		From: from, To: to, Kind: graph.EdgeCalls,
		Meta: map[string]any{
			"synthesized_by": by,
			"provenance":     "heuristic",
			"via":            via,
		},
	})
}

func TestAnalyzeSynthesizers_GroupsByName(t *testing.T) {
	srv, _ := setupTestServer(t)
	addSynthEdge(srv.graph, "a.go::A", "b.go::B", "event-channel", "event.channel")
	addSynthEdge(srv.graph, "a.go::A", "c.go::C", "event-channel", "event.channel")
	addSynthEdge(srv.graph, "cli.go::run", "svc.go::Handle", "grpc-stub", "grpc.stub")
	// A plain (non-synthesized) edge must be ignored.
	srv.graph.AddEdge(&graph.Edge{From: "x.go::X", To: "y.go::Y", Kind: graph.EdgeCalls})

	out := callAnalyzeSynthesizers(t, srv, map[string]any{})
	if total, _ := out["total_edges"].(float64); int(total) != 3 {
		t.Fatalf("expected 3 synthesized edges, got %v", out["total_edges"])
	}
	rows, _ := out["synthesizers"].([]any)
	if len(rows) != 2 {
		t.Fatalf("expected 2 synthesizer groups, got %d", len(rows))
	}
	first := rows[0].(map[string]any)
	if first["synthesizer"] != "event-channel" {
		t.Errorf("expected event-channel first (most edges), got %v", first["synthesizer"])
	}
	if edges, _ := first["edges"].(float64); int(edges) != 2 {
		t.Errorf("expected event-channel to have 2 edges, got %v", first["edges"])
	}
	if first["provenance"] != "heuristic" {
		t.Errorf("expected heuristic provenance, got %v", first["provenance"])
	}
}

func TestAnalyzeSynthesizers_NameFilter(t *testing.T) {
	srv, _ := setupTestServer(t)
	addSynthEdge(srv.graph, "a.go::A", "b.go::B", "event-channel", "event.channel")
	addSynthEdge(srv.graph, "cli.go::run", "svc.go::Handle", "grpc-stub", "grpc.stub")

	out := callAnalyzeSynthesizers(t, srv, map[string]any{"name": "grpc-stub"})
	rows, _ := out["synthesizers"].([]any)
	if len(rows) != 1 {
		t.Fatalf("expected 1 group with name filter, got %d", len(rows))
	}
	if rows[0].(map[string]any)["synthesizer"] != "grpc-stub" {
		t.Errorf("name filter failed: %v", rows[0])
	}
}
