package analysis

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
)

// Feature: multi-repo-support, Property 18: Cross-repo impact traversal
//
// For any graph containing Cross_Repo_Edges, AnalyzeImpact SHALL follow those
// edges during BFS traversal and include affected symbols from other repositories
// in the result. The result SHALL group affected symbols by RepoPrefix and set
// cross_repo_impact to true when symbols from multiple repos are affected.
//
func TestPropertyCrossRepoImpactTraversal(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		g := graph.New()

		// Generate 2 repo prefixes.
		repoA := "repo-a"
		repoB := "repo-b"

		// Generate a random number of functions per repo (1-3).
		numFuncsA := rapid.IntRange(1, 3).Draw(rt, "numFuncsA")
		numFuncsB := rapid.IntRange(1, 3).Draw(rt, "numFuncsB")

		var repoAFuncs []string
		var repoBFuncs []string

		// Create repo-a functions.
		for i := 0; i < numFuncsA; i++ {
			id := fmt.Sprintf("%s/src/a.go::FuncA%d", repoA, i)
			g.AddNode(&graph.Node{
				ID:         id,
				Kind:       graph.KindFunction,
				Name:       fmt.Sprintf("FuncA%d", i),
				FilePath:   fmt.Sprintf("%s/src/a.go", repoA),
				Language:   "go",
				StartLine:  i*10 + 1,
				EndLine:    i*10 + 9,
				RepoPrefix: repoA,
			})
			repoAFuncs = append(repoAFuncs, id)
		}

		// Create repo-b functions.
		for i := 0; i < numFuncsB; i++ {
			id := fmt.Sprintf("%s/src/b.go::FuncB%d", repoB, i)
			g.AddNode(&graph.Node{
				ID:         id,
				Kind:       graph.KindFunction,
				Name:       fmt.Sprintf("FuncB%d", i),
				FilePath:   fmt.Sprintf("%s/src/b.go", repoB),
				Language:   "go",
				StartLine:  i*10 + 1,
				EndLine:    i*10 + 9,
				RepoPrefix: repoB,
			})
			repoBFuncs = append(repoBFuncs, id)
		}

		// Create a cross-repo edge: repo-b's first function calls repo-a's first function.
		g.AddEdge(&graph.Edge{
			From:      repoBFuncs[0],
			To:        repoAFuncs[0],
			Kind:      graph.EdgeCalls,
			CrossRepo: true,
		})

		// Optionally add intra-repo edges in repo-a.
		if numFuncsA > 1 {
			g.AddEdge(&graph.Edge{
				From: repoAFuncs[1],
				To:   repoAFuncs[0],
				Kind: graph.EdgeCalls,
			})
		}

		// Analyze impact starting from repo-a's first function.
		result := AnalyzeImpact(g, []string{repoAFuncs[0]}, nil, nil)
		require.NotNil(t, result)

		// The BFS should follow the cross-repo edge and find repo-b's function.
		foundCrossRepo := false
		for _, entries := range result.ByDepth {
			for _, entry := range entries {
				if entry.RepoPrefix == repoB {
					foundCrossRepo = true
				}
			}
		}
		if !foundCrossRepo {
			rt.Error("cross-repo edge was not followed: expected repo-b symbols in impact results")
		}

		// cross_repo_impact should be true since symbols from multiple repos are affected.
		if !result.CrossRepoImpact {
			rt.Error("CrossRepoImpact should be true when symbols from multiple repos are affected")
		}

		// ByRepo should group symbols by repo prefix.
		if result.ByRepo == nil {
			rt.Error("ByRepo should be populated when cross-repo impact is detected")
		}
		if len(result.ByRepo[repoB]) == 0 {
			rt.Error("ByRepo should contain repo-b entries")
		}
	})
}

