package todos

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func defaultTags() []string {
	return []string{"TODO", "FIXME", "HACK", "XXX", "NOTE"}
}

func TestScan_BasicMarkers(t *testing.T) {
	src := []byte(`package main
// TODO: refactor this
// FIXME(zzet): broken on Windows
# HACK: shell hack
-- NOTE: schema migration pending
/* XXX: thread-unsafe */
`)
	got := Scan(src, defaultTags(), 200)
	if len(got) != 5 {
		t.Fatalf("expected 5 findings, got %d: %+v", len(got), got)
	}
	wantTags := []string{"TODO", "FIXME", "HACK", "NOTE", "XXX"}
	for i, f := range got {
		if f.Tag != wantTags[i] {
			t.Errorf("finding %d: tag = %q, want %q", i, f.Tag, wantTags[i])
		}
	}
	if got[1].Assignee != "zzet" {
		t.Errorf("assignee = %q, want zzet", got[1].Assignee)
	}
}

func TestScan_AssigneeAndDue(t *testing.T) {
	src := []byte(`// TODO(alice)[2026-05-01]: clean up flag once cohort done #PROJ-42
`)
	got := Scan(src, defaultTags(), 200)
	if len(got) != 1 {
		t.Fatalf("expected 1, got %d", len(got))
	}
	f := got[0]
	if f.Assignee != "alice" {
		t.Errorf("assignee = %q", f.Assignee)
	}
	if f.Due != "2026-05-01" {
		t.Errorf("due = %q", f.Due)
	}
	if f.Ticket != "PROJ-42" {
		t.Errorf("ticket = %q", f.Ticket)
	}
	if f.Text == "" {
		t.Errorf("text should not be empty")
	}
}

func TestScan_IgnoresStringLiterals(t *testing.T) {
	src := []byte(`package main

func main() {
	s := "// TODO not a real todo"
	_ = s
}
`)
	got := Scan(src, defaultTags(), 200)
	if len(got) != 0 {
		t.Fatalf("expected no findings (string literal), got %d: %+v", len(got), got)
	}
}

func TestScan_BlockCommentContinuation(t *testing.T) {
	src := []byte(`/*
 * TODO: continued from above
 */
`)
	got := Scan(src, defaultTags(), 200)
	if len(got) != 1 {
		t.Fatalf("expected 1, got %d", len(got))
	}
	if got[0].Tag != "TODO" || got[0].Line != 2 {
		t.Errorf("got %+v", got[0])
	}
}

func TestScan_RespectsMaxText(t *testing.T) {
	long := "// TODO: " + repeat("x", 500)
	got := Scan([]byte(long), defaultTags(), 50)
	if len(got) != 1 {
		t.Fatalf("expected 1, got %d", len(got))
	}
	if len(got[0].Text) != 50 {
		t.Errorf("len(text) = %d, want 50", len(got[0].Text))
	}
}

func TestBuildGraphArtifacts(t *testing.T) {
	findings := []Finding{
		{Tag: "TODO", Text: "do the thing", Line: 10},
		{Tag: "FIXME", Text: "fix the thing", Line: 20, Assignee: "bob", Due: "2026-06-01"},
	}
	nodes, edges := BuildGraphArtifacts("pkg/foo.go", findings, "go")
	if len(nodes) != 2 {
		t.Fatalf("nodes = %d", len(nodes))
	}
	if len(edges) != 2 {
		t.Fatalf("edges = %d", len(edges))
	}
	if nodes[0].Kind != graph.KindTodo {
		t.Errorf("node kind = %q", nodes[0].Kind)
	}
	if nodes[0].ID != "pkg/foo.go::todo:10" {
		t.Errorf("node id = %q", nodes[0].ID)
	}
	if nodes[1].Meta["assignee"] != "bob" {
		t.Errorf("meta.assignee = %v", nodes[1].Meta["assignee"])
	}
	if edges[0].Kind != graph.EdgeAnnotated {
		t.Errorf("edge kind = %q", edges[0].Kind)
	}
	if edges[0].From != "pkg/foo.go" {
		t.Errorf("edge.From = %q", edges[0].From)
	}
}

func TestBuildGraphArtifacts_DisambiguatesSameLine(t *testing.T) {
	findings := []Finding{
		{Tag: "TODO", Text: "first", Line: 10},
		{Tag: "FIXME", Text: "second", Line: 10},
	}
	nodes, _ := BuildGraphArtifacts("pkg/foo.go", findings, "go")
	if nodes[0].ID == nodes[1].ID {
		t.Errorf("expected unique IDs on same line, got %q twice", nodes[0].ID)
	}
}

func repeat(s string, n int) string {
	out := make([]byte, 0, len(s)*n)
	for range n {
		out = append(out, s...)
	}
	return string(out)
}
