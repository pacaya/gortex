package mcp

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/persistence"
)

func TestEligibleForRationale(t *testing.T) {
	base := persistence.MemoryEntry{Kind: "decision", Importance: 3, SymbolIDs: []string{"pkg/a.go::F"}}
	require.True(t, eligibleForRationale(base))

	superseded := base
	superseded.SupersededBy = "newer"
	require.False(t, eligibleForRationale(superseded), "superseded memories are not projected")

	wrongKind := base
	wrongKind.Kind = "reference"
	require.False(t, eligibleForRationale(wrongKind), "only decision/incident/constraint/invariant project")

	lowImportance := base
	lowImportance.Importance = 2
	require.False(t, eligibleForRationale(lowImportance), "low-importance, unpinned memories are skipped")

	pinned := base
	pinned.Importance = 1
	pinned.Pinned = true
	require.True(t, eligibleForRationale(pinned), "pinned memories project regardless of importance")

	noAnchor := base
	noAnchor.SymbolIDs = nil
	require.False(t, eligibleForRationale(noAnchor), "a memory with no anchor motivates nothing")

	incident := persistence.MemoryEntry{Kind: "incident", Importance: 4, FilePaths: []string{"pkg/a.go"}}
	require.True(t, eligibleForRationale(incident), "a file anchor is enough")
}

func TestProjectMemories(t *testing.T) {
	entries := []persistence.MemoryEntry{
		{
			ID: "m1", Kind: "decision", Importance: 5, Title: "Chose X over Y",
			Body:       "Because Z.\nmore detail",
			SymbolIDs:  []string{"pkg/a.go::F", "pkg/a.go::F"}, // duplicate anchor
			FilePaths:  []string{"pkg/a.go"},
			RepoPrefix: "repo",
		},
		{ID: "m2", Kind: "reference", Importance: 5, SymbolIDs: []string{"pkg/b.go::G"}}, // ineligible kind
		{ID: "m3", Kind: "invariant", Importance: 1},                                     // no anchor, ineligible
	}

	nodes, edges := projectMemories(entries)

	require.Len(t, nodes, 1, "only the eligible decision projects")
	n := nodes[0]
	require.Equal(t, "rationale::m1", n.ID)
	require.Equal(t, graph.KindRationale, n.Kind)
	require.Equal(t, "Chose X over Y", n.Name)
	require.Equal(t, "decision", n.Meta["rationale_kind"])
	require.Equal(t, "m1", n.Meta["memory_id"])
	require.Equal(t, rationaleVirtualFile, n.FilePath)
	require.Equal(t, "repo", n.RepoPrefix)

	require.Len(t, edges, 2, "duplicate symbol anchor deduped; file anchor kept")
	targets := make([]string, 0, len(edges))
	for _, e := range edges {
		require.Equal(t, "rationale::m1", e.From)
		require.Equal(t, graph.EdgeMotivates, e.Kind)
		require.Equal(t, "memory_projected", e.Origin)
		targets = append(targets, e.To)
	}
	require.ElementsMatch(t, []string{"pkg/a.go::F", "pkg/a.go"}, targets)
}

func TestProjectMemories_TitleFallsBackToFirstBodyLine(t *testing.T) {
	nodes, _ := projectMemories([]persistence.MemoryEntry{
		{ID: "m", Kind: "decision", Importance: 4, Body: "first line is the headline\nrest", SymbolIDs: []string{"x"}},
	})
	require.Len(t, nodes, 1)
	require.Equal(t, "first line is the headline", nodes[0].Name)
}
