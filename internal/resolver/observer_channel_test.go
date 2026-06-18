package resolver

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// observerChannelEdgeBetween finds a synthesized observer-channel call edge.
func observerChannelEdgeBetween(g graph.Store, from, to string) *graph.Edge {
	for _, e := range g.GetOutEdges(from) {
		if e.To == to && e.Kind == graph.EdgeCalls && e.Meta != nil {
			if v, _ := e.Meta["via"].(string); v == observerChannelVia {
				return e
			}
		}
	}
	return nil
}

func TestResolveObserverChannelCalls_PairsDispatcherToCallback(t *testing.T) {
	g := graph.New()
	// A Store with a callbacks field accessed by a registrar (subscribe) and a
	// dispatcher (notify).
	g.AddNode(&graph.Node{ID: "store.go::Store.callbacks", Kind: graph.KindField, Name: "callbacks", FilePath: "store.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "store.go::Store.subscribe", Kind: graph.KindMethod, Name: "subscribe", FilePath: "store.go", StartLine: 5, Language: "go"})
	g.AddNode(&graph.Node{ID: "store.go::Store.notify", Kind: graph.KindMethod, Name: "notify", FilePath: "store.go", StartLine: 9, Language: "go"})
	g.AddEdge(&graph.Edge{From: "store.go::Store.subscribe", To: "store.go::Store.callbacks", Kind: graph.EdgeAccessesField})
	g.AddEdge(&graph.Edge{From: "store.go::Store.notify", To: "store.go::Store.callbacks", Kind: graph.EdgeAccessesField})

	// A registration site: setup() calls Store.subscribe and passes handleRender
	// (both on the same line).
	g.AddNode(&graph.Node{ID: "app.go::setup", Kind: graph.KindFunction, Name: "setup", FilePath: "app.go", StartLine: 20, Language: "go"})
	g.AddNode(&graph.Node{ID: "app.go::handleRender", Kind: graph.KindFunction, Name: "handleRender", FilePath: "app.go", StartLine: 30, Language: "go"})
	g.AddEdge(&graph.Edge{From: "app.go::setup", To: "store.go::Store.subscribe", Kind: graph.EdgeCalls, FilePath: "app.go", Line: 22})
	g.AddEdge(&graph.Edge{From: "app.go::setup", To: "app.go::handleRender", Kind: graph.EdgeReads, FilePath: "app.go", Line: 22})

	n := ResolveObserverChannelCalls(g)
	assert.Equal(t, 1, n)

	e := observerChannelEdgeBetween(g, "store.go::Store.notify", "app.go::handleRender")
	require.NotNil(t, e, "notify (dispatcher) should call handleRender (registered callback)")
	assert.Equal(t, "store.go::Store.callbacks", e.Meta["channel_field"])
	assert.Equal(t, SynthObserverChannel, e.Meta[MetaSynthesizedBy])
	assert.Equal(t, graph.OriginASTInferred, e.Origin)
}

func TestResolveObserverChannelCalls_NoDispatcherNoEdge(t *testing.T) {
	g := graph.New()
	// A registrar with no dispatcher on the same field — no channel.
	g.AddNode(&graph.Node{ID: "s.go::S.cb", Kind: graph.KindField, Name: "cb", FilePath: "s.go"})
	g.AddNode(&graph.Node{ID: "s.go::S.subscribe", Kind: graph.KindMethod, Name: "subscribe", FilePath: "s.go", StartLine: 1})
	g.AddEdge(&graph.Edge{From: "s.go::S.subscribe", To: "s.go::S.cb", Kind: graph.EdgeAccessesField})
	g.AddNode(&graph.Node{ID: "a.go::setup", Kind: graph.KindFunction, Name: "setup", FilePath: "a.go"})
	g.AddNode(&graph.Node{ID: "a.go::h", Kind: graph.KindFunction, Name: "h", FilePath: "a.go"})
	g.AddEdge(&graph.Edge{From: "a.go::setup", To: "s.go::S.subscribe", Kind: graph.EdgeCalls, FilePath: "a.go", Line: 3})
	g.AddEdge(&graph.Edge{From: "a.go::setup", To: "a.go::h", Kind: graph.EdgeReads, FilePath: "a.go", Line: 3})

	assert.Equal(t, 0, ResolveObserverChannelCalls(g))
}

func TestResolveObserverChannelCalls_Idempotent(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "store.go::Store.callbacks", Kind: graph.KindField, Name: "callbacks", FilePath: "store.go"})
	g.AddNode(&graph.Node{ID: "store.go::Store.onUpdate", Kind: graph.KindMethod, Name: "onUpdate", FilePath: "store.go", StartLine: 5})
	g.AddNode(&graph.Node{ID: "store.go::Store.triggerUpdate", Kind: graph.KindMethod, Name: "triggerUpdate", FilePath: "store.go", StartLine: 9})
	g.AddEdge(&graph.Edge{From: "store.go::Store.onUpdate", To: "store.go::Store.callbacks", Kind: graph.EdgeAccessesField})
	g.AddEdge(&graph.Edge{From: "store.go::Store.triggerUpdate", To: "store.go::Store.callbacks", Kind: graph.EdgeAccessesField})
	g.AddNode(&graph.Node{ID: "app.go::setup", Kind: graph.KindFunction, Name: "setup", FilePath: "app.go"})
	g.AddNode(&graph.Node{ID: "app.go::render", Kind: graph.KindFunction, Name: "render", FilePath: "app.go"})
	g.AddEdge(&graph.Edge{From: "app.go::setup", To: "store.go::Store.onUpdate", Kind: graph.EdgeCalls, FilePath: "app.go", Line: 22})
	g.AddEdge(&graph.Edge{From: "app.go::setup", To: "app.go::render", Kind: graph.EdgeReferences, FilePath: "app.go", Line: 22})

	first := ResolveObserverChannelCalls(g)
	second := ResolveObserverChannelCalls(g)
	assert.Equal(t, first, second)

	count := 0
	for e := range g.EdgesByKind(graph.EdgeCalls) {
		if e != nil && e.Meta != nil {
			if v, _ := e.Meta["via"].(string); v == observerChannelVia {
				count++
			}
		}
	}
	assert.Equal(t, 1, count)
}
