package mcp

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

func TestAnalyzeDocStaleness(t *testing.T) {
	g := graph.New()
	g.AddBatch([]*graph.Node{
		{ID: "pkg/a.go::Live", Kind: graph.KindFunction, Name: "Live"},
		{ID: "deck.pptx::doc:slide-1", Kind: graph.KindDoc, FilePath: "deck.pptx", Name: "deck slide 1",
			Meta: map[string]any{"data_class": "content"}},
		{ID: "rationale::m1", Kind: graph.KindRationale, Name: "old decision"},
		{ID: "fresh.pptx::doc:slide-1", Kind: graph.KindDoc, FilePath: "fresh.pptx", Name: "fresh slide",
			Meta: map[string]any{"data_class": "content"}},
	}, []*graph.Edge{
		{From: "deck.pptx::doc:slide-1", To: "pkg/a.go::Live", Kind: graph.EdgeMotivates},
		{From: "deck.pptx::doc:slide-1", To: "pkg/a.go::Gone", Kind: graph.EdgeMotivates}, // deleted symbol
		{From: "rationale::m1", To: "unresolved::Frob", Kind: graph.EdgeMotivates},        // pending stub
		{From: "fresh.pptx::doc:slide-1", To: "pkg/a.go::Live", Kind: graph.EdgeMotivates},
	})

	res := analyzeDocStaleness(g, 50)
	require.Equal(t, 4, res.AssessedLinks)
	require.Len(t, res.Stale, 2, "deck (1 dangling) + rationale (1 pending); the all-live source is excluded")

	require.Equal(t, "deck.pptx::doc:slide-1", res.Stale[0].Source, "dangling ranks above pending")
	require.Equal(t, "dangling", res.Stale[0].WorstState)
	require.Equal(t, 1, res.Stale[0].Dangling)
	require.Equal(t, 2, res.Stale[0].TotalRefs)

	require.Equal(t, "rationale::m1", res.Stale[1].Source)
	require.Equal(t, "pending", res.Stale[1].WorstState)
	require.Equal(t, 1, res.Stale[1].Pending)
}

func TestAnalyzeDocStaleness_Empty(t *testing.T) {
	res := analyzeDocStaleness(graph.New(), 50)
	require.Zero(t, res.AssessedLinks)
	require.Empty(t, res.Stale)
	require.NotEmpty(t, res.Note)
}
