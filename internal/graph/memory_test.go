package graph

import (
	"testing"
)

func TestRepoMemoryEstimate_Empty(t *testing.T) {
	g := New()
	est := g.RepoMemoryEstimate("nonexistent")
	if est.NodeCount != 0 || est.EdgeCount != 0 || est.Total() != 0 {
		t.Errorf("empty repo should estimate zero, got %+v", est)
	}
}

func TestRepoMemoryEstimate_NodesAndEdges(t *testing.T) {
	g := New()
	n1 := &Node{ID: "r/pkg/a.go::Foo", Kind: KindFunction, Name: "Foo",
		QualName: "pkg.Foo", FilePath: "pkg/a.go", Language: "go",
		RepoPrefix: "r"}
	n2 := &Node{ID: "r/pkg/a.go::Bar", Kind: KindFunction, Name: "Bar",
		QualName: "pkg.Bar", FilePath: "pkg/a.go", Language: "go",
		RepoPrefix: "r"}
	g.AddNode(n1)
	g.AddNode(n2)
	g.AddEdge(&Edge{From: n1.ID, To: n2.ID, Kind: "CALLS", FilePath: "pkg/a.go"})

	est := g.RepoMemoryEstimate("r")
	if est.NodeCount != 2 {
		t.Errorf("expected 2 nodes, got %d", est.NodeCount)
	}
	if est.EdgeCount != 1 {
		t.Errorf("expected 1 edge, got %d", est.EdgeCount)
	}
	if est.NodeBytes == 0 || est.EdgeBytes == 0 {
		t.Errorf("expected non-zero byte estimates, got %+v", est)
	}
}

func TestRepoMemoryEstimate_MetaContributes(t *testing.T) {
	g := New()
	short := &Node{ID: "r/a::s", Kind: KindVariable, Name: "s",
		FilePath: "a", RepoPrefix: "r"}
	long := &Node{ID: "r/a::l", Kind: KindVariable, Name: "l",
		FilePath: "a", RepoPrefix: "r",
		Meta: map[string]any{
			"signature": "func Foo(ctx context.Context, x, y int) (string, error)",
			"docstring": "A long docstring that takes many bytes to store in memory",
			"tags":      []string{"public", "deprecated", "hot-path"},
		}}

	g.AddNode(short)
	g.AddNode(long)

	shortEst := g.RepoMemoryEstimate("r").NodeBytes / 2 // rough avg — need per-node
	_ = shortEst

	// Direct per-node check via unexported helper is fine from test in same package.
	if nodeBytes(long) <= nodeBytes(short) {
		t.Errorf("a node with Meta should be bigger than one without: long=%d short=%d",
			nodeBytes(long), nodeBytes(short))
	}
}

func TestRepoMemoryEstimate_RemoveEdgeDecrements(t *testing.T) {
	g := New()
	n1 := &Node{ID: "r/a::Foo", Kind: KindFunction, Name: "Foo", FilePath: "a", RepoPrefix: "r"}
	n2 := &Node{ID: "r/a::Bar", Kind: KindFunction, Name: "Bar", FilePath: "a", RepoPrefix: "r"}
	g.AddNode(n1)
	g.AddNode(n2)
	e := &Edge{From: n1.ID, To: n2.ID, Kind: "CALLS", FilePath: "a"}
	g.AddEdge(e)
	before := g.RepoMemoryEstimate("r")
	if before.EdgeCount != 1 {
		t.Fatalf("expected 1 edge before remove, got %d", before.EdgeCount)
	}
	g.RemoveEdge(n1.ID, n2.ID, "CALLS")
	after := g.RepoMemoryEstimate("r")
	if after.EdgeCount != 0 {
		t.Errorf("expected 0 edges after RemoveEdge, got %d", after.EdgeCount)
	}
	if after.EdgeBytes != 0 {
		t.Errorf("expected 0 edge bytes after RemoveEdge, got %d", after.EdgeBytes)
	}
}

func TestRepoMemoryEstimate_IdempotentAddDoesNotDoubleCount(t *testing.T) {
	g := New()
	n1 := &Node{ID: "r/a::Foo", Kind: KindFunction, Name: "Foo", FilePath: "a", RepoPrefix: "r"}
	n2 := &Node{ID: "r/a::Bar", Kind: KindFunction, Name: "Bar", FilePath: "a", RepoPrefix: "r"}
	g.AddNode(n1)
	g.AddNode(n2)
	e := &Edge{From: n1.ID, To: n2.ID, Kind: "CALLS", FilePath: "a", Line: 10}
	g.AddEdge(e)
	g.AddEdge(e) // same identity — must not double-count
	g.AddNode(n1) // same identity — must not double-count nodes

	est := g.RepoMemoryEstimate("r")
	if est.NodeCount != 2 {
		t.Errorf("expected 2 nodes after duplicate Adds, got %d", est.NodeCount)
	}
	if est.EdgeCount != 1 {
		t.Errorf("expected 1 edge after duplicate AddEdge, got %d", est.EdgeCount)
	}
}

