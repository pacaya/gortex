package analysis

import (
	"reflect"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// hierTestGraph builds a small fixture: two packages, each with two
// functions, plus a cross-package call fan-out.
//
//	pkg/auth/login.go   :: Login,  Logout
//	pkg/store/db.go     :: Save,   Load
//
// Edges (all calls unless noted):
//
//	Login  -> Logout   (intra-auth)
//	Login  -> Save     (auth -> store)
//	Login  -> Load     (auth -> store)
//	Logout -> Save     (auth -> store)
//	Save   -> Load     (intra-store)
//
// So auth->store has 3 underlying call edges; auth and store each
// carry exactly one intra-group (self-loop) edge.
func hierTestGraph() *graph.Graph {
	g := graph.New()
	add := func(id, file string) {
		g.AddNode(&graph.Node{ID: id, Kind: graph.KindFunction, Name: id, FilePath: file, RepoPrefix: "demo"})
	}
	add("Login", "pkg/auth/login.go")
	add("Logout", "pkg/auth/login.go")
	add("Save", "pkg/store/db.go")
	add("Load", "pkg/store/db.go")

	g.AddEdge(&graph.Edge{From: "Login", To: "Logout", Kind: graph.EdgeCalls})
	g.AddEdge(&graph.Edge{From: "Login", To: "Save", Kind: graph.EdgeCalls})
	g.AddEdge(&graph.Edge{From: "Login", To: "Load", Kind: graph.EdgeCalls})
	g.AddEdge(&graph.Edge{From: "Logout", To: "Save", Kind: graph.EdgeCalls})
	g.AddEdge(&graph.Edge{From: "Save", To: "Load", Kind: graph.EdgeCalls})
	return g
}

// hierTestCommunities assigns Login+Logout to one community and
// Save+Load to another — the service tier's input.
func hierTestCommunities() *CommunityResult {
	return &CommunityResult{
		Communities: []Community{
			{ID: "community-0", Label: "auth", Members: []string{"Login", "Logout"}, Size: 2},
			{ID: "community-1", Label: "storage", Members: []string{"Save", "Load"}, Size: 2},
		},
		NodeToComm: map[string]string{
			"Login": "community-0", "Logout": "community-0",
			"Save": "community-1", "Load": "community-1",
		},
	}
}

func nodeByID(v *HierarchyView, id string) *HierarchyNode {
	for i := range v.Nodes {
		if v.Nodes[i].ID == id {
			return &v.Nodes[i]
		}
	}
	return nil
}

func edgeBetween(v *HierarchyView, from, to string) *HierarchyEdge {
	for i := range v.Edges {
		if v.Edges[i].From == from && v.Edges[i].To == to {
			return &v.Edges[i]
		}
	}
	return nil
}

func TestBuildHierarchy_LevelsRollUpCorrectly(t *testing.T) {
	g := hierTestGraph()
	cr := hierTestCommunities()

	cases := []struct {
		name          string
		level         ResolutionLevel
		communities   *CommunityResult
		wantNodeIDs   []string // sorted by leaf desc then id asc
		wantLeafCount map[string]int
		wantEdge      [3]any // from, to, weight — the auth->store rollup edge
		wantSelfLoops map[string]int
	}{
		{
			name:        "package tier groups by directory",
			level:       LevelPackage,
			communities: cr,
			// Both packages have 2 leaves; tie broken by ID ascending.
			wantNodeIDs:   []string{"package:pkg/auth", "package:pkg/store"},
			wantLeafCount: map[string]int{"package:pkg/auth": 2, "package:pkg/store": 2},
			wantEdge:      [3]any{"package:pkg/auth", "package:pkg/store", 3},
			wantSelfLoops: map[string]int{"package:pkg/auth": 1, "package:pkg/store": 1},
		},
		{
			name:          "service tier groups by community",
			level:         LevelService,
			communities:   cr,
			wantNodeIDs:   []string{"service:community-0", "service:community-1"},
			wantLeafCount: map[string]int{"service:community-0": 2, "service:community-1": 2},
			wantEdge:      [3]any{"service:community-0", "service:community-1", 3},
			wantSelfLoops: map[string]int{"service:community-0": 1, "service:community-1": 1},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := BuildHierarchy(g, tc.level, tc.communities)
			if v.Level != tc.level {
				t.Fatalf("view level = %q, want %q", v.Level, tc.level)
			}

			gotIDs := make([]string, len(v.Nodes))
			for i, n := range v.Nodes {
				gotIDs[i] = n.ID
			}
			if !reflect.DeepEqual(gotIDs, tc.wantNodeIDs) {
				t.Fatalf("rollup node IDs = %v, want %v", gotIDs, tc.wantNodeIDs)
			}

			// Rolled-up output must contain no function-leaf nodes.
			for _, n := range v.Nodes {
				if g.GetNode(n.ID) != nil {
					t.Errorf("rollup node %q collides with a base-graph leaf node", n.ID)
				}
			}

			for id, want := range tc.wantLeafCount {
				n := nodeByID(v, id)
				if n == nil {
					t.Fatalf("missing rollup node %q", id)
				}
				if n.LeafCount != want {
					t.Errorf("leaf_count[%q] = %d, want %d", id, n.LeafCount, want)
				}
			}

			from, to, w := tc.wantEdge[0].(string), tc.wantEdge[1].(string), tc.wantEdge[2].(int)
			e := edgeBetween(v, from, to)
			if e == nil {
				t.Fatalf("missing rollup edge %s -> %s", from, to)
			}
			if e.Weight != w {
				t.Errorf("rollup edge %s -> %s weight = %d, want %d (count of underlying call edges)", from, to, e.Weight, w)
			}

			if !reflect.DeepEqual(v.SelfLoops, tc.wantSelfLoops) {
				t.Errorf("self_loops = %v, want %v", v.SelfLoops, tc.wantSelfLoops)
			}

			if v.LeafCount != 4 {
				t.Errorf("total leaf_count = %d, want 4", v.LeafCount)
			}
		})
	}
}

