package semantic

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

func TestConfirmEdge(t *testing.T) {
	e := &graph.Edge{
		From:            "a.go::Foo",
		To:              "b.go::Bar",
		Kind:            graph.EdgeCalls,
		Confidence:      0.6,
		ConfidenceLabel: "INFERRED",
	}

	ConfirmEdge(e, "test-provider")

	assert.Equal(t, 1.0, e.Confidence)
	assert.Equal(t, "EXTRACTED", e.ConfidenceLabel)
	assert.Equal(t, "test-provider", e.Meta["semantic_source"])
}

func TestAddSemanticEdge(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "a.go::Foo", Kind: graph.KindFunction, Name: "Foo", FilePath: "a.go"})
	g.AddNode(&graph.Node{ID: "b.go::Bar", Kind: graph.KindFunction, Name: "Bar", FilePath: "b.go"})

	e := AddSemanticEdge(g, "a.go::Foo", "b.go::Bar", graph.EdgeCalls, "a.go", 10, "test")

	assert.Equal(t, 1.0, e.Confidence)
	assert.Equal(t, "EXTRACTED", e.ConfidenceLabel)
	assert.Equal(t, "test", e.Meta["semantic_source"])

	// Verify it's in the graph.
	edges := g.GetOutEdges("a.go::Foo")
	require.Len(t, edges, 1)
	assert.Equal(t, "b.go::Bar", edges[0].To)
}

func TestRefuteEdge(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "a.go::Foo", Kind: graph.KindFunction, Name: "Foo", FilePath: "a.go"})
	g.AddNode(&graph.Node{ID: "b.go::Bar", Kind: graph.KindFunction, Name: "Bar", FilePath: "b.go"})
	g.AddEdge(&graph.Edge{From: "a.go::Foo", To: "b.go::Bar", Kind: graph.EdgeCalls, Confidence: 0.5})

	e := &graph.Edge{From: "a.go::Foo", To: "b.go::Bar", Kind: graph.EdgeCalls}
	removed := RefuteEdge(g, e)

	assert.True(t, removed)
	assert.Empty(t, g.GetOutEdges("a.go::Foo"))
}

func TestFindMatchingEdge(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "a.go::Foo", Kind: graph.KindFunction, Name: "Foo", FilePath: "a.go"})
	g.AddNode(&graph.Node{ID: "b.go::Bar", Kind: graph.KindFunction, Name: "Bar", FilePath: "b.go"})
	g.AddEdge(&graph.Edge{From: "a.go::Foo", To: "b.go::Bar", Kind: graph.EdgeCalls})

	found := FindMatchingEdge(g, "a.go::Foo", "b.go::Bar", graph.EdgeCalls)
	assert.NotNil(t, found)

	notFound := FindMatchingEdge(g, "a.go::Foo", "b.go::Bar", graph.EdgeReferences)
	assert.Nil(t, notFound)
}

func TestEnrichNodeMeta(t *testing.T) {
	n := &graph.Node{ID: "a.go::Foo", Kind: graph.KindFunction, Name: "Foo"}

	EnrichNodeMeta(n, "semantic_type", "func() error", "test")

	assert.Equal(t, "func() error", n.Meta["semantic_type"])
	assert.Equal(t, "test", n.Meta["semantic_source"])
}

func TestNodesByLanguage(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "a.go::Foo", Kind: graph.KindFunction, Name: "Foo", Language: "go"})
	g.AddNode(&graph.Node{ID: "b.ts::Bar", Kind: graph.KindFunction, Name: "Bar", Language: "typescript"})
	g.AddNode(&graph.Node{ID: "c.go::Baz", Kind: graph.KindFunction, Name: "Baz", Language: "go"})

	goNodes := NodesByLanguage(g, "go")
	assert.Len(t, goNodes, 2)

	tsNodes := NodesByLanguage(g, "typescript")
	assert.Len(t, tsNodes, 1)
}
