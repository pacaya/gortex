package resolver

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

func expoNativeNode(g graph.Store, id, lang, module, method string) {
	g.AddNode(&graph.Node{
		ID: id, Kind: graph.KindMethod, Name: lastSeg(id),
		FilePath: id, StartLine: 1, Language: lang,
		Meta: map[string]any{"expo_module": module, "expo_method": method},
	})
}

// expoJSCall adds a JS caller plus the rn.native placeholder edge a
// requireNativeModule('mod').method() call produces.
func expoJSCall(g graph.Store, callerID, module, method string) {
	g.AddNode(&graph.Node{ID: callerID, Kind: graph.KindFunction, Name: lastSeg(callerID), FilePath: "app.ts", Language: "typescript"})
	g.AddEdge(&graph.Edge{
		From: callerID, To: "unresolved::rn::" + module + "::" + method,
		Kind: graph.EdgeCalls, FilePath: "app.ts", Line: 4,
		Meta: map[string]any{"via": rnNativeVia, "rn_module": module, "rn_method": method},
	})
}

func expoBridgeEdge(g graph.Store, from, to string) *graph.Edge {
	for _, e := range g.GetOutEdges(from) {
		if e.To == to && e.Kind == graph.EdgeCalls && e.Meta != nil {
			if v, _ := e.Meta["via"].(string); v == expoBridgeVia {
				return e
			}
		}
	}
	return nil
}

func TestResolveExpoModuleBridge_Pairs(t *testing.T) {
	g := graph.New()
	expoJSCall(g, "app.ts::useMath", "Math", "add")
	expoNativeNode(g, "ios/MathModule.swift::expo:Math:add", "swift", "Math", "add")
	expoNativeNode(g, "android/MathModule.kt::expo:Math:add", "kotlin", "Math", "add")

	n := ResolveExpoModuleBridge(g)
	assert.Equal(t, 2, n, "JS call bridges to both the Swift and Kotlin Expo impls")

	sw := expoBridgeEdge(g, "app.ts::useMath", "ios/MathModule.swift::expo:Math:add")
	require.NotNil(t, sw)
	assert.Equal(t, SynthExpoModules, sw.Meta[MetaSynthesizedBy])
	assert.Equal(t, "swift", sw.Meta["native_language"])
	require.NotNil(t, expoBridgeEdge(g, "app.ts::useMath", "android/MathModule.kt::expo:Math:add"))
}

func TestResolveExpoModuleBridge_NoMatch(t *testing.T) {
	g := graph.New()
	expoJSCall(g, "app.ts::useMath", "Math", "add")
	expoNativeNode(g, "ios/Other.swift::expo:Other:sub", "swift", "Other", "sub")
	assert.Equal(t, 0, ResolveExpoModuleBridge(g))
}