func TestBuildHierarchy_SystemTierGroupsByRepo(t *testing.T) {
	g := hierTestGraph()
	v := BuildHierarchy(g, LevelSystem, nil)
	if len(v.Nodes) != 1 {
		t.Fatalf("system tier should yield exactly one repo group, got %d", len(v.Nodes))
	}
	if v.Nodes[0].ID != "system:demo" {
		t.Errorf("system node ID = %q, want system:demo", v.Nodes[0].ID)
	}
	if v.Nodes[0].LeafCount != 4 {
		t.Errorf("system node leaf_count = %d, want 4", v.Nodes[0].LeafCount)
	}
	// One repo means every edge is intra-group — no cross-group edges.
	if len(v.Edges) != 0 {
		t.Errorf("single-repo system tier should have no cross-group edges, got %d", len(v.Edges))
	}
	if v.SelfLoops["system:demo"] != 5 {
		t.Errorf("system self-loop count = %d, want 5 (every call edge intra-repo)", v.SelfLoops["system:demo"])
	}
}

func TestBuildHierarchy_FileTier(t *testing.T) {
	g := hierTestGraph()
	v := BuildHierarchy(g, LevelFile, nil)
	if len(v.Nodes) != 2 {
		t.Fatalf("file tier should yield two file groups, got %d", len(v.Nodes))
	}
	for _, want := range []string{"file:pkg/auth/login.go", "file:pkg/store/db.go"} {
		if nodeByID(v, want) == nil {
			t.Errorf("missing file rollup node %q", want)
		}
	}
	e := edgeBetween(v, "file:pkg/auth/login.go", "file:pkg/store/db.go")
	if e == nil || e.Weight != 3 {
		t.Fatalf("file rollup edge auth->store should have weight 3, got %+v", e)
	}
}

func TestBuildHierarchy_SymbolTierIsLeafGraph(t *testing.T) {
	g := hierTestGraph()
	v := BuildHierarchy(g, LevelSymbol, nil)
	// At the symbol tier each leaf is its own group: 4 leaves, and the
	// 4 cross-leaf call edges become cross-group edges; the 5th edge
	// has no self-loop because no leaf calls itself.
	if len(v.Nodes) != 4 {
		t.Fatalf("symbol tier node count = %d, want 4", len(v.Nodes))
	}
	if len(v.Edges) != 5 {
		t.Fatalf("symbol tier edge count = %d, want 5", len(v.Edges))
	}
	if len(v.SelfLoops) != 0 {
		t.Errorf("symbol tier should have no self loops, got %v", v.SelfLoops)
	}
	// Every symbol-tier rollup-node ID is a base-graph leaf ID.
	for _, n := range v.Nodes {
		if g.GetNode(n.ID) == nil {
			t.Errorf("symbol-tier node %q is not a base-graph node", n.ID)
		}
		if n.LeafCount != 1 {
			t.Errorf("symbol-tier node %q leaf_count = %d, want 1", n.ID, n.LeafCount)
		}
	}
}

