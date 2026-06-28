package query_test

import (
	"path/filepath"
	"sort"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/query"
)

// bfsFixture seeds a store with a deep call chain plus a cycle, a
// reference edge, and an unresolved dynamic-dispatch target, so the
// traversal exercises depth, cycle termination, and boundary recording.
func bfsFixture(s graph.Store) {
	for _, id := range []string{"main", "a", "b", "c", "d", "x", "h"} {
		s.AddNode(&graph.Node{ID: id, Kind: graph.KindFunction, Name: id, FilePath: "p.go", Language: "go", StartLine: 1, EndLine: 5})
	}
	add := func(from, to string, kind graph.EdgeKind, line int) {
		s.AddEdge(&graph.Edge{From: from, To: to, Kind: kind, FilePath: "p.go", Line: line, Confidence: 1, Origin: graph.OriginASTResolved})
	}
	add("main", "a", graph.EdgeCalls, 1)
	add("a", "b", graph.EdgeCalls, 2)
	add("b", "c", graph.EdgeCalls, 3)
	add("c", "d", graph.EdgeCalls, 4)
	add("a", "x", graph.EdgeCalls, 5)
	add("b", "a", graph.EdgeCalls, 6) // cycle
	add("h", "d", graph.EdgeReferences, 7)
	add("c", "unresolved::dyn", graph.EdgeCalls, 8) // dropped dynamic dispatch
}

func nodeIDSet(sg *query.SubGraph) []string {
	ids := make([]string, 0, len(sg.Nodes))
	for _, n := range sg.Nodes {
		ids = append(ids, n.ID)
	}
	sort.Strings(ids)
	return ids
}

func edgeKeySet(sg *query.SubGraph) []string {
	keys := make([]string, 0, len(sg.Edges))
	for _, e := range sg.Edges {
		keys = append(keys, e.From+"->"+e.To+":"+string(e.Kind))
	}
	sort.Strings(keys)
	return keys
}

func eqStrings(a, b []string) bool {
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

// TestBFSBackendParity asserts the SQLite recursive-CTE BFS path and the
// in-memory Go BFS path drive get_call_chain / get_callers to identical
// reachable node sets, discovery-edge endpoint sets, and dispatch-boundary
// flags — the cross-backend identical-results guarantee.
func TestBFSBackendParity(t *testing.T) {
	mem := graph.New()
	bfsFixture(mem)

	disk, err := store_sqlite.Open(filepath.Join(t.TempDir(), "store.sqlite"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer disk.Close()
	bfsFixture(disk)

	memEng := query.NewEngine(mem)
	diskEng := query.NewEngine(disk)

	cases := []struct {
		name string
		run  func(e *query.Engine) *query.SubGraph
	}{
		{"call_chain/deep", func(e *query.Engine) *query.SubGraph {
			return e.GetCallChain("main", query.QueryOptions{Depth: 6, Limit: 50, Detail: "brief"})
		}},
		{"call_chain/shallow", func(e *query.Engine) *query.SubGraph {
			return e.GetCallChain("main", query.QueryOptions{Depth: 2, Limit: 50, Detail: "brief"})
		}},
		{"callers/deep", func(e *query.Engine) *query.SubGraph {
			return e.GetCallers("d", query.QueryOptions{Depth: 6, Limit: 50, Detail: "brief"})
		}},
		{"call_chain/limit", func(e *query.Engine) *query.SubGraph {
			return e.GetCallChain("main", query.QueryOptions{Depth: 6, Limit: 3, Detail: "brief"})
		}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			memSG := c.run(memEng)
			diskSG := c.run(diskEng)

			if mn, dn := nodeIDSet(memSG), nodeIDSet(diskSG); !eqStrings(mn, dn) {
				t.Fatalf("node sets differ:\n memory: %v\n sqlite: %v", mn, dn)
			}
			if me, de := edgeKeySet(memSG), edgeKeySet(diskSG); !eqStrings(me, de) {
				t.Fatalf("edge sets differ:\n memory: %v\n sqlite: %v", me, de)
			}
			if memSG.LowerBound != diskSG.LowerBound {
				t.Fatalf("LowerBound differs: memory=%v sqlite=%v", memSG.LowerBound, diskSG.LowerBound)
			}
		})
	}

	// The deep call chain drops c -> unresolved::dyn, so the reachable set is
	// a floor on both backends.
	if sg := memEng.GetCallChain("main", query.QueryOptions{Depth: 6, Limit: 50, Detail: "brief"}); !sg.LowerBound {
		t.Fatalf("expected LowerBound=true for the dynamic-dispatch chain, got %+v", sg.Boundaries)
	}
}
