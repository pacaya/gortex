package resolver

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// getFileNodesCounter wraps a graph.Store and counts GetFileNodes calls so a
// test can prove the per-file receiver rebind builds a package's type index
// once (O(files)) rather than rebuilding it per file (O(files^2)).
type getFileNodesCounter struct {
	graph.Store
	calls int
}

func (c *getFileNodesCounter) GetFileNodes(fp string) []*graph.Node {
	c.calls++
	return c.Store.GetFileNodes(fp)
}

// TestRebindGoMethodReceiversForFile_MemoizesPackageTypeIndex is the regression
// for the scoped-tail quadratic: dispatching the per-file receiver rebind over
// every file of a D-file package rebuilt the package's type index from scratch
// each time — D GetFileNodes per file, D^2 for the package. The index is a pure
// function of the directory and is not mutated by the tail, so it must be built
// once and reused, collapsing the package cost back to O(D).
func TestRebindGoMethodReceiversForFile_MemoizesPackageTypeIndex(t *testing.T) {
	const dfiles = 12
	dir := "repoa/pkg"
	typeFile := dir + "/t0.go"

	cs := &getFileNodesCounter{Store: graph.New()}
	// The canonical type T lives in file 0; every other file declares a method
	// on T whose receiver is a phantom <methodfile>::T the rebind must lift onto
	// file 0's T — the cross-file-method shape the pass exists to fix.
	cs.AddNode(&graph.Node{ID: typeFile, Kind: graph.KindFile, Name: "t0.go", FilePath: typeFile, Language: "go", RepoPrefix: "repoa"})
	cs.AddNode(&graph.Node{ID: typeFile + "::T", Kind: graph.KindType, Name: "T", FilePath: typeFile, Language: "go", RepoPrefix: "repoa"})
	for i := 1; i < dfiles; i++ {
		f := fmt.Sprintf("%s/m%d.go", dir, i)
		cs.AddNode(&graph.Node{ID: f, Kind: graph.KindFile, Name: filepath.Base(f), FilePath: f, Language: "go", RepoPrefix: "repoa"})
		m := fmt.Sprintf("%s::T.M%d", f, i)
		cs.AddNode(&graph.Node{ID: m, Kind: graph.KindMethod, Name: fmt.Sprintf("M%d", i), FilePath: f, Language: "go", RepoPrefix: "repoa"})
		cs.AddEdge(&graph.Edge{From: m, To: f + "::T", Kind: graph.EdgeMemberOf, FilePath: f, Line: 1})
	}

	r := New(cs)
	r.buildDirIndexes()
	defer r.clearDirIndexes()

	files := []string{typeFile}
	for i := 1; i < dfiles; i++ {
		files = append(files, fmt.Sprintf("%s/m%d.go", dir, i))
	}
	cs.calls = 0
	for _, f := range files {
		r.rebindGoMethodReceiversForFile(f)
	}

	// Memoized: the type index is built once (dfiles GetFileNodes) plus one
	// memberOf fetch per file — ~2·D. The quadratic rebuild would be ~D·(D+1)
	// (132 for D=12); 3·D (36) sits well below it and above the linear cost.
	assert.LessOrEqualf(t, cs.calls, 3*dfiles,
		"per-file receiver rebind rebuilt the package type index per file (%d GetFileNodes for %d files)", cs.calls, dfiles)

	// Correctness: every phantom cross-file receiver rebound onto file 0's T.
	for i := 1; i < dfiles; i++ {
		f := fmt.Sprintf("%s/m%d.go", dir, i)
		assert.Truef(t, hasEdgeKind(cs, fmt.Sprintf("%s::T.M%d", f, i), typeFile+"::T", graph.EdgeMemberOf),
			"method M%d's receiver must rebind onto the canonical type", i)
	}
}

