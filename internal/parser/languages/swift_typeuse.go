package languages

import (
	"strconv"
	"strings"
	"unicode"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// emitSwiftReferenceForms is a single structural pass that surfaces the Swift
// reference forms a plain type-annotation extractor misses, so find_usages
// lands them without a language server:
//
//   - INSTANTIATION — `Foo()` / `Foo.init(...)`. Swift has no `new`; a
//     construction is a call_expression whose callee is a Capitalized
//     simple_identifier, or `Foo.init`. Emitted as graph.EdgeInstantiates
//     (not policed by the cross-package guard).
//   - INHERITANCE / CONFORMANCE — the `inheritance_specifier` entries of a
//     class / struct / extension declaration (`class X: Base, Proto`). Swift
//     spells both superclass and protocol conformance with `:`. Emitted as
//     EdgeReferences with ref_context "inherit".
//   - CASTS / TYPE-TESTS — `x as Foo`, `x as? Foo`, `x as! Foo` (as_expression)
//     and `x is Foo` (check_expression). Emitted as EdgeReferences with
//     ref_context "cast".
//   - STATIC / MEMBER ACCESS — `Foo.shared`, `Foo.Constant`: a
//     navigation_expression whose head is a bare Capitalized simple_identifier
//     (not self / a lowercase value binding). Emitted as EdgeReferences with
//     ref_context "static_access".
//
// Every structural EdgeReferences edge is stamped Origin
// graph.OriginASTResolved: cross_pkg_guard reverts weak-tier (text_matched /
// ast_inferred) EdgeReferences whose target is a bare `unresolved::X`, which
// these all are. EdgeInstantiates is not call-like, so it is left at the same
// resolved origin for consistency.
//
// The pass is Capitalization- and scope-aware: only Capitalized leaf names
// participate, Swift primitives are dropped via isSwiftPrimitive, and static
// access only fires when the navigation head is a bare Capitalized identifier
// (self / lowercase value receivers are excluded). Each (owner, type, line,
// ref_context) tuple is emitted at most once. Owners are attributed to the
// enclosing function (buildFuncRanges / findEnclosingFunc), else the enclosing
// Swift type, else the file node.
//
// Type-annotation edges (property / parameter / return / stored-property
// types) are already emitted as EdgeTypedAs by swift_function_shape.go — this
// pass deliberately does NOT re-emit those.
func emitSwiftReferenceForms(root *sitter.Node, src []byte, filePath, fileID string, funcRanges []funcRange, typeRanges []swiftTypeRange, result *parser.ExtractionResult) {
	if root == nil || result == nil {
		return
	}
	seen := map[string]bool{}

	owner := func(line int) string {
		if id := findEnclosingFunc(funcRanges, line); id != "" {
			return id
		}
		if name, ok := findEnclosingSwiftType(typeRanges, line); ok {
			return filePath + "::" + name
		}
		return fileID
	}

	emit := func(ownerID, typeName string, line int, kind graph.EdgeKind, useKind string) {
		if ownerID == "" {
			return
		}
		leaf := swiftBaseTypeName(typeName)
		if leaf == "" || isSwiftPrimitive(leaf) || !isSwiftCapitalized(leaf) {
			return
		}
		key := ownerID + "\x00" + leaf + "\x00" + useKind + "\x00" + strconv.Itoa(line)
		if seen[key] {
			return
		}
		seen[key] = true
		edge := &graph.Edge{
			From:     ownerID,
			To:       "unresolved::" + leaf,
			Kind:     kind,
			FilePath: filePath,
			Line:     line,
			Origin:   graph.OriginASTResolved,
		}
		if useKind != "" {
			edge.Meta = map[string]any{"ref_context": useKind}
		}
		result.Edges = append(result.Edges, edge)
	}

	walkNodes(root, func(n *sitter.Node) {
		switch n.Type() {
		case "call_expression":
			// INSTANTIATION: `Foo()` or `Foo.init(...)`.
			if name := swiftInstantiatedTypeName(n, src); name != "" {
				line := int(n.StartPoint().Row) + 1
				emit(owner(line), name, line, graph.EdgeInstantiates, "")
			}

		case "inheritance_specifier":
			// INHERITANCE / CONFORMANCE: a supertype or conformed protocol.
			// The owner is the type being declared — read directly from the
			// enclosing declaration's own name node so sequential / sibling
			// type declarations never mis-attribute by line range.
			name := swiftInheritanceTypeName(n, src)
			if name == "" {
				return
			}
			line := int(n.StartPoint().Row) + 1
			ownerID := fileID
			if declName := swiftDeclaredTypeName(n.Parent(), src); declName != "" {
				ownerID = filePath + "::" + declName
			} else if typeName, ok := findEnclosingSwiftType(typeRanges, line); ok {
				ownerID = filePath + "::" + typeName
			}
			emit(ownerID, name, line, graph.EdgeReferences, graph.RefContextInherit)

		case "as_expression":
			// CAST: `x as Foo` / `x as? Foo` / `x as! Foo`.
			if name := swiftTypeChildName(n, src); name != "" {
				line := int(n.StartPoint().Row) + 1
				emit(owner(line), name, line, graph.EdgeReferences, graph.RefContextCast)
			}

		case "check_expression":
			// TYPE-TEST: `x is Foo`.
			if name := swiftTypeChildName(n, src); name != "" {
				line := int(n.StartPoint().Row) + 1
				emit(owner(line), name, line, graph.EdgeReferences, graph.RefContextCast)
			}

		case "navigation_expression":
			// STATIC / MEMBER ACCESS: `Foo.shared`, `Foo.Constant`. Only when
			// the head is a bare Capitalized identifier — exclude self, a
			// lowercase value receiver, and the `Foo.init()` construction form
			// (which is handled by call_expression above; its navigation_
			// expression parent is a call_expression).
			if isSwiftInitNavigation(n, src) {
				return
			}
			if name := swiftStaticAccessHead(n, src); name != "" {
				line := int(n.StartPoint().Row) + 1
				emit(owner(line), name, line, graph.EdgeReferences, graph.RefContextStaticAccess)
			}
		}
	})
}

// swiftInstantiatedTypeName returns the constructed type name for a
// call_expression that is a construction (`Foo()` or `Foo.init(...)`), or "".
// A direct simple_identifier callee that is Capitalized is a construction; a
// navigation callee whose head is a Capitalized identifier and whose suffix is
// `init` is the explicit `Foo.init(...)` form.
func swiftInstantiatedTypeName(call *sitter.Node, src []byte) string {
	callee := call.NamedChild(0)
	if callee == nil {
		return ""
	}
	switch callee.Type() {
	case "simple_identifier":
		name := strings.TrimSpace(callee.Content(src))
		if isSwiftCapitalized(name) {
			return name
		}
	case "navigation_expression":
		head := callee.NamedChild(0)
		suffix := swiftNavigationSuffixName(callee, src)
		if head != nil && head.Type() == "simple_identifier" && suffix == "init" {
			name := strings.TrimSpace(head.Content(src))
			if isSwiftCapitalized(name) {
				return name
			}
		}
	}
	return ""
}

// swiftInheritanceTypeName returns the named type of an inheritance_specifier
// (its user_type / type_identifier child), or "".
func swiftInheritanceTypeName(n *sitter.Node, src []byte) string {
	return swiftTypeChildName(n, src)
}

// swiftDeclaredTypeName returns the name of the type a class / struct / enum /
// extension declaration introduces, read from the declaration node's leading
// name child (`class X` → "X", `extension Foo` → "Foo"). The grammar lowers
// all four to a class_declaration whose first named child is the declared name
// (a type_identifier for class/struct/enum, a user_type for an extension);
// inheritance_specifier / class_body children follow it. Returns "" for a node
// that is not such a declaration, so the inheritance owner falls back to the
// line-range lookup.
func swiftDeclaredTypeName(decl *sitter.Node, src []byte) string {
	if decl == nil || decl.Type() != "class_declaration" {
		return ""
	}
	for i := 0; i < int(decl.NamedChildCount()); i++ {
		c := decl.NamedChild(i)
		if c == nil {
			continue
		}
		switch c.Type() {
		case "type_identifier":
			return strings.TrimSpace(c.Content(src))
		case "user_type":
			// An extension's name is a user_type; its bare type_identifier
			// leaf is the declared type.
			return swiftBaseTypeName(strings.TrimSpace(c.Content(src)))
		case "inheritance_specifier", "class_body", "enum_class_body":
			// Reached the inheritance clause / body before a name — not a
			// named declaration we can attribute.
			return ""
		}
	}
	return ""
}

// swiftTypeChildName returns the text of the first user_type / type_identifier
// descendant child of n (the RHS type of a cast, type test, or inheritance
// specifier), or "".
func swiftTypeChildName(n *sitter.Node, src []byte) string {
	for i := 0; i < int(n.NamedChildCount()); i++ {
		c := n.NamedChild(i)
		if c == nil {
			continue
		}
		switch c.Type() {
		case "user_type", "type_identifier":
			return strings.TrimSpace(c.Content(src))
		}
	}
	return ""
}

// swiftStaticAccessHead returns the head identifier of a navigation_expression
// when it is a bare Capitalized simple_identifier (`Foo.shared` → "Foo"), or
// "". A self_expression head, a lowercase head (a value receiver), or a nested
// navigation head (a chained access whose left is itself an expression) all
// yield "" — only a top-level type-name access is a static reference.
func swiftStaticAccessHead(n *sitter.Node, src []byte) string {
	head := n.NamedChild(0)
	if head == nil || head.Type() != "simple_identifier" {
		return ""
	}
	name := strings.TrimSpace(head.Content(src))
	if !isSwiftCapitalized(name) {
		return ""
	}
	return name
}

// isSwiftInitNavigation reports whether a navigation_expression is the callee
// of a `Foo.init(...)` call — i.e. its suffix is `init` and its parent is a
// call_expression. Such a node is a construction handled by the call_expression
// branch, so the static-access branch must skip it to avoid double-emitting.
func isSwiftInitNavigation(n *sitter.Node, src []byte) bool {
	if swiftNavigationSuffixName(n, src) != "init" {
		return false
	}
	parent := n.Parent()
	return parent != nil && parent.Type() == "call_expression"
}

// swiftNavigationSuffixName returns the trailing member name of a
// navigation_expression's navigation_suffix (`Foo.bar` → "bar"), or "".
func swiftNavigationSuffixName(n *sitter.Node, src []byte) string {
	for i := 0; i < int(n.NamedChildCount()); i++ {
		c := n.NamedChild(i)
		if c == nil || c.Type() != "navigation_suffix" {
			continue
		}
		for j := 0; j < int(c.NamedChildCount()); j++ {
			id := c.NamedChild(j)
			if id != nil && id.Type() == "simple_identifier" {
				return strings.TrimSpace(id.Content(src))
			}
		}
	}
	return ""
}

// isSwiftCapitalized reports whether s begins with an uppercase letter — the
// Swift convention that distinguishes a type name from a value binding. Used to
// gate every reference form so a lowercase identifier (a local, a function, a
// property) never manufactures a type-reference edge.
func isSwiftCapitalized(s string) bool {
	if s == "" {
		return false
	}
	return unicode.IsUpper([]rune(s)[0])
}
