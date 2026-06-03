package resolver

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// fabricBridgeVia marks a synthesized Fabric spec↔native-view-manager edge.
const fabricBridgeVia = "fabric.component"

// ResolveFabricComponents is the framework-dispatch synthesizer for React
// Native Fabric / Codegen view components. The TS extractor emits a node
// per `codegenNativeComponent<Props>('Name')` spec (Meta
// fabric_component, plus fabric_events from DirectEventHandler props); the
// Objective-C and Java extractors emit a node per native view manager
// (Meta fabric_component derived from the manager class name, plus
// fabric_props). This pass binds the spec to its native implementation(s)
// by component name with bidirectional EdgeReferences bridge edges, so a
// Fabric component spec resolves to the native code that renders it.
//
// Full recompute and idempotent (graph.AddEdge dedupes; graph.EvictFile
// drops the bridge on reindex). Edges ride at ast_inferred with
// synthesizer provenance.
//
// Returns the number of Fabric specs bound to at least one native view
// manager.
func ResolveFabricComponents(g graph.Store) int {
	if g == nil {
		return 0
	}

	var specs []*graph.Node
	nativeByComponent := map[string][]*graph.Node{}
	for _, n := range nodesByKindsOrAll(g, graph.KindType) {
		if n == nil || n.Meta == nil {
			continue
		}
		comp, _ := n.Meta["fabric_component"].(string)
		if comp == "" {
			continue
		}
		if _, isNative := n.Meta["fabric_native"]; isNative {
			nativeByComponent[fabricNormalize(comp)] = append(nativeByComponent[fabricNormalize(comp)], n)
		} else {
			specs = append(specs, n)
		}
	}
	if len(specs) == 0 || len(nativeByComponent) == 0 {
		return 0
	}

	var batch []*graph.Edge
	bound := 0
	for _, spec := range specs {
		comp, _ := spec.Meta["fabric_component"].(string)
		matches := nativeByComponent[fabricNormalize(comp)]
		if len(matches) == 0 {
			continue
		}
		linked := false
		for _, native := range matches {
			if native.ID == spec.ID {
				continue
			}
			batch = append(batch,
				fabricBridgeEdge(spec, native, comp),
				fabricBridgeEdge(native, spec, comp),
			)
			linked = true
		}
		if linked {
			bound++
		}
	}

	for _, e := range batch {
		g.AddEdge(e)
	}
	return bound
}

// fabricNormalize folds a component name for cross-language matching: the
// native side often carries platform prefixes/suffixes the JS spec drops
// (RCTWebView ↔ WebView). Lower-cases and strips a leading "rct"/"rn".
func fabricNormalize(name string) string {
	n := strings.ToLower(name)
	n = strings.TrimPrefix(n, "rct")
	n = strings.TrimPrefix(n, "rn")
	return n
}

func fabricBridgeEdge(from, to *graph.Node, component string) *graph.Edge {
	return &graph.Edge{
		From:            from.ID,
		To:              to.ID,
		Kind:            graph.EdgeReferences,
		FilePath:        from.FilePath,
		Line:            from.StartLine,
		Confidence:      0.6,
		ConfidenceLabel: graph.ConfidenceLabelFor(graph.EdgeReferences, 0.6),
		Origin:          graph.OriginASTInferred,
		Meta: map[string]any{
			"via":              fabricBridgeVia,
			"fabric_component": component,
			"to_language":      to.Language,
			MetaSynthesizedBy:  SynthFabric,
			MetaProvenance:     ProvenanceHeuristic,
		},
	}
}
