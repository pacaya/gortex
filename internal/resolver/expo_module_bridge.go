package resolver

import "github.com/zzet/gortex/internal/graph"

// expoBridgeVia marks a synthesized JS→Expo-native call edge.
const expoBridgeVia = "expo.bridge"

// ResolveExpoModuleBridge is the framework-dispatch synthesizer for Expo
// Modules. The Swift/Kotlin extractors stamp expo_module + expo_method on
// synthetic method nodes parsed from the Expo DSL (Name / Function /
// AsyncFunction). The JS/TS extractor already emits an rn-native
// placeholder call edge (Meta["via"]="rn.native", rn_module + rn_method)
// for `requireNativeModule('Foo').bar()` — the canonical Expo JS consumer
// form. This pass joins them: for each such JS call it synthesizes a calls
// edge to every Expo native implementation of that (module, method).
//
// Full recompute and idempotent: graph.AddEdge dedupes by key,
// graph.EvictFile drops the edge in both directions on reindex. Edges
// ride at ast_inferred with synthesizer provenance.
//
// Returns the number of Expo bridge edges synthesized.
func ResolveExpoModuleBridge(g graph.Store) int {
	if g == nil {
		return 0
	}

	type modKey struct{ module, method string }
	expoByKey := map[modKey][]*graph.Node{}
	for _, n := range nodesByKindsOrAll(g, graph.KindMethod) {
		if n == nil || n.Meta == nil {
			continue
		}
		mod, _ := n.Meta["expo_module"].(string)
		meth, _ := n.Meta["expo_method"].(string)
		if mod == "" || meth == "" {
			continue
		}
		expoByKey[modKey{mod, meth}] = append(expoByKey[modKey{mod, meth}], n)
	}
	if len(expoByKey) == 0 {
		return 0
	}

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
		for _, native := range expoByKey[modKey{mod, meth}] {
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
					"via":             expoBridgeVia,
					"expo_module":     mod,
					"expo_method":     meth,
					"native_language": native.Language,
					MetaSynthesizedBy: SynthExpoModules,
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
