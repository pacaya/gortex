package resolver

import "github.com/zzet/gortex/internal/graph"

const (
	// rnNativeVia marks the JS→native placeholder call edge the JS/TS
	// extractor emits (mirrors languages.rnNativeVia, kept as a literal
	// so the resolver doesn't import the parser package).
	rnNativeVia = "rn.native"
	// rnBridgeVia marks a synthesized JS→native-implementation edge.
	rnBridgeVia = "rn.bridge"
)

// ResolveReactNativeBridge is the framework-dispatch synthesizer for the
// React Native bridge. The JS/TS extractor emits a placeholder call edge
// (Meta["via"]="rn.native", carrying rn_module + rn_method) for every
// `NativeModules.<module>.<method>()` / `requireNativeModule(...)` /
// `TurboModuleRegistry.get(...)` call. The Objective-C and Java
// extractors stamp rn_module + rn_method on the native method nodes that
// implement those modules (RCT_EXPORT_METHOD / RCT_REMAP_METHOD on iOS,
// @ReactMethod on Android). This pass joins them: for each JS call it
// synthesizes a calls edge to every native implementation of that
// (module, method) — typically two, the iOS and Android sides — so a JS
// call resolves into the native code that runs it and a native method
// (otherwise unreferenced) shows its JS callers.
//
// Full recompute and idempotent: edges are re-derived from the
// placeholder markers and the native metadata, graph.AddEdge dedupes by
// key, and graph.EvictFile drops a synthesized edge in both directions
// when either side's file is reindexed. Edges ride at ast_inferred and
// carry synthesizer provenance.
//
// Returns the number of native bridge edges synthesized.
func ResolveReactNativeBridge(g graph.Store) int {
	if g == nil {
		return 0
	}

	type modKey struct{ module, method string }
	nativeByKey := map[modKey][]*graph.Node{}
	for _, n := range nodesByKindsOrAll(g, graph.KindMethod) {
		if n == nil || n.Meta == nil {
			continue
		}
		mod, _ := n.Meta["rn_module"].(string)
		meth, _ := n.Meta["rn_method"].(string)
		if mod == "" || meth == "" {
			continue
		}
		k := modKey{mod, meth}
		nativeByKey[k] = append(nativeByKey[k], n)
	}
	if len(nativeByKey) == 0 {
		return 0
	}

	// Dedupe (callerID → nativeID) so a caller invoking the same native
	// method from several lines yields one edge per implementation.
	type pairKey struct{ from, to string }
	seenPair := map[pairKey]bool{}
	var batch []*graph.Edge
	synthesized := 0
	for e := range g.EdgesByKind(graph.EdgeCalls) {
		if e == nil || e.Meta == nil || e.From == "" {
			continue
		}
		if v, _ := e.Meta["via"].(string); v != rnNativeVia {
			continue
		}
		mod, _ := e.Meta["rn_module"].(string)
		meth, _ := e.Meta["rn_method"].(string)
		if mod == "" || meth == "" {
			continue
		}
		for _, native := range nativeByKey[modKey{mod, meth}] {
			if native.ID == "" || native.ID == e.From {
				continue
			}
			pk := pairKey{from: e.From, to: native.ID}
			if seenPair[pk] {
				continue
			}
			seenPair[pk] = true
			batch = append(batch, &graph.Edge{
				From:            e.From,
				To:              native.ID,
				Kind:            graph.EdgeCalls,
				FilePath:        e.FilePath,
				Line:            e.Line,
				Confidence:      0.6,
				ConfidenceLabel: graph.ConfidenceLabelFor(graph.EdgeCalls, 0.6),
				Origin:          graph.OriginASTInferred,
				Meta: map[string]any{
					"via":             rnBridgeVia,
					"rn_module":       mod,
					"rn_method":       meth,
					"native_language": native.Language,
					MetaSynthesizedBy: SynthReactNative,
					MetaProvenance:    ProvenanceHeuristic,
				},
			})
			synthesized++
		}
	}

	for _, e := range batch {
		g.AddEdge(e)
	}
	return synthesized
}
