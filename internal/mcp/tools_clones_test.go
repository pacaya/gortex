package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/graph"
)

// buildCloneGraph constructs a graph with two clone clusters:
//
//   - {pkg/a.go::compute, pkg/b.go::calculate} at similarity 0.90 —
//     `compute` has zero incoming edges (dead), `calculate` is called
//     by Run (live). This is the "dead duplicate of live code" cluster.
//   - {pkg/a.go::helperOne, pkg/a.go::helperTwo} at similarity 0.85 —
//     both have incoming calls, so neither is dead.
func buildCloneGraph() *graph.Graph {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "pkg/a.go", Kind: graph.KindFile, Name: "pkg/a.go", FilePath: "pkg/a.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "pkg/b.go", Kind: graph.KindFile, Name: "pkg/b.go", FilePath: "pkg/b.go", Language: "go"})

	fn := func(id, name, file string, line int) {
		g.AddNode(&graph.Node{ID: id, Kind: graph.KindFunction, Name: name, FilePath: file, StartLine: line, Language: "go"})
	}
	fn("pkg/a.go::compute", "compute", "pkg/a.go", 10)
	fn("pkg/b.go::calculate", "calculate", "pkg/b.go", 20)
	fn("pkg/a.go::helperOne", "helperOne", "pkg/a.go", 40)
	fn("pkg/a.go::helperTwo", "helperTwo", "pkg/a.go", 60)
	fn("pkg/b.go::Run", "Run", "pkg/b.go", 5)
	fn("pkg/a.go::Driver", "Driver", "pkg/a.go", 5)

	// Liveness: calculate / helperOne / helperTwo all have incoming
	// calls. compute deliberately has none → dead code.
	call := func(from, to string) {
		g.AddEdge(&graph.Edge{From: from, To: to, Kind: graph.EdgeCalls, FilePath: "pkg/b.go", Line: 1})
	}
	call("pkg/b.go::Run", "pkg/b.go::calculate")
	call("pkg/a.go::Driver", "pkg/a.go::helperOne")
	call("pkg/a.go::Driver", "pkg/a.go::helperTwo")

	// Symmetric EdgeSimilarTo pairs, as the indexer emits them.
	sim := func(a, b string, score float64) {
		for _, e := range [][2]string{{a, b}, {b, a}} {
			g.AddEdge(&graph.Edge{
				From: e[0], To: e[1], Kind: graph.EdgeSimilarTo,
				FilePath: "pkg/a.go", Confidence: score,
				Origin: graph.OriginASTInferred,
				Meta:   map[string]any{"similarity": score},
			})
		}
	}
	sim("pkg/a.go::compute", "pkg/b.go::calculate", 0.90)
	sim("pkg/a.go::helperOne", "pkg/a.go::helperTwo", 0.85)
	return g
}

func callFindClones(t *testing.T, srv *Server, args map[string]any) map[string]any {
	t.Helper()
	req := mcplib.CallToolRequest{}
	req.Params.Name = "find_clones"
	req.Params.Arguments = args
	res, err := srv.handleFindClones(context.Background(), req)
	if err != nil {
		t.Fatalf("handleFindClones: %v", err)
	}
	if res.IsError {
		t.Fatalf("tool returned error: %+v", res.Content)
	}
	textBlock := res.Content[0].(mcplib.TextContent)
	var out map[string]any
	if err := json.Unmarshal([]byte(textBlock.Text), &out); err != nil {
		t.Fatalf("json unmarshal: %v\n%s", err, textBlock.Text)
	}
	return out
}

func TestFindClones_AllClusters(t *testing.T) {
	srv := &Server{graph: buildCloneGraph()}
	out := callFindClones(t, srv, map[string]any{})

	if got, _ := out["total"].(float64); got != 2 {
		t.Fatalf("total clusters = %v, want 2", got)
	}
	if got, _ := out["pairs"].(float64); got != 2 {
		t.Fatalf("pairs = %v, want 2", got)
	}
	clusters, _ := out["clusters"].([]any)
	if len(clusters) != 2 {
		t.Fatalf("expected 2 clusters, got %d", len(clusters))
	}
	// Dead-bearing cluster sorts first.
	first := clusters[0].(map[string]any)
	if dc, _ := first["dead_count"].(float64); dc != 1 {
		t.Errorf("first cluster dead_count = %v, want 1", dc)
	}
	if has, _ := first["has_dead_code"].(bool); !has {
		t.Error("first cluster should have has_dead_code=true")
	}
	// Verify the dead member is `compute`.
	members, _ := first["members"].([]any)
	foundDead := false
	for _, m := range members {
		mm := m.(map[string]any)
		if mm["id"] == "pkg/a.go::compute" {
			if dead, _ := mm["is_dead"].(bool); !dead {
				t.Error("compute should be is_dead=true")
			}
			foundDead = true
		}
		if mm["id"] == "pkg/b.go::calculate" {
			if dead, _ := mm["is_dead"].(bool); dead {
				t.Error("calculate is called by Run — should be is_dead=false")
			}
		}
	}
	if !foundDead {
		t.Error("compute not present in the dead-bearing cluster")
	}
}