// TestRebindGoMethodReceivers_CollapsesCrossFileMethods is the
// regression for the Go extractor emitting EdgeMemberOf targets as
// <methodfile>::TypeName. When methods on the same type live in
// different files of the same package, the parser produces a phantom
// type ID per method-file; the rebind pass must collapse them onto
// the canonical <typefile>::TypeName node so InferImplements and the
// downstream MCP tools (find_implementations, class_hierarchy) see
// the consolidated method set.
func TestRebindGoMethodReceivers_CollapsesCrossFileMethods(t *testing.T) {
	g := graph.New()

	// Type defined in indexer.go.
	typeID := "internal/indexer/indexer.go::Indexer"
	g.AddNode(&graph.Node{
		ID: typeID, Kind: graph.KindType, Name: "Indexer",
		FilePath: "internal/indexer/indexer.go", Language: "go",
	})

	// Method declared in a *different* file in the same package — the
	// parser emits a phantom receiver target.
	methodID := "internal/indexer/crash_isolation.go::Indexer.crashIsolationEnabled"
	g.AddNode(&graph.Node{
		ID: methodID, Kind: graph.KindMethod, Name: "crashIsolationEnabled",
		FilePath: "internal/indexer/crash_isolation.go", Language: "go",
	})
	phantomTarget := "internal/indexer/crash_isolation.go::Indexer"
	memberEdge := &graph.Edge{
		From: methodID, To: phantomTarget, Kind: graph.EdgeMemberOf,
		FilePath: "internal/indexer/crash_isolation.go", Line: 23,
	}
	g.AddEdge(memberEdge)

	// Sanity: pre-pass the phantom target has no real node.
	require.Nil(t, g.GetNode(phantomTarget), "phantom target must not exist as a real node")

	r := New(g)
	r.rebindGoMethodReceivers()

	// Post-pass: the edge points at the canonical type node.
	assert.Equal(t, typeID, memberEdge.To,
		"EdgeMemberOf must be rewritten from <methodfile>::Type to canonical <typefile>::Type")

	// And the same-file method on the type works too — covered by not
	// breaking a control case:
	g2 := graph.New()
	g2.AddNode(&graph.Node{
		ID: "pkg/foo.go::Foo", Kind: graph.KindType, Name: "Foo",
		FilePath: "pkg/foo.go", Language: "go",
	})
	g2.AddNode(&graph.Node{
		ID: "pkg/foo.go::Foo.Bar", Kind: graph.KindMethod, Name: "Bar",
		FilePath: "pkg/foo.go", Language: "go",
	})
	sameFileEdge := &graph.Edge{
		From: "pkg/foo.go::Foo.Bar", To: "pkg/foo.go::Foo",
		Kind: graph.EdgeMemberOf, FilePath: "pkg/foo.go", Line: 5,
	}
	g2.AddEdge(sameFileEdge)

	New(g2).rebindGoMethodReceivers()
	assert.Equal(t, "pkg/foo.go::Foo", sameFileEdge.To,
		"same-file method edge must be left unchanged")
}

// TestRebindGoMethodReceivers_LanguageGated guards against the pass
// rewriting non-Go EdgeMemberOf edges. Java/TS/Python group methods
// in the class body so their EdgeMemberOf targets are already
// in-file; we don't want the pass touching them.
func TestRebindGoMethodReceivers_LanguageGated(t *testing.T) {
	g := graph.New()

	// A type and a method in the same Go package — would normally be
	// a rebind candidate.
	g.AddNode(&graph.Node{
		ID: "pkg/types.go::Server", Kind: graph.KindType, Name: "Server",
		FilePath: "pkg/types.go", Language: "go",
	})
	// But the METHOD is declared as TypeScript (e.g. a TS extractor
	// that emits the same EdgeMemberOf shape for some bridging
	// reason). Pass must leave it alone.
	tsMethod := &graph.Node{
		ID: "pkg/handler.ts::Server.serve", Kind: graph.KindMethod, Name: "serve",
		FilePath: "pkg/handler.ts", Language: "typescript",
	}
	g.AddNode(tsMethod)
	edge := &graph.Edge{
		From: tsMethod.ID, To: "pkg/handler.ts::Server",
		Kind: graph.EdgeMemberOf, FilePath: "pkg/handler.ts", Line: 1,
	}
	g.AddEdge(edge)

	New(g).rebindGoMethodReceivers()
	assert.Equal(t, "pkg/handler.ts::Server", edge.To,
		"non-Go method edge must NOT be rewritten by the Go-only rebind pass")
}

// TestRebindGoMethodReceivers_AmbiguousNameSkipped guards against the
// pass picking an arbitrary winner when two distinct types share the
// same name in the same package (shouldn't happen in valid Go, but
// the pass should leave the phantom alone rather than mis-bind).
func TestRebindGoMethodReceivers_AmbiguousNameSkipped(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{
		ID: "pkg/a.go::Dup", Kind: graph.KindType, Name: "Dup",
		FilePath: "pkg/a.go", Language: "go",
	})
	g.AddNode(&graph.Node{
		ID: "pkg/b.go::Dup", Kind: graph.KindType, Name: "Dup",
		FilePath: "pkg/b.go", Language: "go",
	})
	g.AddNode(&graph.Node{
		ID: "pkg/c.go::Dup.M", Kind: graph.KindMethod, Name: "M",
		FilePath: "pkg/c.go", Language: "go",
	})
	edge := &graph.Edge{
		From: "pkg/c.go::Dup.M", To: "pkg/c.go::Dup",
		Kind: graph.EdgeMemberOf, FilePath: "pkg/c.go", Line: 1,
	}
	g.AddEdge(edge)

	New(g).rebindGoMethodReceivers()
	assert.Equal(t, "pkg/c.go::Dup", edge.To,
		"ambiguous type name in same package must leave the edge phantom rather than guess")
}
