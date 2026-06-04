package resolver

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// rnNativeMethod adds a native (objc/java) method node carrying RN bridge
// metadata.
func rnNativeMethod(g graph.Store, id, lang, module, method string) {
	g.AddNode(&graph.Node{
		ID: id, Kind: graph.KindMethod, Name: lastSeg(id),
		FilePath: id, StartLine: 1, Language: lang,
		Meta: map[string]any{"rn_module": module, "rn_method": method},
	})
}

// rnJSCall adds a JS caller plus its rn.native placeholder call edge.
func rnJSCall(g graph.Store, callerID, module, method string) {
	g.AddNode(&graph.Node{ID: callerID, Kind: graph.KindFunction, Name: lastSeg(callerID), FilePath: "app.js", Language: "javascript"})
	g.AddEdge(&graph.Edge{
		From: callerID, To: "unresolved::rn::" + module + "::" + method,
		Kind: graph.EdgeCalls, FilePath: "app.js", Line: 3,
		Meta: map[string]any{"via": rnNativeVia, "rn_module": module, "rn_method": method},
	})
}

func rnBridgeEdge(g graph.Store, from, to string) *graph.Edge {
	for _, e := range g.GetOutEdges(from) {
		if e.To == to && e.Kind == graph.EdgeCalls && e.Meta != nil {
			if v, _ := e.Meta["via"].(string); v == rnBridgeVia {
				return e
			}
		}
	}
	return nil
}

func TestResolveReactNativeBridge_BothPlatforms(t *testing.T) {
	g := graph.New()
	rnJSCall(g, "app.js::go", "Calendar", "createEvent")
	rnNativeMethod(g, "ios/Calendar.m::createEvent:location:", "objc", "Calendar", "createEvent")
	rnNativeMethod(g, "android/Calendar.java::CalendarModule.createEvent", "java", "Calendar", "createEvent")

	n := ResolveReactNativeBridge(g)
	assert.Equal(t, 2, n, "a JS call bridges to both the iOS and Android native impls")

	ios := rnBridgeEdge(g, "app.js::go", "ios/Calendar.m::createEvent:location:")
	require.NotNil(t, ios)
	assert.Equal(t, "objc", ios.Meta["native_language"])
	assert.Equal(t, SynthReactNative, ios.Meta[MetaSynthesizedBy])
	assert.Equal(t, graph.OriginASTInferred, ios.Origin)

	android := rnBridgeEdge(g, "app.js::go", "android/Calendar.java::CalendarModule.createEvent")
	require.NotNil(t, android)
	assert.Equal(t, "java", android.Meta["native_language"])
}

func TestResolveReactNativeBridge_NoNativeNoEdge(t *testing.T) {
	g := graph.New()
	rnJSCall(g, "app.js::go", "Calendar", "createEvent")
	rnNativeMethod(g, "ios/Other.m::doThing", "objc", "Other", "doThing")
	assert.Equal(t, 0, ResolveReactNativeBridge(g))
}

func TestResolveReactNativeBridge_Idempotent(t *testing.T) {
	g := graph.New()
	rnJSCall(g, "app.js::go", "Calendar", "createEvent")
	rnNativeMethod(g, "ios/Calendar.m::createEvent:", "objc", "Calendar", "createEvent")
	first := ResolveReactNativeBridge(g)
	second := ResolveReactNativeBridge(g)
	assert.Equal(t, first, second)
	count := 0
	for e := range g.EdgesByKind(graph.EdgeCalls) {
		if e != nil && e.Meta != nil {
			if v, _ := e.Meta["via"].(string); v == rnBridgeVia {
				count++
			}
		}
	}
	assert.Equal(t, 1, count)
}
