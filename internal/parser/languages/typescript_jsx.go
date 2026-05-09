package languages

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// emitJSXRenderEdges walks a function/method/arrow body for JSX child
// components rendered inside it and emits one EdgeRendersChild per
// unique child component. The parent ID is the enclosing function's
// graph ID (passed in by the caller).
//
// Detection:
//   - jsx_element  → `<Foo>...</Foo>` — read opening element's name.
//   - jsx_self_closing_element → `<Foo />` — read its name.
//
// Naming convention: capital-first-letter element names are component
// references; lowercase names map to HTML/SVG primitives and are
// skipped (rendering edges to <div> and <span> would be pure noise).
// Member-expression names (`Foo.Bar`) preserve the qualified shape so
// the resolver can land them on `unresolved::Foo.Bar`.
//
// Dedup: the same component rendered in multiple branches of the same
// function emits one edge — the dependency is "this parent renders
// this child", and counting branches is a different question.
func emitJSXRenderEdges(parentID string, body *sitter.Node, src []byte, filePath string, result *parser.ExtractionResult) {
	if body == nil || parentID == "" {
		return
	}
	seen := make(map[string]int) // child name → first line
	walkAST(body, func(n *sitter.Node) bool {
		if n == nil {
			return true
		}
		switch n.Type() {
		case "jsx_element":
			opening := n.NamedChild(0)
			if opening != nil && opening.Type() == "jsx_opening_element" {
				if name := jsxElementName(opening, src); name != "" {
					if _, dup := seen[name]; !dup {
						seen[name] = int(n.StartPoint().Row) + 1
					}
				}
			}
		case "jsx_self_closing_element":
			if name := jsxElementName(n, src); name != "" {
				if _, dup := seen[name]; !dup {
					seen[name] = int(n.StartPoint().Row) + 1
				}
			}
		}
		return true
	})
	for name, line := range seen {
		if !isJSXComponentName(name) {
			continue
		}
		result.Edges = append(result.Edges, &graph.Edge{
			From:     parentID,
			To:       "unresolved::" + name,
			Kind:     graph.EdgeRendersChild,
			FilePath: filePath,
			Line:     line,
			Origin:   graph.OriginASTInferred,
			Meta: map[string]any{
				"child_name": name,
			},
		})
	}
}

// jsxElementName returns the bare or qualified element name from a
// jsx_opening_element / jsx_self_closing_element node. Returns "" for
// shapes the resolver can't act on (jsx_namespace_name, fragments
// `<>...</>`, member expressions deeper than two segments).
func jsxElementName(node *sitter.Node, src []byte) string {
	if node == nil {
		return ""
	}
	nameNode := node.ChildByFieldName("name")
	if nameNode == nil {
		// Fallback: walk children for the name token. tree-sitter
		// grammars vary on whether the field is exposed.
		for i := 0; i < int(node.NamedChildCount()); i++ {
			c := node.NamedChild(i)
			if c != nil && (c.Type() == "identifier" || c.Type() == "member_expression" || c.Type() == "nested_identifier") {
				nameNode = c
				break
			}
		}
	}
	if nameNode == nil {
		return ""
	}
	switch nameNode.Type() {
	case "identifier":
		return nameNode.Content(src)
	case "member_expression", "nested_identifier":
		// Two-segment qualified name: `Foo.Bar`.
		return strings.TrimSpace(nameNode.Content(src))
	}
	return ""
}

// isJSXComponentName reports whether name is a component reference (as
// opposed to an HTML/SVG primitive). React's convention: capital first
// letter or contains a `.` (member-access components).
func isJSXComponentName(name string) bool {
	if name == "" {
		return false
	}
	if strings.Contains(name, ".") {
		return true
	}
	first := name[0]
	return first >= 'A' && first <= 'Z'
}
