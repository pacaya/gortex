package resolver

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

func springListener(g *graph.Graph, id, file, evType string) {
	g.AddNode(&graph.Node{ID: id, Kind: graph.KindMethod, Name: lastSeg(id), FilePath: file, Language: "java",
		Meta: map[string]any{"spring_listener_type": evType}})
}

func springPublish(g *graph.Graph, fromID, file, evType string) {
	if g.GetNode(fromID) == nil {
		g.AddNode(&graph.Node{ID: fromID, Kind: graph.KindMethod, Name: lastSeg(fromID), FilePath: file, Language: "java"})
	}
	g.AddEdge(&graph.Edge{From: fromID, To: "unresolved::*." + evType, Kind: graph.EdgeCalls, FilePath: file,
		Meta: map[string]any{"via": springEventVia, "spring_event_type": evType}})
}

func synthSpringEdge(g graph.Store, from, to string) *graph.Edge {
	for e := range g.EdgesByKind(graph.EdgeCalls) {
		if e == nil || e.From != from || e.To != to || e.Meta == nil {
			continue
		}
		if by, _ := e.Meta[MetaSynthesizedBy].(string); by == SynthSpringEvent {
			return e
		}
	}
	return nil
}

func TestResolveSpringEventCalls_TypeKeyedFanout(t *testing.T) {
	g := graph.New()
	springListener(g, "App.java::MailListener.on", "App.java", "OrderPlaced")
	springListener(g, "App.java::AuditListener.onApplicationEvent", "App.java", "OrderPlaced")
	springPublish(g, "App.java::OrderService.place", "App.java", "OrderPlaced")

	n := ResolveSpringEventCalls(g)
	require.Equal(t, 2, n, "both listeners receive an edge")

	a := synthSpringEdge(g, "App.java::OrderService.place", "App.java::MailListener.on")
	require.NotNil(t, a)
	assert.Equal(t, ConfidenceTyped, a.Confidence)
	assert.Equal(t, ProvenanceFramework, a.Meta[MetaProvenance])
	assert.NotNil(t, synthSpringEdge(g, "App.java::OrderService.place", "App.java::AuditListener.onApplicationEvent"))
}

func TestResolveSpringEventCalls_SubtypeReachesBaseListener(t *testing.T) {
	// A listener for the base event type receives a published subtype when
	// the type-hierarchy edge is present.
	g := graph.New()
	springListener(g, "App.java::Base.on", "App.java", "OrderEvent")
	springPublish(g, "App.java::Svc.fire", "App.java", "OrderPlaced")
	// OrderPlaced extends OrderEvent.
	g.AddNode(&graph.Node{ID: "App.java::OrderPlaced", Kind: graph.KindType, Name: "OrderPlaced", FilePath: "App.java"})
	g.AddEdge(&graph.Edge{From: "App.java::OrderPlaced", To: "unresolved::OrderEvent", Kind: graph.EdgeExtends, FilePath: "App.java"})

	n := ResolveSpringEventCalls(g)
	require.Equal(t, 1, n)
	assert.NotNil(t, synthSpringEdge(g, "App.java::Svc.fire", "App.java::Base.on"),
		"subtype publish reaches the base-type listener")
}

func TestResolveSpringEventCalls_NoListenerStaysPlaceholder(t *testing.T) {
	g := graph.New()
	springListener(g, "App.java::L.on", "App.java", "SomeEvent")
	springPublish(g, "App.java::S.fire", "App.java", "OtherEvent")

	assert.Equal(t, 0, ResolveSpringEventCalls(g))
	assert.Nil(t, synthSpringEdge(g, "App.java::S.fire", "App.java::L.on"))
}
