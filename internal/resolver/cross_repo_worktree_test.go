package resolver

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

// strictBoundary returns a non-nil cross-workspace lookup that declares
// no deps, so crossWorkspaceEligible enforces a hard workspace boundary.
// (A nil lookup is intentionally permissive, which would mask the
// worktree-instance preference under test.)
func strictBoundary() CrossWorkspaceDepLookup {
	return func(string) []CrossWorkspaceDepRule { return nil }
}

// worktreeImportGraph models the issue #47 cross-repo shape: module
// `oas-orm` is checked out twice — the canonical copy in workspace
// "base" and a worktree instance (its own prefix) in workspace "task" —
// and a caller in `callerWS` imports it. The two module nodes share a
// QualName, so the graph's single-valued qual-name index can only hold
// one of them; resolution must still bind to the caller's own workspace.
func worktreeImportGraph(callerWS string) (*graph.Graph, *graph.Edge) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "sherlock/main.go", Kind: graph.KindFile, Name: "main.go",
		FilePath: "sherlock/main.go", Language: "go", RepoPrefix: "sherlock", WorkspaceID: callerWS})
	// Canonical checkout (workspace "base").
	g.AddNode(&graph.Node{ID: "oas-orm/orm.go::orm", Kind: graph.KindPackage, Name: "orm", QualName: "oas-orm",
		FilePath: "oas-orm/orm.go", Language: "go", RepoPrefix: "oas-orm", WorkspaceID: "base"})
	// Worktree instance of the SAME module (workspace "task").
	g.AddNode(&graph.Node{ID: "oas-orm@task/orm.go::orm", Kind: graph.KindPackage, Name: "orm", QualName: "oas-orm",
		FilePath: "oas-orm@task/orm.go", Language: "go", RepoPrefix: "oas-orm@task", WorkspaceID: "task"})
	edge := &graph.Edge{From: "sherlock/main.go", To: "unresolved::import::oas-orm",
		Kind: graph.EdgeImports, FilePath: "sherlock/main.go", Line: 3}
	g.AddEdge(edge)
	return g, edge
}

// TestCrossRepoImport_ResolvesIntoCallerWorkspaceWorktree is the core
// resolver regression for issue #47: a caller in the task workspace
// importing a module that exists both canonically (another workspace)
// and as a worktree instance (its own workspace) must bind to the
// worktree instance — regardless of which copy the single-valued
// qual-name index happens to surface.
func TestCrossRepoImport_ResolvesIntoCallerWorkspaceWorktree(t *testing.T) {
	g, edge := worktreeImportGraph("task")
	cr := NewCrossRepo(g)
	cr.SetCrossWorkspaceDepLookup(strictBoundary())

	stats := cr.ResolveAll()

	require.Equal(t, 1, stats.Resolved)
	assert.Equal(t, "oas-orm@task/orm.go::orm", edge.To,
		"import from a task-workspace caller must bind to the worktree instance, not the canonical")
	assert.True(t, edge.CrossRepo)
}

// TestCrossRepoImport_ResolvesIntoCanonicalForBaseWorkspace is the
// mirror: a base-workspace caller binds to the canonical checkout, so
// the two instances coexist without bleeding into one another.
func TestCrossRepoImport_ResolvesIntoCanonicalForBaseWorkspace(t *testing.T) {
	g, edge := worktreeImportGraph("base")
	cr := NewCrossRepo(g)
	cr.SetCrossWorkspaceDepLookup(strictBoundary())

	stats := cr.ResolveAll()

	require.Equal(t, 1, stats.Resolved)
	assert.Equal(t, "oas-orm/orm.go::orm", edge.To,
		"import from the base-workspace caller must bind to the canonical checkout")
	assert.True(t, edge.CrossRepo)
}

// TestCrossRepoImport_UnrelatedWorkspaceStaysExternal guards the
// boundary: a caller in a third workspace that imports neither instance
// (no cross_workspace_deps) must stay external rather than mis-binding
// to whichever copy the qual-name index surfaced.
func TestCrossRepoImport_UnrelatedWorkspaceStaysExternal(t *testing.T) {
	g, edge := worktreeImportGraph("other")
	cr := NewCrossRepo(g)
	cr.SetCrossWorkspaceDepLookup(strictBoundary())

	stats := cr.ResolveAll()

	assert.Equal(t, 0, stats.CrossRepoEdges, "unrelated workspace must not bind into either instance")
	assert.Equal(t, "external::oas-orm", edge.To)
	assert.False(t, edge.CrossRepo)
}

// TestCrossRepoImport_DeclaredDepResolvesToCanonical confirms the
// existing cross_workspace_deps escape hatch still works alongside the
// worktree preference: when "task" has no own instance but declares a
// dep on "base", the import resolves across the boundary to the
// canonical copy.
func TestCrossRepoImport_DeclaredDepResolvesToCanonical(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "sherlock/main.go", Kind: graph.KindFile, Name: "main.go",
		FilePath: "sherlock/main.go", Language: "go", RepoPrefix: "sherlock", WorkspaceID: "task"})
	g.AddNode(&graph.Node{ID: "oas-orm/orm.go::orm", Kind: graph.KindPackage, Name: "orm", QualName: "oas-orm",
		FilePath: "oas-orm/orm.go", Language: "go", RepoPrefix: "oas-orm", WorkspaceID: "base"})
	edge := &graph.Edge{From: "sherlock/main.go", To: "unresolved::import::oas-orm",
		Kind: graph.EdgeImports, FilePath: "sherlock/main.go", Line: 3}
	g.AddEdge(edge)

	cr := NewCrossRepo(g)
	cr.SetCrossWorkspaceDepLookup(func(sourceWS string) []CrossWorkspaceDepRule {
		if sourceWS == "task" {
			return []CrossWorkspaceDepRule{{Workspace: "base", Modules: []string{"oas-orm"}}}
		}
		return nil
	})

	stats := cr.ResolveAll()

	require.Equal(t, 1, stats.Resolved)
	assert.Equal(t, "oas-orm/orm.go::orm", edge.To)
	assert.True(t, edge.CrossRepo)
}
