package resolver

import (
	"fmt"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

// storeNodeSet returns the store's nodes as a sorted, comparable set of
// (id, kind) tuples — used alongside storeEdgeSet so the tail-pass equivalence
// tests also catch a node the scoped pass materialised but the full pass did
// not (or vice-versa).
func storeNodeSet(s graph.Store) []string {
	var out []string
	for _, n := range s.AllNodes() {
		if n == nil {
			continue
		}
		out = append(out, n.ID+"\t"+string(n.Kind))
	}
	sort.Strings(out)
	return out
}

// buildGoTailFixture populates a store with the warm-restart-of-repo-A state
// that exercises every post-resolve Go attribution pass. Repo A's edges start
// in the freshly-re-stubbed / pre-classification shape a reindex leaves behind;
// repo B is already in its post-full-resolve steady state (nothing the tail
// passes should touch), matching a warm restart where only repo A re-indexed.
func buildGoTailFixture(s graph.Store) {
	// --- files ---
	s.AddNode(&graph.Node{ID: "repoa/pkg/a.go", Kind: graph.KindFile, Name: "a.go", FilePath: "repoa/pkg/a.go", Language: "go", RepoPrefix: "repoa"})
	s.AddNode(&graph.Node{ID: "repoa/pkg/a2.go", Kind: graph.KindFile, Name: "a2.go", FilePath: "repoa/pkg/a2.go", Language: "go", RepoPrefix: "repoa"})
	s.AddNode(&graph.Node{ID: "repob/lib/b.go", Kind: graph.KindFile, Name: "b.go", FilePath: "repob/lib/b.go", Language: "go", RepoPrefix: "repob"})

	// (0) same-repo call — resolved by the compute loop, not a tail pass.
	s.AddNode(&graph.Node{ID: "repoa/pkg/a.go::Foo", Kind: graph.KindFunction, Name: "Foo", FilePath: "repoa/pkg/a.go", Language: "go", RepoPrefix: "repoa"})
	s.AddNode(&graph.Node{ID: "repoa/pkg/a.go::CallerA", Kind: graph.KindFunction, Name: "CallerA", FilePath: "repoa/pkg/a.go", Language: "go", RepoPrefix: "repoa"})
	s.AddEdge(&graph.Edge{From: "repoa/pkg/a.go::CallerA", To: "unresolved::Foo", Kind: graph.EdgeCalls, FilePath: "repoa/pkg/a.go", Line: 2})

	// (1) builtin attribution: unresolved::len from a repoa function.
	s.AddNode(&graph.Node{ID: "repoa/pkg/a.go::UsesLen", Kind: graph.KindFunction, Name: "UsesLen", FilePath: "repoa/pkg/a.go", Language: "go", RepoPrefix: "repoa"})
	s.AddEdge(&graph.Edge{From: "repoa/pkg/a.go::UsesLen", To: "unresolved::len", Kind: graph.EdgeCalls, FilePath: "repoa/pkg/a.go", Line: 3})

	// (2) external-call materialisation: a pre-classified stdlib target (the
	// shape resolveExtern leaves in the compute loop; not an unresolved:: stub).
	s.AddNode(&graph.Node{ID: "repoa/pkg/a.go::UsesFmt", Kind: graph.KindFunction, Name: "UsesFmt", FilePath: "repoa/pkg/a.go", Language: "go", RepoPrefix: "repoa"})
	s.AddEdge(&graph.Edge{From: "repoa/pkg/a.go::UsesFmt", To: "stdlib::fmt::Errorf", Kind: graph.EdgeCalls, FilePath: "repoa/pkg/a.go", Line: 4})

	// (3) method-receiver rebind: a phantom <methodfile>::Type receiver for a
	// method declared in a different file of the same package.
	s.AddNode(&graph.Node{ID: "repoa/pkg/a.go::Widget", Kind: graph.KindType, Name: "Widget", FilePath: "repoa/pkg/a.go", Language: "go", RepoPrefix: "repoa"})
	s.AddNode(&graph.Node{ID: "repoa/pkg/a2.go::Widget.Do", Kind: graph.KindMethod, Name: "Do", FilePath: "repoa/pkg/a2.go", Language: "go", RepoPrefix: "repoa"})
	s.AddEdge(&graph.Edge{From: "repoa/pkg/a2.go::Widget.Do", To: "repoa/pkg/a2.go::Widget", Kind: graph.EdgeMemberOf, FilePath: "repoa/pkg/a2.go", Line: 2})

	// (4) bare-name local binding: unresolved::x with an in-scope local.
	s.AddNode(&graph.Node{ID: "repoa/pkg/a.go::Fn", Kind: graph.KindFunction, Name: "Fn", FilePath: "repoa/pkg/a.go", Language: "go", RepoPrefix: "repoa"})
	s.AddNode(&graph.Node{ID: "repoa/pkg/a.go::Fn#local:x", Kind: graph.KindLocal, Name: "x", FilePath: "repoa/pkg/a.go", Language: "go", RepoPrefix: "repoa", StartLine: 6})
	s.AddEdge(&graph.Edge{From: "repoa/pkg/a.go::Fn", To: "unresolved::x", Kind: graph.EdgeReads, FilePath: "repoa/pkg/a.go", Line: 7})

	// (5) generic-param binding: unresolved::T with an in-scope tparam.
	s.AddNode(&graph.Node{ID: "repoa/pkg/a.go::Gen", Kind: graph.KindFunction, Name: "Gen", FilePath: "repoa/pkg/a.go", Language: "go", RepoPrefix: "repoa"})
	s.AddNode(&graph.Node{ID: "repoa/pkg/a.go::Gen#tparam:T", Kind: graph.KindGenericParam, Name: "T", FilePath: "repoa/pkg/a.go", Language: "go", RepoPrefix: "repoa"})
	s.AddEdge(&graph.Edge{From: "repoa/pkg/a.go::Gen", To: "unresolved::T", Kind: graph.EdgeReferences, FilePath: "repoa/pkg/a.go", Line: 9})

	// --- repo B: steady state (already-resolved call + a foreign stub) ---
	s.AddNode(&graph.Node{ID: "repob/lib/b.go::Bar", Kind: graph.KindFunction, Name: "Bar", FilePath: "repob/lib/b.go", Language: "go", RepoPrefix: "repob"})
	s.AddNode(&graph.Node{ID: "repob/lib/b.go::CallerB", Kind: graph.KindFunction, Name: "CallerB", FilePath: "repob/lib/b.go", Language: "go", RepoPrefix: "repob"})
	s.AddEdge(&graph.Edge{From: "repob/lib/b.go::CallerB", To: "repob/lib/b.go::Bar", Kind: graph.EdgeCalls, FilePath: "repob/lib/b.go", Line: 7, Origin: graph.OriginASTResolved})
	s.AddEdge(&graph.Edge{From: "repob/lib/b.go::CallerB", To: "repob::unresolved::Ghost", Kind: graph.EdgeCalls, FilePath: "repob/lib/b.go", Line: 9})
}

// TestResolveAll_ScopedEqualsFull_GoTailPasses is the equivalence gate for the
// scoped post-resolve Go attribution passes (method-receiver rebind, bare-name
// and generic-param binding, builtin + external-call materialisation). A scoped
// ResolveAll over changed repo A and a full ResolveAll over an identical twin
// must land the same node + edge set — and the scoped pass must actually have
// run the tail passes over repo A.
func TestResolveAll_ScopedEqualsFull_GoTailPasses(t *testing.T) {
	scopedStore := graph.New()
	buildGoTailFixture(scopedStore)
	twinStore := graph.New()
	buildGoTailFixture(twinStore)

	scoped := New(scopedStore)
	scoped.SetScope(map[string]struct{}{"repoa": {}})
	require.NotNil(t, scoped.ResolveAll())

	full := New(twinStore)
	require.NotNil(t, full.ResolveAll())

	assert.Equal(t, storeEdgeSet(twinStore), storeEdgeSet(scopedStore),
		"scoped and full resolves must produce identical final edge sets")
	assert.Equal(t, storeNodeSet(twinStore), storeNodeSet(scopedStore),
		"scoped and full resolves must materialise identical node sets")

	// The scoped tail passes ran over repo A: the builtin was attributed off the
	// unresolved stub, the phantom method receiver was lifted onto the canonical
	// type, and the stdlib symbol/module nodes were materialised.
	assert.False(t, hasCallEdge(scopedStore, "repoa/pkg/a.go::UsesLen", "unresolved::len"),
		"repo A's builtin call must no longer be unresolved")
	assert.True(t, hasEdgeKind(scopedStore, "repoa/pkg/a2.go::Widget.Do", "repoa/pkg/a.go::Widget", graph.EdgeMemberOf),
		"repo A's phantom method receiver must rebind onto the canonical type")
	assert.True(t, hasBuiltinNode(scopedStore, "len"),
		"the len builtin node must be materialised in the scoped store")
	assert.True(t, hasGoModuleNode(scopedStore, "fmt"),
		"the fmt stdlib module node must be materialised in the scoped store")

	// Repo B stayed exactly as the fixture left it — the scoped pass skipped it.
	assert.True(t, hasCallEdge(scopedStore, "repob/lib/b.go::CallerB", "repob/lib/b.go::Bar"),
		"repo B's resolved call must stay resolved")
	assert.True(t, hasCallEdge(scopedStore, "repob/lib/b.go::CallerB", "repob::unresolved::Ghost"),
		"repo B's foreign stub must stay unresolved")
}

// TestResolveAll_ScopedAllReposEqualsFull is the degenerate check: putting
// every repo in scope must reproduce the full whole-graph resolve exactly, so
// the per-file attribution dispatch and the repo-filtered language passes are
// faithful reimplementations of their whole-graph forms.
func TestResolveAll_ScopedAllReposEqualsFull(t *testing.T) {
	scopedStore := graph.New()
	buildGoTailFixture(scopedStore)
	twinStore := graph.New()
	buildGoTailFixture(twinStore)

	scoped := New(scopedStore)
	scoped.SetScope(map[string]struct{}{"repoa": {}, "repob": {}})
	require.NotNil(t, scoped.ResolveAll())

	full := New(twinStore)
	require.NotNil(t, full.ResolveAll())

	assert.Equal(t, storeEdgeSet(twinStore), storeEdgeSet(scopedStore))
	assert.Equal(t, storeNodeSet(twinStore), storeNodeSet(scopedStore))
}

// TestResolveAll_ScopedEqualsFull_JavaOverrideDispatch covers the scoped Java
// override-dispatch pass (one of the filtered post-resolve passes with no
// per-file sibling): an ambiguous member call in the changed repo must fan out
// to the override family identically under a scoped and a full resolve.
func TestResolveAll_ScopedEqualsFull_JavaOverrideDispatch(t *testing.T) {
	baseF := "repoa/model/NamedEntity.java"
	ownerF := "repoa/owner/Owner.java"
	sysF := "repoa/system/PropertiesLogger.java"
	caller := sysF + "::PropertiesLogger.printProperties"
	build := func(s graph.Store) {
		for _, f := range []string{baseF, ownerF, sysF} {
			s.AddNode(&graph.Node{ID: f, Kind: graph.KindFile, Name: f, FilePath: f, Language: "java", RepoPrefix: "repoa"})
		}
		// Both NamedEntity and Owner extend BaseEntity (Owner via Person), so
		// their toString overrides share a common ancestor and fan out together.
		s.AddNode(&graph.Node{ID: baseF + "::NamedEntity", Kind: graph.KindType, Name: "NamedEntity", FilePath: baseF, Language: "java", RepoPrefix: "repoa", Meta: map[string]any{"scope_parent": "BaseEntity"}})
		s.AddNode(&graph.Node{ID: ownerF + "::Owner", Kind: graph.KindType, Name: "Owner", FilePath: ownerF, Language: "java", RepoPrefix: "repoa", Meta: map[string]any{"scope_parent": "Person"}})
		s.AddNode(&graph.Node{ID: ownerF + "::Person", Kind: graph.KindType, Name: "Person", FilePath: ownerF, Language: "java", RepoPrefix: "repoa", Meta: map[string]any{"scope_parent": "BaseEntity"}})
		s.AddNode(&graph.Node{ID: baseF + "::NamedEntity.toString", Kind: graph.KindMethod, Name: "toString", FilePath: baseF, Language: "java", RepoPrefix: "repoa", Meta: map[string]any{"receiver": "NamedEntity"}})
		s.AddNode(&graph.Node{ID: ownerF + "::Owner.toString", Kind: graph.KindMethod, Name: "toString", FilePath: ownerF, Language: "java", RepoPrefix: "repoa", Meta: map[string]any{"receiver": "Owner"}})
		s.AddNode(&graph.Node{ID: caller, Kind: graph.KindMethod, Name: "printProperties", FilePath: sysF, Language: "java", RepoPrefix: "repoa", Meta: map[string]any{"receiver": "PropertiesLogger"}})
		s.AddEdge(&graph.Edge{From: caller, To: "unresolved::*.toString", Kind: graph.EdgeCalls, FilePath: sysF, Line: 125})
	}
	scopedStore := graph.New()
	build(scopedStore)
	twinStore := graph.New()
	build(twinStore)

	scoped := New(scopedStore)
	scoped.SetScope(map[string]struct{}{"repoa": {}})
	require.NotNil(t, scoped.ResolveAll())

	full := New(twinStore)
	require.NotNil(t, full.ResolveAll())

	assert.Equal(t, storeEdgeSet(twinStore), storeEdgeSet(scopedStore),
		"scoped and full Java override dispatch must produce identical edge sets")
	// The dispatch actually fanned out to both overrides under the scoped pass.
	assert.True(t, hasEdgeKind(scopedStore, caller, baseF+"::NamedEntity.toString", graph.EdgeCalls))
	assert.True(t, hasEdgeKind(scopedStore, caller, ownerF+"::Owner.toString", graph.EdgeCalls))
	for _, e := range scopedStore.GetOutEdges(caller) {
		assert.NotEqual(t, "unresolved::*.toString", e.To, "no toString call should remain ambiguous after fan-out")
	}
}

// TestResolveAll_ScopedEqualsFull_RelativeImports covers the scoped
// relative-import pass (a second filtered post-resolve pass): a Python relative
// import in the changed repo must resolve to the same internal file node under
// a scoped and a full resolve.
func TestResolveAll_ScopedEqualsFull_RelativeImports(t *testing.T) {
	build := func(s graph.Store) {
		s.AddNode(&graph.Node{ID: "repoa/pkg/app.py", Kind: graph.KindFile, Name: "app.py", FilePath: "repoa/pkg/app.py", Language: "python", RepoPrefix: "repoa"})
		s.AddNode(&graph.Node{ID: "repoa/pkg/util.py", Kind: graph.KindFile, Name: "util.py", FilePath: "repoa/pkg/util.py", Language: "python", RepoPrefix: "repoa"})
		s.AddEdge(&graph.Edge{From: "repoa/pkg/app.py", To: "unresolved::pyrel::repoa/pkg/util", Kind: graph.EdgeImports, FilePath: "repoa/pkg/app.py", Line: 1})
	}
	scopedStore := graph.New()
	build(scopedStore)
	twinStore := graph.New()
	build(twinStore)

	scoped := New(scopedStore)
	scoped.SetScope(map[string]struct{}{"repoa": {}})
	require.NotNil(t, scoped.ResolveAll())

	full := New(twinStore)
	require.NotNil(t, full.ResolveAll())

	assert.Equal(t, storeEdgeSet(twinStore), storeEdgeSet(scopedStore),
		"scoped and full relative-import resolution must produce identical edge sets")
	assert.True(t, hasEdgeKind(scopedStore, "repoa/pkg/app.py", "repoa/pkg/util.py", graph.EdgeImports),
		"the python relative import must resolve to the internal file under the scoped pass")
}

// TestScopedTailExceedsFileBudget pins the size gate that routes a large changed
// repo away from the per-file attribution storm: the gate fires when any repo in
// scope holds more KindFile nodes than the budget, and reads the already-built
// dirIndex rather than re-materializing the repo just to count.
func TestScopedTailExceedsFileBudget(t *testing.T) {
	s := graph.New()
	for i := 0; i < 3; i++ {
		f := fmt.Sprintf("repoa/pkg/f%d.go", i)
		s.AddNode(&graph.Node{ID: f, Kind: graph.KindFile, Name: fmt.Sprintf("f%d.go", i), FilePath: f, Language: "go", RepoPrefix: "repoa"})
	}
	s.AddNode(&graph.Node{ID: "repob/x/b.go", Kind: graph.KindFile, Name: "b.go", FilePath: "repob/x/b.go", Language: "go", RepoPrefix: "repob"})

	r := New(s)
	r.buildDirIndexes()
	defer r.clearDirIndexes()

	orig := scopedTailFileBudget
	defer func() { scopedTailFileBudget = orig }()

	r.SetScope(map[string]struct{}{"repoa": {}})
	scopedTailFileBudget = 2
	assert.True(t, r.scopedTailExceedsFileBudget(), "repoa's 3 files exceed a budget of 2")
	scopedTailFileBudget = 3
	assert.False(t, r.scopedTailExceedsFileBudget(), "repoa's 3 files do not exceed a budget of 3")

	// A small changed repo stays on the per-file path even when a large sibling
	// exists, because only in-scope repos are counted.
	r.SetScope(map[string]struct{}{"repob": {}})
	scopedTailFileBudget = 2
	assert.False(t, r.scopedTailExceedsFileBudget(), "repob's single file is within budget")
}

// TestResolveAll_ScopedLargeRepoEqualsFull is the correctness gate for the size
// guard: when a changed repo exceeds the file budget the scoped tail runs the
// whole-graph streaming passes instead of the per-file dispatch, and must still
// land the identical edge + node set a full resolve produces. buildGoTailFixture
// gives repo A two files, so a budget of 1 forces the streaming branch.
func TestResolveAll_ScopedLargeRepoEqualsFull(t *testing.T) {
	orig := scopedTailFileBudget
	scopedTailFileBudget = 1
	defer func() { scopedTailFileBudget = orig }()

	scopedStore := graph.New()
	buildGoTailFixture(scopedStore)
	twinStore := graph.New()
	buildGoTailFixture(twinStore)

	scoped := New(scopedStore)
	scoped.SetScope(map[string]struct{}{"repoa": {}})
	require.NotNil(t, scoped.ResolveAll())

	full := New(twinStore)
	require.NotNil(t, full.ResolveAll())

	assert.Equal(t, storeEdgeSet(twinStore), storeEdgeSet(scopedStore),
		"a scoped resolve that streams the large repo's tail must equal the full resolve")
	assert.Equal(t, storeNodeSet(twinStore), storeNodeSet(scopedStore))

	// The tail still attributed repo A's edges under the streaming branch.
	assert.False(t, hasCallEdge(scopedStore, "repoa/pkg/a.go::UsesLen", "unresolved::len"),
		"repo A's builtin call must be attributed under the streaming tail")
	assert.True(t, hasEdgeKind(scopedStore, "repoa/pkg/a2.go::Widget.Do", "repoa/pkg/a.go::Widget", graph.EdgeMemberOf),
		"repo A's phantom method receiver must rebind under the streaming tail")
}

// TestBuildImportClosureFiltered_Equivalence pins the closure-scoping contract
// the cross-package guard relies on: for every caller file whose repo is in the
// filter set, the filtered closure entry is byte-identical to the whole-graph
// build, and a nil filter reproduces the whole-graph closure exactly.
func TestBuildImportClosureFiltered_Equivalence(t *testing.T) {
	g := graph.New()
	// repoa file imports a sibling repoa package.
	g.AddNode(&graph.Node{ID: "repoa/x/a.go", Kind: graph.KindFile, Name: "a.go", FilePath: "repoa/x/a.go", Language: "go", RepoPrefix: "repoa"})
	g.AddNode(&graph.Node{ID: "repoa/y/dep.go", Kind: graph.KindFile, Name: "dep.go", FilePath: "repoa/y/dep.go", Language: "go", RepoPrefix: "repoa"})
	g.AddEdge(&graph.Edge{From: "repoa/x/a.go", To: "repoa/y/dep.go", Kind: graph.EdgeImports, FilePath: "repoa/x/a.go", Line: 1, Origin: graph.OriginASTResolved})
	// repob file imports a sibling repob package.
	g.AddNode(&graph.Node{ID: "repob/m/b.go", Kind: graph.KindFile, Name: "b.go", FilePath: "repob/m/b.go", Language: "go", RepoPrefix: "repob"})
	g.AddNode(&graph.Node{ID: "repob/n/dep.go", Kind: graph.KindFile, Name: "dep.go", FilePath: "repob/n/dep.go", Language: "go", RepoPrefix: "repob"})
	g.AddEdge(&graph.Edge{From: "repob/m/b.go", To: "repob/n/dep.go", Kind: graph.EdgeImports, FilePath: "repob/m/b.go", Line: 1, Origin: graph.OriginASTResolved})

	r := New(g)
	full := r.buildImportClosure()
	nilFiltered := r.buildImportClosureFiltered(nil)
	assert.Equal(t, full, nilFiltered, "nil filter must reproduce the whole-graph closure")

	scoped := r.buildImportClosureFiltered(map[string]struct{}{"repoa": {}})
	// Every repoa caller entry is identical to the whole-graph build...
	assert.Equal(t, full["repoa/x/a.go"], scoped["repoa/x/a.go"],
		"a caller in the filter set keeps its whole-graph reachable-dir set")
	require.Contains(t, scoped["repoa/x/a.go"], "repoa/y",
		"the imported sibling dir must be reachable")
	// ...and the out-of-scope repob caller is absent from the scoped closure.
	_, ok := scoped["repob/m/b.go"]
	assert.False(t, ok, "a caller outside the filter set is not seeded")
}

// hasEdgeKind reports whether s holds an edge of the given kind from → to.
func hasEdgeKind(s graph.Store, from, to string, kind graph.EdgeKind) bool {
	for _, e := range s.GetOutEdges(from) {
		if e.Kind == kind && e.To == to {
			return true
		}
	}
	return false
}

// hasBuiltinNode reports whether a KindBuiltin node named name was materialised.
func hasBuiltinNode(s graph.Store, name string) bool {
	for _, n := range s.AllNodes() {
		if n != nil && n.Kind == graph.KindBuiltin && n.Name == name {
			return true
		}
	}
	return false
}

// hasGoModuleNode reports whether a KindModule node named name was materialised.
func hasGoModuleNode(s graph.Store, name string) bool {
	for _, n := range s.AllNodes() {
		if n != nil && n.Kind == graph.KindModule && n.Name == name {
			return true
		}
	}
	return false
}
