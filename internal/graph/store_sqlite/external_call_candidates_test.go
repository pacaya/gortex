package store_sqlite_test

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// TestExternalCallCandidateEdges asserts the pushdown selects exactly the
// external-package terminals (dep:: / stdlib:: / external::, the
// per-repo-prefixed stdlib form, and already-materialised external-call::
// nodes) and nothing else — no ordinary resolved call/reference edges,
// no non-call edges.
func TestExternalCallCandidateEdges(t *testing.T) {
	s := openTestStore(t)

	add := func(from, to string, kind graph.EdgeKind) {
		s.AddEdge(&graph.Edge{From: from, To: to, Kind: kind, FilePath: "a.go", Line: 1})
	}
	// External terminals — should be selected.
	add("a.go::f", "dep::github.com/x/y::Z", graph.EdgeCalls)
	add("a.go::f", "stdlib::fmt::Sprintf", graph.EdgeCalls)
	add("a.go::f", "myrepo::stdlib::net/http::Get", graph.EdgeCalls) // per-repo-prefixed stdlib form
	add("a.go::f", "external::svc.internal/api", graph.EdgeReferences)
	add("a.go::f", "external-call::dep::github.com/a/b", graph.EdgeCalls) // already synthesized
	// Non-candidates — must NOT be selected.
	add("a.go::f", "a.go::resolvedCallee", graph.EdgeCalls)   // ordinary resolved call
	add("a.go::f", "unresolved::SomeName", graph.EdgeCalls)   // bare unresolved (no import evidence)
	add("a.go::f", "a.go::SomeType", graph.EdgeImplements)    // not a call/ref edge
	add("a.go::f", "dep::github.com/x/y::Z", graph.EdgeTests) // dep target but wrong kind

	got := map[string]bool{}
	for _, e := range s.ExternalCallCandidateEdges() {
		got[e.To] = true
	}

	want := []string{
		"dep::github.com/x/y::Z",
		"stdlib::fmt::Sprintf",
		"myrepo::stdlib::net/http::Get",
		"external::svc.internal/api",
		"external-call::dep::github.com/a/b",
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("ExternalCallCandidateEdges missing external terminal %q", w)
		}
	}
	notWant := []string{"a.go::resolvedCallee", "unresolved::SomeName", "a.go::SomeType"}
	for _, nw := range notWant {
		if got[nw] {
			t.Errorf("ExternalCallCandidateEdges wrongly selected non-candidate %q", nw)
		}
	}
	// The EdgeImplements/EdgeTests rows share targets with selected ones
	// but must not inflate the count via the wrong kind: exactly the 5
	// distinct external targets above, all from calls/references kinds.
	if len(got) != len(want) {
		t.Errorf("selected %d distinct targets, want %d: %v", len(got), len(want), got)
	}
}
