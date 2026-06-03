package languages

import (
	"regexp"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// React Native Fabric / Codegen view-component recognition. A Fabric
// component is declared in a TS spec with
// `codegenNativeComponent<NativeProps>('RCTMyView')`, and its NativeProps
// interface declares event props typed `DirectEventHandler<T>` /
// `BubblingEventHandler<T>`. This spec is the cross-language ground truth
// the native view manager (an ObjC RCTViewManager with
// RCT_EXPORT_VIEW_PROPERTY, or a Java ViewManager with @ReactProp)
// implements. The Fabric synthesizer links the spec to the native manager
// by component name.

var (
	// codegenNativeComponent<Props>('Name') — the type arg is optional.
	fabricCodegenRe = regexp.MustCompile(`codegenNativeComponent\s*(?:<[^>]*>)?\s*\(\s*['"]([^'"]+)['"]`)
	// onChange?: DirectEventHandler<ChangeEvent> / BubblingEventHandler<...>
	fabricEventRe = regexp.MustCompile(`(?m)^\s*(\w+)\??\s*:\s*(?:Direct|Bubbling)EventHandler\s*<`)
)

// emitFabricComponentNodes scans a TS/JS Fabric spec for a
// codegenNativeComponent declaration and emits a synthetic component node
// carrying fabric_component plus the event-handler prop names parsed from
// the spec, so the Fabric synthesizer can pair it with the native view
// manager.
func emitFabricComponentNodes(src []byte, filePath, language, fileID string, result *parser.ExtractionResult) {
	s := string(src)
	comp := fabricCodegenRe.FindStringSubmatch(s)
	if comp == nil {
		return
	}
	component := comp[1]
	id := filePath + "::fabric:" + component

	var events []string
	for _, m := range fabricEventRe.FindAllStringSubmatch(s, -1) {
		events = append(events, m[1])
	}
	line := lineAt(src, fabricCodegenRe.FindStringIndex(s)[0])
	node := &graph.Node{
		ID: id, Kind: graph.KindType, Name: component,
		FilePath: filePath, StartLine: line, EndLine: line,
		Language: language,
		Meta:     map[string]any{"fabric_component": component},
	}
	if len(events) > 0 {
		node.Meta["fabric_events"] = events
	}
	result.Nodes = append(result.Nodes, node)
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: line,
	})
}
