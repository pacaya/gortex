package mcp

import (
	"context"
	"encoding/json"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/zzet/gortex/internal/graph"
)

func callAnalyzeChannelOps(t *testing.T, srv *Server, args map[string]any) map[string]any {
	t.Helper()
	args["kind"] = "channel_ops"
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

func addChannelEdge(g graph.Store, kind graph.EdgeKind, from, to, file string, line int) {
	g.AddEdge(&graph.Edge{
		From:     from,
		To:       to,
		Kind:     kind,
		FilePath: file,
		Line:     line,
	})
}

func TestAnalyzeChannelOps_GroupsBySendsAndRecvs(t *testing.T) {
	srv, _ := setupTestServer(t)
	addChannelEdge(srv.graph, graph.EdgeSends, "f.go::Producer", "unresolved::ch", "f.go", 10)
	addChannelEdge(srv.graph, graph.EdgeSends, "f.go::Producer", "unresolved::ch", "f.go", 11)
	addChannelEdge(srv.graph, graph.EdgeRecvs, "f.go::Consumer", "unresolved::ch", "f.go", 20)

	out := callAnalyzeChannelOps(t, srv, map[string]any{})
	rows, _ := out["channels"].([]any)
	if len(rows) != 1 {
		t.Fatalf("expected 1 channel, got %d: %+v", len(rows), rows)
	}
	row := rows[0].(map[string]any)
	if got, _ := row["sends"].(float64); got != 2 {
		t.Errorf("sends = %v, want 2", got)
	}
	if got, _ := row["recvs"].(float64); got != 1 {
		t.Errorf("recvs = %v, want 1", got)
	}
}

func TestAnalyzeChannelOps_PathPrefixFilter(t *testing.T) {
	srv, _ := setupTestServer(t)
	addChannelEdge(srv.graph, graph.EdgeSends, "in.go::P", "unresolved::ch1", "internal/in.go", 1)
	addChannelEdge(srv.graph, graph.EdgeSends, "out.go::P", "unresolved::ch2", "cmd/out.go", 1)

	out := callAnalyzeChannelOps(t, srv, map[string]any{
		"path_prefix": "internal/",
	})
	if got, _ := out["total"].(float64); got != 1 {
		t.Errorf("total = %v, want 1 (path_prefix should drop cmd/)", got)
	}
}

func TestAnalyzeChannelOps_OrphanProducerOrConsumer(t *testing.T) {
	srv, _ := setupTestServer(t)
	// only sends — no receivers
	addChannelEdge(srv.graph, graph.EdgeSends, "f.go::P", "unresolved::orphan_send", "f.go", 1)
	// only recvs
	addChannelEdge(srv.graph, graph.EdgeRecvs, "f.go::C", "unresolved::orphan_recv", "f.go", 1)

	out := callAnalyzeChannelOps(t, srv, map[string]any{})
	rows, _ := out["channels"].([]any)
	if len(rows) != 2 {
		t.Fatalf("expected 2 channels, got %d", len(rows))
	}
	// orphan_send should have sends>0, recvs=0; orphan_recv inverse.
	hadSendOrphan, hadRecvOrphan := false, false
	for _, raw := range rows {
		r := raw.(map[string]any)
		ch, _ := r["channel"].(string)
		s, _ := r["sends"].(float64)
		recv, _ := r["recvs"].(float64)
		if ch == "unresolved::orphan_send" && s == 1 && recv == 0 {
			hadSendOrphan = true
		}
		if ch == "unresolved::orphan_recv" && s == 0 && recv == 1 {
			hadRecvOrphan = true
		}
	}
	if !hadSendOrphan || !hadRecvOrphan {
		t.Fatalf("expected both orphan rows: sendOrphan=%v recvOrphan=%v", hadSendOrphan, hadRecvOrphan)
	}
}