func TestBuildHierarchy_ServiceTierFallsBackToPackage(t *testing.T) {
	g := hierTestGraph()
	// No community result: every leaf falls back to its package dir.
	v := BuildHierarchy(g, LevelService, nil)
	for _, want := range []string{"service:dir:pkg/auth", "service:dir:pkg/store"} {
		if nodeByID(v, want) == nil {
			t.Errorf("missing service fallback node %q (nodes: %+v)", want, v.Nodes)
		}
	}
	if e := edgeBetween(v, "service:dir:pkg/auth", "service:dir:pkg/store"); e == nil || e.Weight != 3 {
		t.Fatalf("service-fallback rollup edge should have weight 3, got %+v", e)
	}
}

func TestBuildHierarchy_NoFunctionLeavesInRollup(t *testing.T) {
	g := hierTestGraph()
	// Add file + import scaffolding nodes — they must not become
	// rollup nodes and their structural edges must not be counted.
	g.AddNode(&graph.Node{ID: "pkg/auth/login.go", Kind: graph.KindFile, FilePath: "pkg/auth/login.go", RepoPrefix: "demo"})
	g.AddNode(&graph.Node{ID: "imp::fmt", Kind: graph.KindImport, FilePath: "pkg/auth/login.go", RepoPrefix: "demo"})
	g.AddEdge(&graph.Edge{From: "pkg/auth/login.go", To: "Login", Kind: graph.EdgeDefines})
	g.AddEdge(&graph.Edge{From: "pkg/auth/login.go", To: "imp::fmt", Kind: graph.EdgeImports})

	for _, level := range []ResolutionLevel{LevelFile, LevelPackage, LevelService, LevelSystem} {
		v := BuildHierarchy(g, level, hierTestCommunities())
		// Leaf count stays 4 — the file and import nodes are not leaves.
		if v.LeafCount != 4 {
			t.Errorf("level %q: leaf_count = %d, want 4 (file/import not counted)", level, v.LeafCount)
		}
		for _, n := range v.Nodes {
			if n.ID == "pkg/auth/login.go" || n.ID == "imp::fmt" {
				t.Errorf("level %q: scaffolding node %q leaked into the rollup", level, n.ID)
			}
		}
	}
}

func TestBuildHierarchy_Deterministic(t *testing.T) {
	g := hierTestGraph()
	cr := hierTestCommunities()
	for _, level := range []ResolutionLevel{LevelSymbol, LevelFile, LevelPackage, LevelService, LevelSystem} {
		first := BuildHierarchy(g, level, cr)
		for i := 0; i < 8; i++ {
			again := BuildHierarchy(g, level, cr)
			if !reflect.DeepEqual(first, again) {
				t.Fatalf("level %q: BuildHierarchy is not deterministic across runs", level)
			}
		}
	}
}

func TestBuildHierarchy_InvalidInputs(t *testing.T) {
	if v := BuildHierarchy(nil, LevelPackage, nil); v == nil || len(v.Nodes) != 0 {
		t.Errorf("nil graph should yield an empty view, got %+v", v)
	}
	g := hierTestGraph()
	v := BuildHierarchy(g, ResolutionLevel("galaxy"), nil)
	if v == nil || len(v.Nodes) != 0 || v.Level != ResolutionLevel("galaxy") {
		t.Errorf("unknown level should yield an empty view carrying that level, got %+v", v)
	}
	if !ValidResolutionLevel(LevelService) || ValidResolutionLevel(ResolutionLevel("galaxy")) {
		t.Error("ValidResolutionLevel misclassified a level")
	}
}

func TestPackageDirOf(t *testing.T) {
	cases := []struct{ in, want string }{
		{"pkg/auth/login.go", "pkg/auth"},
		{"main.go", "(root)"},
		{"a/b/c/x.go", "a/b/c"},
		{"", "(root)"},
	}
	for _, c := range cases {
		if got := packageDirOf(c.in); got != c.want {
			t.Errorf("packageDirOf(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
