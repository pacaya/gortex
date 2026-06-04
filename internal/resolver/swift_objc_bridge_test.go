package resolver

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

func swiftMethodNode(g graph.Store, id, selector string) {
	g.AddNode(&graph.Node{
		ID: id, Kind: graph.KindMethod, Name: lastSeg(id), FilePath: "ios/App.swift", StartLine: 10,
		Language: "swift", Meta: map[string]any{"objc_selector": selector},
	})
}

func objcMethodNode(g graph.Store, id, selector string) {
	g.AddNode(&graph.Node{
		ID: id, Kind: graph.KindMethod, Name: selector, FilePath: "ios/Legacy.m", StartLine: 5,
		Language: "objc",
	})
}

func bridgeEdgeBetween(g graph.Store, from, to string) *graph.Edge {
	for _, e := range g.GetOutEdges(from) {
		if e.To == to && e.Kind == graph.EdgeReferences && e.Meta != nil {
			if v, _ := e.Meta["via"].(string); v == swiftObjCBridgeVia {
				return e
			}
		}
	}
	return nil
}

func TestResolveSwiftObjCBridge_BindsSelector(t *testing.T) {
	g := graph.New()
	swiftMethodNode(g, "ios/App.swift::Mover.move", "moveFrom:to:")
	objcMethodNode(g, "ios/Legacy.m::moveFrom:to:", "moveFrom:to:")

	n := ResolveSwiftObjCBridge(g)
	assert.Equal(t, 1, n)

	fwd := bridgeEdgeBetween(g, "ios/App.swift::Mover.move", "ios/Legacy.m::moveFrom:to:")
	require.NotNil(t, fwd, "swift→objc bridge edge")
	assert.Equal(t, "moveFrom:to:", fwd.Meta["objc_selector"])
	assert.Equal(t, SynthSwiftObjC, fwd.Meta[MetaSynthesizedBy])
	assert.Equal(t, graph.OriginASTInferred, fwd.Origin)

	rev := bridgeEdgeBetween(g, "ios/Legacy.m::moveFrom:to:", "ios/App.swift::Mover.move")
	require.NotNil(t, rev, "objc→swift bridge edge (bidirectional)")
}

func TestResolveSwiftObjCBridge_ExplicitSelector(t *testing.T) {
	g := graph.New()
	swiftMethodNode(g, "ios/App.swift::Mover.moveCustom", "customMove:")
	objcMethodNode(g, "ios/Legacy.m::customMove:", "customMove:")
	objcMethodNode(g, "ios/Legacy.m::unrelated:", "unrelated:")

	assert.Equal(t, 1, ResolveSwiftObjCBridge(g))
	assert.NotNil(t, bridgeEdgeBetween(g, "ios/App.swift::Mover.moveCustom", "ios/Legacy.m::customMove:"))
	assert.Nil(t, bridgeEdgeBetween(g, "ios/App.swift::Mover.moveCustom", "ios/Legacy.m::unrelated:"))
}

func TestResolveSwiftObjCBridge_NoMatchNoEdge(t *testing.T) {
	g := graph.New()
	swiftMethodNode(g, "ios/App.swift::Mover.move", "moveFrom:to:")
	objcMethodNode(g, "ios/Legacy.m::other:", "other:")
	assert.Equal(t, 0, ResolveSwiftObjCBridge(g))
}

func TestResolveSwiftObjCBridge_Idempotent(t *testing.T) {
	g := graph.New()
	swiftMethodNode(g, "ios/App.swift::Mover.move", "moveFrom:to:")
	objcMethodNode(g, "ios/Legacy.m::moveFrom:to:", "moveFrom:to:")
	first := ResolveSwiftObjCBridge(g)
	second := ResolveSwiftObjCBridge(g)
	assert.Equal(t, first, second)
	// Exactly two bridge edges (one each direction) survive dedup.
	count := 0
	for e := range g.EdgesByKind(graph.EdgeReferences) {
		if e != nil && e.Meta != nil {
			if v, _ := e.Meta["via"].(string); v == swiftObjCBridgeVia {
				count++
			}
		}
	}
	assert.Equal(t, 2, count)
}