func TestRepoMemoryEstimate_EvictFileDecrements(t *testing.T) {
	g := New()
	n1 := &Node{ID: "r/a::Foo", Kind: KindFunction, Name: "Foo", FilePath: "a", RepoPrefix: "r"}
	n2 := &Node{ID: "r/b::Bar", Kind: KindFunction, Name: "Bar", FilePath: "b", RepoPrefix: "r"}
	g.AddNode(n1)
	g.AddNode(n2)
	g.AddEdge(&Edge{From: n1.ID, To: n2.ID, Kind: "CALLS", FilePath: "a"})

	g.EvictFile("a")
	est := g.RepoMemoryEstimate("r")
	if est.NodeCount != 1 {
		t.Errorf("expected 1 node after evicting file 'a', got %d", est.NodeCount)
	}
	// Edge sourced from evicted node 'Foo' must also be gone.
	if est.EdgeCount != 0 {
		t.Errorf("expected 0 edges after evicting source node's file, got %d", est.EdgeCount)
	}
}

func TestRepoMemoryEstimate_EvictRepoZeroesCounter(t *testing.T) {
	g := New()
	g.AddNode(&Node{ID: "r1/a::A", Kind: KindFunction, Name: "A", FilePath: "a", RepoPrefix: "r1"})
	g.AddNode(&Node{ID: "r1/a::B", Kind: KindFunction, Name: "B", FilePath: "a", RepoPrefix: "r1"})
	g.AddNode(&Node{ID: "r2/x::X", Kind: KindFunction, Name: "X", FilePath: "x", RepoPrefix: "r2"})
	g.AddEdge(&Edge{From: "r1/a::A", To: "r1/a::B", Kind: "CALLS", FilePath: "a"})
	g.AddEdge(&Edge{From: "r1/a::A", To: "r2/x::X", Kind: "CALLS", FilePath: "a"})

	g.EvictRepo("r1")

	r1 := g.RepoMemoryEstimate("r1")
	if r1.NodeCount != 0 || r1.EdgeCount != 0 || r1.Total() != 0 {
		t.Errorf("evicted repo should estimate zero, got %+v", r1)
	}
	// r2 has only the surviving X node; the cross-repo edge into X was
	// attributed to r1 (source repo) so it shouldn't show up under r2.
	r2 := g.RepoMemoryEstimate("r2")
	if r2.NodeCount != 1 {
		t.Errorf("r2 should still have 1 node, got %d", r2.NodeCount)
	}
	if r2.EdgeCount != 0 {
		t.Errorf("r2 should have 0 outgoing edges, got %d", r2.EdgeCount)
	}
}

func TestAllRepoMemoryEstimates_AggregatesAcrossRepos(t *testing.T) {
	g := New()
	g.AddNode(&Node{ID: "r1/a::A", Kind: KindFunction, Name: "A", FilePath: "a", RepoPrefix: "r1"})
	g.AddNode(&Node{ID: "r2/b::B", Kind: KindFunction, Name: "B", FilePath: "b", RepoPrefix: "r2"})
	g.AddNode(&Node{ID: "r2/b::C", Kind: KindFunction, Name: "C", FilePath: "b", RepoPrefix: "r2"})
	g.AddEdge(&Edge{From: "r2/b::B", To: "r2/b::C", Kind: "CALLS", FilePath: "b"})

	all := g.AllRepoMemoryEstimates()
	if got := all["r1"].NodeCount; got != 1 {
		t.Errorf("r1 node count = %d, want 1", got)
	}
	if got := all["r2"].NodeCount; got != 2 {
		t.Errorf("r2 node count = %d, want 2", got)
	}
	if got := all["r2"].EdgeCount; got != 1 {
		t.Errorf("r2 edge count = %d, want 1", got)
	}
	if got := all["r1"].EdgeCount; got != 0 {
		t.Errorf("r1 edge count = %d, want 0", got)
	}
	// Sanity: bulk and per-repo agree.
	if perRepo := g.RepoMemoryEstimate("r2"); perRepo != all["r2"] {
		t.Errorf("per-repo and bulk disagree for r2: per-repo=%+v bulk=%+v", perRepo, all["r2"])
	}
}

// walkRepoCounts is a reference implementation: walks every node and
// every edge once to bucket counts by repo. Used to verify the running
// counters stay in sync across a mix of mutations.
func walkRepoCounts(g *Graph) (nodes, edges map[string]int) {
	nodes = make(map[string]int)
	edges = make(map[string]int)
	for _, n := range g.AllNodes() {
		if n.RepoPrefix != "" {
			nodes[n.RepoPrefix]++
		}
	}
	for _, e := range g.AllEdges() {
		src := g.GetNode(e.From)
		if src != nil && src.RepoPrefix != "" {
			edges[src.RepoPrefix]++
		}
	}
	return
}