func TestFindClones_DeadOnly(t *testing.T) {
	srv := &Server{graph: buildCloneGraph()}
	out := callFindClones(t, srv, map[string]any{"dead_only": true})

	clusters, _ := out["clusters"].([]any)
	if len(clusters) != 1 {
		t.Fatalf("dead_only should yield exactly the 1 dead-bearing cluster, got %d", len(clusters))
	}
	c := clusters[0].(map[string]any)
	if has, _ := c["has_dead_code"].(bool); !has {
		t.Error("dead_only cluster must have has_dead_code=true")
	}
}

func TestFindClones_MinSimilarityFilter(t *testing.T) {
	srv := &Server{graph: buildCloneGraph()}
	// 0.88 keeps the 0.90 pair, drops the 0.85 pair.
	out := callFindClones(t, srv, map[string]any{"min_similarity": 0.88})
	if got, _ := out["total"].(float64); got != 1 {
		t.Fatalf("min_similarity=0.88 should leave 1 cluster, got %v", got)
	}
}

func TestFindClones_PathPrefixFilter(t *testing.T) {
	srv := &Server{graph: buildCloneGraph()}
	// Only pkg/a.go symbols in scope: the compute/calculate pair spans
	// a.go↔b.go so it is dropped (calculate out of scope); the
	// helperOne/helperTwo pair is fully inside pkg/a.go and survives.
	out := callFindClones(t, srv, map[string]any{"path_prefix": "pkg/a.go"})
	clusters, _ := out["clusters"].([]any)
	if len(clusters) != 1 {
		t.Fatalf("path_prefix should leave 1 cluster, got %d", len(clusters))
	}
	c := clusters[0].(map[string]any)
	members, _ := c["members"].([]any)
	for _, m := range members {
		id, _ := m.(map[string]any)["id"].(string)
		if !strings.HasPrefix(id, "pkg/a.go") {
			t.Errorf("member %s leaked past path_prefix", id)
		}
	}
}

func TestFindClones_RepoFilter(t *testing.T) {
	g := buildCloneGraph()
	// Tag every node with a repo prefix so the repo filter has something
	// to match; a non-matching repo must yield zero clusters.
	for _, n := range g.AllNodes() {
		n.RepoPrefix = "myrepo"
	}
	srv := &Server{graph: g}

	if out := callFindClones(t, srv, map[string]any{"repo": "myrepo"}); func() bool {
		got, _ := out["total"].(float64)
		return got != 2
	}() {
		t.Error("repo=myrepo should match all clusters")
	}
	if out := callFindClones(t, srv, map[string]any{"repo": "other"}); func() bool {
		got, _ := out["total"].(float64)
		return got != 0
	}() {
		t.Error("repo=other should match nothing")
	}
}

func TestFindClones_GCXFormat(t *testing.T) {
	srv := &Server{graph: buildCloneGraph()}
	req := mcplib.CallToolRequest{}
	req.Params.Name = "find_clones"
	req.Params.Arguments = map[string]any{"format": "gcx"}
	res, err := srv.handleFindClones(context.Background(), req)
	if err != nil {
		t.Fatalf("handleFindClones gcx: %v", err)
	}
	if res.IsError {
		t.Fatalf("tool returned error: %+v", res.Content)
	}
	text := res.Content[0].(mcplib.TextContent).Text
	if !strings.Contains(text, "find_clones.summary") {
		t.Errorf("gcx output missing summary section:\n%s", text)
	}
	if !strings.Contains(text, "find_clones.clusters") {
		t.Errorf("gcx output missing clusters section:\n%s", text)
	}
}

func TestFindClones_EmptyGraph(t *testing.T) {
	srv := &Server{graph: graph.New()}
	out := callFindClones(t, srv, map[string]any{})
	if got, _ := out["total"].(float64); got != 0 {
		t.Fatalf("empty graph should yield 0 clusters, got %v", got)
	}
}
