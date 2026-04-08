package resolver

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"pgregory.net/rapid"
)

// --- Unit tests for CrossRepoResolver (Task 7.1) ---

func TestCrossRepoResolveAll_SameRepoPreferred(t *testing.T) {
	g := graph.New()

	// Repo A: caller and a target function.
	g.AddNode(&graph.Node{ID: "repoA/pkg/a.go::Caller", Kind: graph.KindFunction, Name: "Caller", FilePath: "repoA/pkg/a.go", Language: "go", RepoPrefix: "repoA"})
	g.AddNode(&graph.Node{ID: "repoA/pkg/b.go::Helper", Kind: graph.KindFunction, Name: "Helper", FilePath: "repoA/pkg/b.go", Language: "go", RepoPrefix: "repoA"})

	// Repo B: same-named function.
	g.AddNode(&graph.Node{ID: "repoB/lib/c.go::Helper", Kind: graph.KindFunction, Name: "Helper", FilePath: "repoB/lib/c.go", Language: "go", RepoPrefix: "repoB"})

	edge := &graph.Edge{From: "repoA/pkg/a.go::Caller", To: "unresolved::Helper", Kind: graph.EdgeCalls, FilePath: "repoA/pkg/a.go", Line: 5}
	g.AddEdge(edge)

	cr := NewCrossRepo(g)
	stats := cr.ResolveAll()

	assert.Equal(t, 1, stats.Resolved)
	assert.Equal(t, 0, stats.CrossRepoEdges)
	assert.Equal(t, "repoA/pkg/b.go::Helper", edge.To)
	assert.False(t, edge.CrossRepo)
}

func TestCrossRepoResolveAll_CrossRepoFallback(t *testing.T) {
	g := graph.New()

	// Repo A: caller, no matching function.
	g.AddNode(&graph.Node{ID: "repoA/pkg/a.go::Caller", Kind: graph.KindFunction, Name: "Caller", FilePath: "repoA/pkg/a.go", Language: "go", RepoPrefix: "repoA"})

	// Repo B: target function.
	g.AddNode(&graph.Node{ID: "repoB/lib/c.go::Helper", Kind: graph.KindFunction, Name: "Helper", FilePath: "repoB/lib/c.go", Language: "go", RepoPrefix: "repoB"})

	edge := &graph.Edge{From: "repoA/pkg/a.go::Caller", To: "unresolved::Helper", Kind: graph.EdgeCalls, FilePath: "repoA/pkg/a.go", Line: 5}
	g.AddEdge(edge)

	cr := NewCrossRepo(g)
	stats := cr.ResolveAll()

	assert.Equal(t, 1, stats.Resolved)
	assert.Equal(t, 1, stats.CrossRepoEdges)
	assert.Equal(t, "repoB/lib/c.go::Helper", edge.To)
	assert.True(t, edge.CrossRepo)
	assert.Equal(t, 1, stats.ByRepo["repoB"])
}

func TestCrossRepoResolveAll_Unresolvable(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "repoA/a.go::Foo", Kind: graph.KindFunction, Name: "Foo", FilePath: "repoA/a.go", Language: "go", RepoPrefix: "repoA"})

	edge := &graph.Edge{From: "repoA/a.go::Foo", To: "unresolved::NonExistent", Kind: graph.EdgeCalls, FilePath: "repoA/a.go", Line: 5}
	g.AddEdge(edge)

	cr := NewCrossRepo(g)
	stats := cr.ResolveAll()

	assert.Equal(t, 0, stats.Resolved)
	assert.Equal(t, 1, stats.Unresolved)
}

func TestCrossRepoResolveAll_ImportCrossRepo(t *testing.T) {
	g := graph.New()

	g.AddNode(&graph.Node{ID: "repoA/main.go", Kind: graph.KindFile, Name: "main.go", FilePath: "repoA/main.go", Language: "go", RepoPrefix: "repoA"})
	g.AddNode(&graph.Node{ID: "repoB/utils/utils.go", Kind: graph.KindPackage, Name: "utils", QualName: "utils", FilePath: "repoB/utils/utils.go", Language: "go", RepoPrefix: "repoB"})

	edge := &graph.Edge{From: "repoA/main.go", To: "unresolved::import::utils", Kind: graph.EdgeImports, FilePath: "repoA/main.go", Line: 3}
	g.AddEdge(edge)

	cr := NewCrossRepo(g)
	stats := cr.ResolveAll()

	assert.Equal(t, 1, stats.Resolved)
	assert.Equal(t, 1, stats.CrossRepoEdges)
	assert.Equal(t, "repoB/utils/utils.go", edge.To)
	assert.True(t, edge.CrossRepo)
}

func TestCrossRepoResolveAll_MethodCrossRepo(t *testing.T) {
	g := graph.New()

	g.AddNode(&graph.Node{ID: "repoA/pkg/a.go::Caller", Kind: graph.KindFunction, Name: "Caller", FilePath: "repoA/pkg/a.go", Language: "go", RepoPrefix: "repoA"})
	g.AddNode(&graph.Node{ID: "repoB/lib/b.go::Server.Start", Kind: graph.KindMethod, Name: "Start", FilePath: "repoB/lib/b.go", Language: "go", RepoPrefix: "repoB"})

	edge := &graph.Edge{From: "repoA/pkg/a.go::Caller", To: "unresolved::*.Start", Kind: graph.EdgeCalls, FilePath: "repoA/pkg/a.go", Line: 10}
	g.AddEdge(edge)

	cr := NewCrossRepo(g)
	stats := cr.ResolveAll()

	assert.Equal(t, 1, stats.Resolved)
	assert.Equal(t, 1, stats.CrossRepoEdges)
	assert.Equal(t, "repoB/lib/b.go::Server.Start", edge.To)
	assert.True(t, edge.CrossRepo)
}

