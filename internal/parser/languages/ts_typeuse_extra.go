package languages

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// emitTSConstraintRefs emits an EdgeReferences for every generic-parameter
// constraint a declaration names — `<T extends ExcalidrawElement>` references
// ExcalidrawElement. The grammar shape is
//
//	(type_parameters (type_parameter (type_identifier) (constraint <type>)))
//
// Owned by the declaring symbol (class / interface / function / type alias), so
// find_usages(ExcalidrawElement) surfaces the bound site without a language
// server. The constraint type is decomposed through tsTypeRefs — the same gate
// every other type-use form uses — so primitives, builtins, and unions are
// handled uniformly.
func emitTSConstraintRefs(declNode *sitter.Node, ownerID, filePath string, src []byte, result *parser.ExtractionResult) {
	if declNode == nil || ownerID == "" {
		return
	}
	tparams := findChildByType(declNode, "type_parameters")
	if tparams == nil {
		return
	}
	for i, _nc := 0, int(tparams.NamedChildCount()); i < _nc; i++ {
		tp := tparams.NamedChild(i)
		if tp == nil || tp.Type() != "type_parameter" {
			continue
		}
		constraint := findChildByType(tp, "constraint")
		if constraint == nil {
			continue
		}
		line := int(constraint.StartPoint().Row) + 1
		// constraint text is `extends <type>` — strip the keyword before
		// decomposing.
		txt := strings.TrimSpace(constraint.Content(src))
		txt = strings.TrimPrefix(txt, "extends ")
		for _, name := range tsTypeRefs(txt) {
			result.Edges = append(result.Edges, &graph.Edge{
				From:     ownerID,
				To:       "unresolved::" + name,
				Kind:     graph.EdgeReferences,
				FilePath: filePath,
				Line:     line,
				Origin:   graph.OriginASTResolved,
				Meta:     map[string]any{"ref_context": "constraint"},
			})
		}
	}
}

// emitTSTypeNodeRefs walks a type-bearing AST subtree and emits one EdgeTypedAs
// per distinct named type it contains — every type_identifier and every
// `typeof X` query target, deduped, primitives / container-utility generics
// skipped. This catches the named types a string decomposer misses when the
// type is structurally complex: an object-type literal
// (`{ elements: ExcalidrawElement[]; appState: AppState }`), a function type
// (`(el: ExcalidrawElement) => void`), a mapped / conditional type, or a deep
// indexed-access type — all of which collapse to one non-identifier token under
// the text-based decomposer. Used for type-alias bodies so a type named only
// inside a structural type is still reachable by find_usages without an LSP.
func emitTSTypeNodeRefs(typeNode *sitter.Node, ownerID, filePath, useKind string, src []byte, result *parser.ExtractionResult) {
	if typeNode == nil || ownerID == "" {
		return
	}
	seen := map[string]bool{}
	line := int(typeNode.StartPoint().Row) + 1
	emit := func(name string) {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] || isTSPrimitive(name) || tsBuiltinGenerics[name] || !isTSTypeName(name) {
			return
		}
		seen[name] = true
		result.Edges = append(result.Edges, &graph.Edge{
			From:     ownerID,
			To:       "unresolved::" + name,
			Kind:     graph.EdgeTypedAs,
			FilePath: filePath,
			Line:     line,
			Origin:   graph.OriginASTInferred,
			Meta:     map[string]any{"use_kind": useKind},
		})
	}
	walkNodes(typeNode, func(n *sitter.Node) {
		switch n.Type() {
		case "type_identifier":
			emit(n.Content(src))
		case "type_query":
			// `typeof X` — the queried value is a plain identifier /
			// member-expression child, not a type_identifier, so the
			// type_identifier walk above skips it. Reference its (last) name
			// so `InstanceType<typeof App>` reaches App.
			for i, _nc := 0, int(n.NamedChildCount()); i < _nc; i++ {
				c := n.NamedChild(i)
				if c == nil {
					continue
				}
				switch c.Type() {
				case "identifier":
					emit(c.Content(src))
				case "member_expression", "nested_identifier":
					emit(lastDottedSegment(c.Content(src)))
				}
			}
		}
	})
}

// lastDottedSegment returns the final segment of a dotted name
// (`React.Component` → "Component"), or the input unchanged when undotted.
func lastDottedSegment(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '.'); i >= 0 {
		return strings.TrimSpace(s[i+1:])
	}
	return s
}
