package resolver

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// eventChannelTestGraph builds the minimal pub/sub shape the
// ResolveEventChannelCalls pass consumes: emitter/listener function
// nodes plus EdgeEmits / EdgeListensOn edges to a shared KindEvent topic
// node.
type eventChannelTestGraph struct{ g graph.Store }

func newEventChannelTestGraph() *eventChannelTestGraph {
	return &eventChannelTestGraph{g: graph.New()}
}

func (b *eventChannelTestGraph) fn(id, filePath string) {
	b.g.AddNode(&graph.Node{ID: id, Kind: graph.KindFunction, Name: lastSeg(id), FilePath: filePath})
}

func (b *eventChannelTestGraph) eventNode(transport, topic string) string {
	id := "event::pubsub::" + transport + "::" + topic
	if b.g.GetNode(id) == nil {
		b.g.AddNode(&graph.Node{ID: id, Kind: graph.KindEvent, Name: topic, Meta: map[string]any{"transport": transport, "event_kind": "pubsub"}})
	}
	return id
}

func (b *eventChannelTestGraph) emit(fromID, transport, topic, filePath string, line int) {
	b.fn(fromID, filePath)
	to := b.eventNode(transport, topic)
	b.g.AddEdge(&graph.Edge{From: fromID, To: to, Kind: graph.EdgeEmits, FilePath: filePath, Line: line, Meta: map[string]any{"transport": transport}})
}

func (b *eventChannelTestGraph) listen(fromID, transport, topic, filePath string, line int) {
	b.fn(fromID, filePath)
	to := b.eventNode(transport, topic)
	b.g.AddEdge(&graph.Edge{From: fromID, To: to, Kind: graph.EdgeListensOn, FilePath: filePath, Line: line, Meta: map[string]any{"transport": transport}})
}

// synthEventEdge returns the synthesized event-channel calls edge between
// from and to, or nil.
func synthEventEdge(g graph.Store, from, to string) *graph.Edge {
	for e := range g.EdgesByKind(graph.EdgeCalls) {
		if e == nil || e.From != from || e.To != to || e.Meta == nil {
			continue
		}
		if v, _ := e.Meta["via"].(string); v == eventChannelVia {
			return e
		}
	}
	return nil
}

func TestResolveEventChannelCalls_PairsInProcessEmitterToListener(t *testing.T) {
	b := newEventChannelTestGraph()
	b.emit("pub/order.go::placeOrder", "eventemitter", "order.created", "pub/order.go", 10)
	b.listen("sub/mailer.go::onOrder", "eventemitter", "order.created", "sub/mailer.go", 20)

	n := ResolveEventChannelCalls(b.g)
	assert.Equal(t, 1, n)

	e := synthEventEdge(b.g, "pub/order.go::placeOrder", "sub/mailer.go::onOrder")
	require.NotNil(t, e, "emitter must reach the listener via a synthesized call edge")
	assert.Equal(t, graph.OriginASTInferred, e.Origin)
	assert.Equal(t, "order.created", e.Meta["event_topic"])
	assert.Equal(t, "eventemitter", e.Meta["event_transport"])
	assert.Equal(t, SynthEventChannel, e.Meta[MetaSynthesizedBy])
	assert.Equal(t, ProvenanceHeuristic, e.Meta[MetaProvenance])
	// The listener sees the inbound synthesized edge.
	require.Len(t, b.g.GetInEdges("sub/mailer.go::onOrder"), 1)
}

func TestResolveEventChannelCalls_FanOutAcrossListeners(t *testing.T) {
	b := newEventChannelTestGraph()
	b.emit("pub/order.go::placeOrder", "socketio", "order", "pub/order.go", 10)
	b.listen("a.go::a", "socketio", "order", "a.go", 1)
	b.listen("b.go::b", "socketio", "order", "b.go", 1)

	n := ResolveEventChannelCalls(b.g)
	assert.Equal(t, 2, n)
	assert.NotNil(t, synthEventEdge(b.g, "pub/order.go::placeOrder", "a.go::a"))
	assert.NotNil(t, synthEventEdge(b.g, "pub/order.go::placeOrder", "b.go::b"))
}

func TestResolveEventChannelCalls_NativeBridgeTransportPaired(t *testing.T) {
	// A native (Swift/ObjC/Kotlin) sendEvent registered under an rn_*
	// transport must pair with the JS addListener handler — the
	// cross-language case.
	b := newEventChannelTestGraph()
	b.emit("ios/Native.swift::Native.sendBattery", "rn_native_event", "battery", "ios/Native.swift", 30)
	b.listen("js/app.ts::onBattery", "rn_native_event", "battery", "js/app.ts", 5)

	n := ResolveEventChannelCalls(b.g)
	assert.Equal(t, 1, n)
	assert.NotNil(t, synthEventEdge(b.g, "ios/Native.swift::Native.sendBattery", "js/app.ts::onBattery"))
}

func TestResolveEventChannelCalls_SkipsBrokerTransports(t *testing.T) {
	// Kafka / NATS / RabbitMQ / Redis are paired by the contracts
	// producer↔consumer layer (EdgeProducesTopic / EdgeConsumesTopic);
	// this pass must not double-cover them.
	for _, transport := range []string{"kafka", "nats", "rabbitmq", "redis", "unknown"} {
		b := newEventChannelTestGraph()
		b.emit("p.go::p", transport, "t", "p.go", 1)
		b.listen("c.go::c", transport, "t", "c.go", 1)
		assert.Equal(t, 0, ResolveEventChannelCalls(b.g), "transport %q must not be paired here", transport)
	}
}

func TestResolveEventChannelCalls_NoSelfEdge(t *testing.T) {
	b := newEventChannelTestGraph()
	// Same function both emits and listens on the topic.
	b.emit("x.go::x", "eventemitter", "tick", "x.go", 1)
	b.listen("x.go::x", "eventemitter", "tick", "x.go", 2)
	assert.Equal(t, 0, ResolveEventChannelCalls(b.g), "a function must not call itself via the event channel")
}

func TestResolveEventChannelCalls_Idempotent(t *testing.T) {
	b := newEventChannelTestGraph()
	b.emit("p.go::p", "eventemitter", "e", "p.go", 1)
	b.listen("c.go::c", "eventemitter", "e", "c.go", 1)
	first := ResolveEventChannelCalls(b.g)
	second := ResolveEventChannelCalls(b.g)
	assert.Equal(t, first, second, "pass count is stable across runs")
	// Exactly one synthesized edge survives (AddEdge dedupes by key).
	count := 0
	for e := range b.g.EdgesByKind(graph.EdgeCalls) {
		if e != nil && e.Meta != nil {
			if v, _ := e.Meta["via"].(string); v == eventChannelVia {
				count++
			}
		}
	}
	assert.Equal(t, 1, count)
}

func TestResolveEventChannelCalls_FanOutCap(t *testing.T) {
	b := newEventChannelTestGraph()
	b.emit("p.go::p", "eventemitter", "busy", "p.go", 1)
	for i := range maxEventChannelFanout + 1 {
		b.listen("l.go::l"+strconv.Itoa(i), "eventemitter", "busy", "l.go", i+1)
	}
	assert.Equal(t, 0, ResolveEventChannelCalls(b.g), "a pathological fan-out channel is skipped, not exploded")
}
