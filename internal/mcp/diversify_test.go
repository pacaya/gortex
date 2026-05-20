package mcp

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search/rerank"
)

func divNode(id, file string) *graph.Node {
	return &graph.Node{ID: id, Name: id, Kind: graph.KindFunction, FilePath: file}
}

func divIDs(nodes []*graph.Node) []string {
	out := make([]string, len(nodes))
	for i, n := range nodes {
		out[i] = n.ID
	}
	return out
}

func TestDiversifyByFile_DemotesOverCapHits(t *testing.T) {
	nodes := []*graph.Node{
		divNode("a1", "a.go"),
		divNode("a2", "a.go"),
		divNode("a3", "a.go"),
		divNode("a4", "a.go"),
		divNode("b1", "b.go"),
	}
	got, _ := diversifyByFile(nodes, nil, 2)
	// First 2 from a.go keep their slots, b1 stays, a3/a4 demoted.
	want := []string{"a1", "a2", "b1", "a3", "a4"}
	if ids := divIDs(got); !equalStrings(ids, want) {
		t.Errorf("diversifyByFile = %v, want %v", ids, want)
	}
}

func TestDiversifyByFile_DisabledWhenCapZero(t *testing.T) {
	nodes := []*graph.Node{divNode("a1", "a.go"), divNode("a2", "a.go")}
	got, _ := diversifyByFile(nodes, nil, 0)
	if ids := divIDs(got); !equalStrings(ids, []string{"a1", "a2"}) {
		t.Errorf("cap 0 must disable diversification, got %v", ids)
	}
}

func TestDiversifyByFile_NoOpWhenUnderCap(t *testing.T) {
	nodes := []*graph.Node{
		divNode("a1", "a.go"),
		divNode("b1", "b.go"),
		divNode("c1", "c.go"),
	}
	got, _ := diversifyByFile(nodes, nil, 3)
	if ids := divIDs(got); !equalStrings(ids, []string{"a1", "b1", "c1"}) {
		t.Errorf("no file over cap → order unchanged, got %v", ids)
	}
}

func TestDiversifyByFile_EmptyFilePathStaysInHead(t *testing.T) {
	nodes := []*graph.Node{
		divNode("a1", "a.go"),
		divNode("a2", "a.go"),
		divNode("nofile", ""),
		divNode("a3", "a.go"),
	}
	got, _ := diversifyByFile(nodes, nil, 1)
	// a.go capped at 1: a2/a3 demoted; the file-less node stays put.
	want := []string{"a1", "nofile", "a2", "a3"}
	if ids := divIDs(got); !equalStrings(ids, want) {
		t.Errorf("diversifyByFile = %v, want %v", ids, want)
	}
}

func TestDiversifyByFile_BreakdownStaysAligned(t *testing.T) {
	nodes := []*graph.Node{
		divNode("a1", "a.go"),
		divNode("a2", "a.go"),
		divNode("a3", "a.go"),
		divNode("b1", "b.go"),
	}
	breakdown := make([]*rerank.Candidate, len(nodes))
	for i, n := range nodes {
		breakdown[i] = &rerank.Candidate{Node: n, TextRank: i}
	}
	gotNodes, gotBreakdown := diversifyByFile(nodes, breakdown, 2)
	if len(gotBreakdown) != len(gotNodes) {
		t.Fatalf("breakdown length %d != nodes length %d", len(gotBreakdown), len(gotNodes))
	}
	for i := range gotNodes {
		if gotBreakdown[i].Node != gotNodes[i] {
			t.Errorf("breakdown[%d] node %q misaligned with result %q",
				i, gotBreakdown[i].Node.ID, gotNodes[i].ID)
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
