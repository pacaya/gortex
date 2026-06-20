package mcp

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

func TestWhyRank(t *testing.T) {
	require.Less(t, whyRank(whyEntry{Kind: "rationale"}), whyRank(whyEntry{Kind: "content"}),
		"curated rationale ranks before lexical content")
}

func TestWhyEntriesFor(t *testing.T) {
	srv, _ := setupTestServer(t)

	srv.graph.AddBatch([]*graph.Node{
		{ID: "pkg/x.go::Frobnicate", Kind: graph.KindFunction, Name: "Frobnicate", FilePath: "pkg/x.go"},
		{ID: "rationale::m1", Kind: graph.KindRationale, Name: "Batch writes for throughput",
			Meta: map[string]any{"rationale_kind": "decision", "section_text": "We batch writes because the DB round-trip dominates."}},
		{ID: "design.pptx::doc:slide-1", Kind: graph.KindDoc, FilePath: "design.pptx",
			Meta: map[string]any{"data_class": "content", "asset_kind": "slide", "section_text": "Frobnicate batches writes."}},
	}, []*graph.Edge{
		{From: "design.pptx::doc:slide-1", To: "pkg/x.go::Frobnicate", Kind: graph.EdgeMotivates, Meta: map[string]any{"signal": "lexical"}},
		{From: "rationale::m1", To: "pkg/x.go::Frobnicate", Kind: graph.EdgeMotivates, Meta: map[string]any{"signal": "memory_projected"}},
	})

	entries := srv.whyEntriesFor(context.Background(), "pkg/x.go::Frobnicate")
	require.Len(t, entries, 2)

	require.Equal(t, "rationale", entries[0].Kind, "curated rationale ranks first")
	require.Equal(t, "rationale::m1", entries[0].SourceID)
	require.Equal(t, "decision", entries[0].RationaleKind)
	require.Equal(t, "memory_projected", entries[0].Signal)

	require.Equal(t, "content", entries[1].Kind)
	require.Equal(t, "design.pptx::doc:slide-1", entries[1].SourceID)
	require.Equal(t, "slide", entries[1].AssetKind)
	require.Equal(t, "lexical", entries[1].Signal)
}

func TestWhyEntriesFor_NoRationale(t *testing.T) {
	srv, _ := setupTestServer(t)
	require.Empty(t, srv.whyEntriesFor(context.Background(), "main.go::helper"),
		"a symbol with no motivating knowledge yields no entries")
}