func assertCountersMatchWalk(t *testing.T, g *Graph, where string) {
	t.Helper()
	walkN, walkE := walkRepoCounts(g)
	got := g.AllRepoMemoryEstimates()
	for prefix, want := range walkN {
		if got[prefix].NodeCount != want {
			t.Errorf("%s: %q node count: counter=%d walk=%d",
				where, prefix, got[prefix].NodeCount, want)
		}
	}
	for prefix, want := range walkE {
		if got[prefix].EdgeCount != want {
			t.Errorf("%s: %q edge count: counter=%d walk=%d",
				where, prefix, got[prefix].EdgeCount, want)
		}
	}
	// Counters that don't exist in the walk are also a bug — leaked
	// entries that survived eviction.
	for prefix, est := range got {
		if walkN[prefix] == 0 && est.NodeCount != 0 {
			t.Errorf("%s: %q has counter NodeCount=%d but walk says 0",
				where, prefix, est.NodeCount)
		}
		if walkE[prefix] == 0 && est.EdgeCount != 0 {
			t.Errorf("%s: %q has counter EdgeCount=%d but walk says 0",
				where, prefix, est.EdgeCount)
		}
	}
}

func TestRepoMemoryCounters_StayInSyncUnderMixedMutations(t *testing.T) {
	g := New()

	// Two repos, several nodes each, a mix of intra- and cross-repo edges.
	r1Nodes := []*Node{
		{ID: "r1/a::A", Kind: KindFunction, Name: "A", FilePath: "a", RepoPrefix: "r1"},
		{ID: "r1/a::B", Kind: KindFunction, Name: "B", FilePath: "a", RepoPrefix: "r1"},
		{ID: "r1/b::C", Kind: KindFunction, Name: "C", FilePath: "b", RepoPrefix: "r1"},
	}
	r2Nodes := []*Node{
		{ID: "r2/x::X", Kind: KindFunction, Name: "X", FilePath: "x", RepoPrefix: "r2"},
		{ID: "r2/x::Y", Kind: KindFunction, Name: "Y", FilePath: "x", RepoPrefix: "r2"},
	}
	for _, n := range append(append([]*Node{}, r1Nodes...), r2Nodes...) {
		g.AddNode(n)
	}
	assertCountersMatchWalk(t, g, "after AddNode")

	edges := []*Edge{
		{From: "r1/a::A", To: "r1/a::B", Kind: "CALLS", FilePath: "a"},
		{From: "r1/a::A", To: "r1/b::C", Kind: "CALLS", FilePath: "a"},
		{From: "r1/b::C", To: "r2/x::X", Kind: "CALLS", FilePath: "b"}, // cross-repo
		{From: "r2/x::X", To: "r2/x::Y", Kind: "CALLS", FilePath: "x"},
		{From: "r2/x::Y", To: "r1/a::A", Kind: "CALLS", FilePath: "x"}, // cross-repo back
	}
	for _, e := range edges {
		g.AddEdge(e)
	}
	assertCountersMatchWalk(t, g, "after AddEdge")

	// Idempotent re-adds must not double-count.
	g.AddNode(r1Nodes[0])
	g.AddEdge(edges[0])
	assertCountersMatchWalk(t, g, "after idempotent re-adds")

	// RemoveEdge.
	g.RemoveEdge("r1/a::A", "r1/a::B", "CALLS")
	assertCountersMatchWalk(t, g, "after RemoveEdge")

	// EvictFile — drops both r1/a::A and r1/a::B's host file, including
	// the cross-repo edge r1/b::C → r2/x::X is left alone.
	g.EvictFile("a")
	assertCountersMatchWalk(t, g, "after EvictFile(a)")

	// AddNode an updated version of an existing node (RepoPrefix change).
	g.AddNode(&Node{ID: "r1/b::C", Kind: KindFunction, Name: "C", FilePath: "b", RepoPrefix: "r1"})
	assertCountersMatchWalk(t, g, "after node update (same prefix)")

	// EvictRepo r2 — must zero its counters and decrement r1's edge
	// counter for the r1/b::C → r2/x::X edge (source is r1, target was r2).
	g.EvictRepo("r2")
	assertCountersMatchWalk(t, g, "after EvictRepo(r2)")
}

func TestMetaBytes_HandlesCommonTypes(t *testing.T) {
	cases := []map[string]any{
		nil,
		{},
		{"s": "hello"},
		{"b": true, "i": 42, "f": 3.14},
		{"list": []string{"a", "b", "c"}},
		{"nested": map[string]any{"k": "v"}},
	}
	for i, m := range cases {
		// Should never panic; empty map returns non-zero-ish (header).
		got := metaBytes(m)
		if m == nil && got != 0 {
			t.Errorf("case %d: nil map should be 0, got %d", i, got)
		}
	}
}
