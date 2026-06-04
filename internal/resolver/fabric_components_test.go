package resolver

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

func fabricSpecNode(g graph.Store, id, component string) {
	g.AddNode(&graph.Node{
		ID: id, Kind: graph.KindType, Name: component, FilePath: id, StartLine: 1,
		Language: "typescript", Meta: map[string]any{"fabric_component": component},
	})
}

func fabricNativeNode(g graph.Store, id, lang, component string) {
	g.AddNode(&graph.Node{
		ID: id, Kind: graph.KindType, Name: component, FilePath: id, StartLine: 1,
		Language: lang, Meta: map[string]any{"fabric_component": component, "fabric_native": lang},
	})
}

func fabricBridge(g graph.Store, from, to string) *graph.Edge {
	for _, e := range g.GetOutEdges(from) {
		if e.To == to && e.Kind == graph.EdgeReferences && e.Meta != nil {
			if v, _ := e.Meta["via"].(string); v == fabricBridgeVia {
				return e
			}
		}
	}
	return nil
}

func TestResolveFabricComponents_BindsSpecToNative(t *testing.T) {
	g := graph.New()
	// TS spec uses RCTColorView; native managers normalize to the same.
	fabricSpecNode(g, "ColorViewNativeComponent.ts::fabric:RCTColorView", "RCTColorView")
	fabricNativeNode(g, "RCTColorViewManager.m::fabric:RCTColorView", "objc", "RCTColorView")
	fabricNativeNode(g, "ColorViewManager.java::fabric:RCTColorView", "java", "RCTColorView")

	n := ResolveFabricComponents(g)
	assert.Equal(t, 1, n, "one spec bound (to two native managers)")

	objc := fabricBridge(g, "ColorViewNativeComponent.ts::fabric:RCTColorView", "RCTColorViewManager.m::fabric:RCTColorView")
	require.NotNil(t, objc)
	assert.Equal(t, SynthFabric, objc.Meta[MetaSynthesizedBy])
	require.NotNil(t, fabricBridge(g, "RCTColorViewManager.m::fabric:RCTColorView", "ColorViewNativeComponent.ts::fabric:RCTColorView"), "bidirectional")
	require.NotNil(t, fabricBridge(g, "ColorViewNativeComponent.ts::fabric:RCTColorView", "ColorViewManager.java::fabric:RCTColorView"))
}

func TestResolveFabricComponents_NormalizedMatch(t *testing.T) {
	g := graph.New()
	// Spec drops the RCT prefix; native keeps it — normalization bridges.
	fabricSpecNode(g, "spec.ts::fabric:ColorView", "ColorView")
	fabricNativeNode(g, "mgr.m::fabric:RCTColorView", "objc", "RCTColorView")
	assert.Equal(t, 1, ResolveFabricComponents(g))
	assert.NotNil(t, fabricBridge(g, "spec.ts::fabric:ColorView", "mgr.m::fabric:RCTColorView"))
}

func TestResolveFabricComponents_NoMatch(t *testing.T) {
	g := graph.New()
	fabricSpecNode(g, "spec.ts::fabric:ColorView", "ColorView")
	fabricNativeNode(g, "mgr.m::fabric:Slider", "objc", "Slider")
	assert.Equal(t, 0, ResolveFabricComponents(g))
}