func TestCrossRepoResolveForRepo(t *testing.T) {
	g := graph.New()

	// Repo A: caller with unresolved edge.
	g.AddNode(&graph.Node{ID: "repoA/a.go::Foo", Kind: graph.KindFunction, Name: "Foo", FilePath: "repoA/a.go", Language: "go", RepoPrefix: "repoA"})
	// Repo B: caller with unresolved edge + target.
	g.AddNode(&graph.Node{ID: "repoB/b.go::Bar", Kind: graph.KindFunction, Name: "Bar", FilePath: "repoB/b.go", Language: "go", RepoPrefix: "repoB"})
	g.AddNode(&graph.Node{ID: "repoB/b.go::Baz", Kind: graph.KindFunction, Name: "Baz", FilePath: "repoB/b.go", Language: "go", RepoPrefix: "repoB"})

	edgeA := &graph.Edge{From: "repoA/a.go::Foo", To: "unresolved::Baz", Kind: graph.EdgeCalls, FilePath: "repoA/a.go", Line: 5}
	edgeB := &graph.Edge{From: "repoB/b.go::Bar", To: "unresolved::Foo", Kind: graph.EdgeCalls, FilePath: "repoB/b.go", Line: 5}
	g.AddEdge(edgeA)
	g.AddEdge(edgeB)

	cr := NewCrossRepo(g)

	// Resolve only repoA edges.
	stats := cr.ResolveForRepo("repoA")

	assert.Equal(t, 1, stats.Resolved)
	assert.Equal(t, 1, stats.CrossRepoEdges)
	assert.Equal(t, "repoB/b.go::Baz", edgeA.To)
	assert.True(t, edgeA.CrossRepo)

	// edgeB should still be unresolved.
	assert.Equal(t, "unresolved::Foo", edgeB.To)
}

// --- Property test for Task 7.2 ---

// Feature: multi-repo-support, Property 10: Cross-repo resolution with same-repo preference
func TestPropertyCrossRepoResolutionSameRepoPreference(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		g := graph.New()

		// Generate a function name.
		funcName := "Func" + rapid.StringMatching(`[A-Z][a-z]{2,8}`).Draw(rt, "funcName")

		// Generate two repo prefixes.
		repoA := "repo-" + rapid.StringMatching(`[a-z]{3,6}`).Draw(rt, "repoA")
		repoB := "repo-" + rapid.StringMatching(`[a-z]{3,6}`).Draw(rt, "repoB")
		// Ensure distinct repos.
		if repoA == repoB {
			repoB = repoB + "x"
		}

		// Decide whether the caller's repo has a same-repo match.
		hasSameRepoMatch := rapid.Bool().Draw(rt, "hasSameRepoMatch")

		// Always add the caller in repoA.
		callerID := repoA + "/src/caller.go::" + "Caller"
		g.AddNode(&graph.Node{
			ID: callerID, Kind: graph.KindFunction, Name: "Caller",
			FilePath: repoA + "/src/caller.go", Language: "go", RepoPrefix: repoA,
		})

		// Always add the target in repoB (cross-repo candidate).
		crossRepoTargetID := repoB + "/lib/target.go::" + funcName
		g.AddNode(&graph.Node{
			ID: crossRepoTargetID, Kind: graph.KindFunction, Name: funcName,
			FilePath: repoB + "/lib/target.go", Language: "go", RepoPrefix: repoB,
		})

		// Optionally add a same-repo target in repoA.
		sameRepoTargetID := repoA + "/src/target.go::" + funcName
		if hasSameRepoMatch {
			g.AddNode(&graph.Node{
				ID: sameRepoTargetID, Kind: graph.KindFunction, Name: funcName,
				FilePath: repoA + "/src/target.go", Language: "go", RepoPrefix: repoA,
			})
		}

		// Add unresolved edge from caller.
		edge := &graph.Edge{
			From: callerID, To: "unresolved::" + funcName,
			Kind: graph.EdgeCalls, FilePath: repoA + "/src/caller.go", Line: 10,
		}
		g.AddEdge(edge)

		cr := NewCrossRepo(g)
		stats := cr.ResolveAll()

		require.Equal(rt, 1, stats.Resolved, "edge should be resolved")
		require.Equal(rt, 0, stats.Unresolved, "no edges should remain unresolved")

		if hasSameRepoMatch {
			// Same-repo match preferred.
			require.Equal(rt, sameRepoTargetID, edge.To,
				"same-repo match should be preferred")
			require.False(rt, edge.CrossRepo,
				"same-repo edge should not be marked cross-repo")
			require.Equal(rt, 0, stats.CrossRepoEdges,
				"no cross-repo edges when same-repo match exists")
		} else {
			// Cross-repo fallback.
			require.Equal(rt, crossRepoTargetID, edge.To,
				"cross-repo target should be used when no same-repo match")
			require.True(rt, edge.CrossRepo,
				"cross-repo edge must have CrossRepo == true")
			require.Equal(rt, 1, stats.CrossRepoEdges,
				"one cross-repo edge expected")
			// Target ID should be a Qualified_Node_ID containing the target's RepoPrefix.
			require.Contains(rt, edge.To, repoB+"/",
				"cross-repo target should use Qualified_Node_ID with target RepoPrefix")
		}
	})
}