// Feature: multi-repo-support, Property 19: Cross-repo safety checks
//
// For any signature change on a symbol that has callers in other repositories,
// VerifyChanges SHALL report violations for those cross-repo callers.
// For any changed symbol that has transitive test dependents in other repositories,
// get_test_targets SHALL include those cross-repo test files in the result.
//
func TestPropertyCrossRepoSafetyChecks(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		g := graph.New()

		repoA := "repo-a"
		repoB := "repo-b"

		// Create a function in repo-a with a signature.
		targetID := fmt.Sprintf("%s/lib.go::DoWork", repoA)
		g.AddNode(&graph.Node{
			ID:         targetID,
			Kind:       graph.KindFunction,
			Name:       "DoWork",
			FilePath:   fmt.Sprintf("%s/lib.go", repoA),
			Language:   "go",
			StartLine:  1,
			EndLine:    10,
			RepoPrefix: repoA,
			Meta:       map[string]any{"signature": "func DoWork(x int, y string)"},
		})

		// Create a caller in repo-b (cross-repo).
		callerID := fmt.Sprintf("%s/main.go::UsesDoWork", repoB)
		g.AddNode(&graph.Node{
			ID:         callerID,
			Kind:       graph.KindFunction,
			Name:       "UsesDoWork",
			FilePath:   fmt.Sprintf("%s/main.go", repoB),
			Language:   "go",
			StartLine:  5,
			EndLine:    15,
			RepoPrefix: repoB,
		})

		// Cross-repo call edge.
		g.AddEdge(&graph.Edge{
			From:      callerID,
			To:        targetID,
			Kind:      graph.EdgeCalls,
			CrossRepo: true,
		})

		// Create a test file in repo-b that depends on the caller.
		testID := fmt.Sprintf("%s/main_test.go::TestUsesDoWork", repoB)
		g.AddNode(&graph.Node{
			ID:         testID,
			Kind:       graph.KindFunction,
			Name:       "TestUsesDoWork",
			FilePath:   fmt.Sprintf("%s/main_test.go", repoB),
			Language:   "go",
			StartLine:  1,
			EndLine:    10,
			RepoPrefix: repoB,
		})
		g.AddEdge(&graph.Edge{
			From: testID,
			To:   callerID,
			Kind: graph.EdgeCalls,
		})

		// Generate a random parameter count change.
		newParamCount := rapid.IntRange(0, 1).Draw(rt, "newParamCount")
		var newSig string
		if newParamCount == 0 {
			newSig = "func DoWork()"
		} else {
			newSig = "func DoWork(x int)"
		}

		// Verify changes: the cross-repo caller should be reported as a violation.
		engine := query.NewEngine(g)
		result := VerifyChanges(g, engine, []SignatureChange{
			{SymbolID: targetID, NewSignature: newSig},
		})

		require.NotNil(t, result)

		// Should have violations from the cross-repo caller.
		foundCrossRepoCaller := false
		for _, v := range result.Violations {
			if v.RepoPrefix == repoB {
				foundCrossRepoCaller = true
			}
		}
		if !foundCrossRepoCaller {
			rt.Error("VerifyChanges should report violations for cross-repo callers")
		}

		// CrossRepoViolations flag should be set.
		if !result.CrossRepoViolations {
			rt.Error("CrossRepoViolations should be true when violations come from other repos")
		}

		// Test targets: AnalyzeImpact should include cross-repo test files.
		impact := AnalyzeImpact(g, []string{targetID}, nil, nil)
		require.NotNil(t, impact)

		foundTestFile := false
		for _, tf := range impact.TestFiles {
			if tf == fmt.Sprintf("%s/main_test.go", repoB) {
				foundTestFile = true
			}
		}
		if !foundTestFile {
			rt.Error("get_test_targets should include cross-repo test files")
		}
	})
}

// TestCrossRepoImpact_Unit is a focused unit test for cross-repo impact analysis.
func TestCrossRepoImpact_Unit(t *testing.T) {
	g := graph.New()

	// Repo A: library function
	g.AddNode(&graph.Node{ID: "repo-a/lib.go::Helper", Kind: graph.KindFunction, Name: "Helper",
		FilePath: "repo-a/lib.go", Language: "go", StartLine: 1, EndLine: 10, RepoPrefix: "repo-a"})

	// Repo B: consumer
	g.AddNode(&graph.Node{ID: "repo-b/app.go::UseHelper", Kind: graph.KindFunction, Name: "UseHelper",
		FilePath: "repo-b/app.go", Language: "go", StartLine: 1, EndLine: 10, RepoPrefix: "repo-b"})

	// Cross-repo edge
	g.AddEdge(&graph.Edge{From: "repo-b/app.go::UseHelper", To: "repo-a/lib.go::Helper", Kind: graph.EdgeCalls, CrossRepo: true})

	result := AnalyzeImpact(g, []string{"repo-a/lib.go::Helper"}, nil, nil)

	require.NotNil(t, result)
	assert.True(t, result.CrossRepoImpact)
	assert.Contains(t, result.ByRepo, "repo-b")
	assert.Len(t, result.ByRepo["repo-b"], 1)
	assert.Equal(t, "repo-b/app.go::UseHelper", result.ByRepo["repo-b"][0].ID)
}
