package languages

import (
	"strconv"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// emitJavaReferenceForms walks the parsed tree once and emits the
// expression-position type references that the declaration-position
// passes (param / return / field / local annotations in
// emitJavaFunctionShape + emitJavaTypeUseEdges) miss. Each of these is a
// genuine `find_usages(Type)` hit that, without an LSP, was previously
// edge-less:
//
//   - INSTANTIATION  `new Foo()`, `new Foo[3]`, `new Outer.Inner()`,
//     plus the generic arguments of `new ArrayList<Request>()` —
//     graph.EdgeInstantiates, ref_context=instantiate.
//   - INHERITANCE    `class X extends Foo implements Bar, Baz` — the base
//     extractor only stamps `scope_parent` meta (for the method-scope
//     resolver), so the type reference for find_usages was missing.
//     EdgeReferences, ref_context=inherit.
//   - CASTS / TYPE-TESTS  `(Foo) x`, `x instanceof Foo`, the pattern
//     `x instanceof Foo f` — EdgeReferences, ref_context=cast.
//   - STATIC / CONSTANT access  `Foo.CONST`, `Foo.staticMethod()`,
//     `Foo.class`, and `@Foo` annotation references whose scope is a
//     Capitalized type identifier — EdgeReferences, ref_context=static_access.
//
// The references are attributed to the enclosing method (falling back to
// the file node) via funcRanges. Every named type is canonicalized
// through canonicalizeJavaTypeRef (strip generics / arrays / varargs /
// package prefix, unwrap common containers) and primitives are dropped
// via isJavaPrimitive, exactly as the declaration-position passes do, so
// the graph isn't flooded with unresolved::int / unresolved::String.
//
// CRITICAL: the structural EdgeReferences forms (inherit / cast /
// static_access) carry a bare `unresolved::X` target. The
// cross-package call guard reverts weak-tier EdgeReferences edges with a
// bare-name target — so these are stamped OriginASTResolved (structural,
// compiler-grade evidence) to stay out of that guard's scope.
// EdgeInstantiates is not a call-like edge and is never policed, so the
// instantiation forms use OriginASTInferred like the other type-use
// edges.
func emitJavaReferenceForms(root *sitter.Node, src []byte, filePath, fileID string, funcRanges []funcRange, result *parser.ExtractionResult) {
	if root == nil {
		return
	}
	seen := make(map[string]bool)
	owner := func(line int) string {
		if id := findEnclosingFunc(funcRanges, line); id != "" {
			return id
		}
		return fileID
	}
	emit := func(rawType string, line int, useKind string, kind graph.EdgeKind, origin string) {
		t := canonicalizeJavaTypeRef(rawType)
		if t == "" || isJavaPrimitive(t) || !isJavaTypeName(t) {
			return
		}
		ownerID := owner(line)
		key := ownerID + "|" + t + "|" + strconv.Itoa(line) + "|" + useKind
		if seen[key] {
			return
		}
		seen[key] = true
		result.Edges = append(result.Edges, &graph.Edge{
			From:     ownerID,
			To:       "unresolved::" + t,
			Kind:     kind,
			FilePath: filePath,
			Line:     line,
			Origin:   origin,
			Meta:     map[string]any{"ref_context": useKind},
		})
	}

	walkNodes(root, func(n *sitter.Node) {
		if n == nil {
			return
		}
		line := int(n.StartPoint().Row) + 1
		switch n.Type() {
		case "object_creation_expression":
			// `new Foo(...)` / `new Outer.Inner(...)` / `new List<Req>()`.
			if ty := javaCreatedTypeText(n, src); ty != "" {
				emit(ty, line, "instantiate", graph.EdgeInstantiates, graph.OriginASTInferred)
			}
			// Generic arguments of the created type are themselves
			// instantiation-position references (`new ArrayList<Request>()`
			// uses Request).
			emitJavaTypeArgRefs(n, src, line, "instantiate", graph.EdgeInstantiates, graph.OriginASTInferred, emit)

		case "array_creation_expression":
			// `new Foo[3]` — the element type child.
			if ty := javaFirstTypeChildText(n, src); ty != "" {
				emit(ty, line, "instantiate", graph.EdgeInstantiates, graph.OriginASTInferred)
			}

		case "cast_expression":
			// `(Foo) x` — every type child (an intersection cast
			// `(A & B) x` has several) plus their generic args.
			emitJavaTypeChildRefs(n, src, line, "cast", graph.EdgeReferences, graph.OriginASTResolved, emit)

		case "instanceof_expression":
			// `x instanceof Foo` and the pattern `x instanceof Foo f`.
			emitJavaTypeChildRefs(n, src, line, "cast", graph.EdgeReferences, graph.OriginASTResolved, emit)

		case "class_literal":
			// `Foo.class` — the type child.
			if ty := javaFirstTypeChildText(n, src); ty != "" {
				emit(ty, line, "static_access", graph.EdgeReferences, graph.OriginASTResolved)
			}

		case "field_access":
			// `Foo.CONST` — static field read off a Capitalized type
			// scope. `this.x`, `obj.x`, and chained `a.b.c` are excluded
			// because their object is not a bare Capitalized identifier.
			if ty := javaStaticScopeIdent(n, src); ty != "" {
				emit(ty, line, "static_access", graph.EdgeReferences, graph.OriginASTResolved)
			}

		case "method_invocation":
			// `Foo.staticMethod()` — static call off a Capitalized type
			// scope. `obj.method()` (lowercase scope) and bare `foo()`
			// (no scope) are excluded. The call edge itself is still
			// emitted by the existing call pass; this adds the type
			// reference for find_usages(Foo).
			if ty := javaStaticScopeIdent(n, src); ty != "" {
				emit(ty, line, "static_access", graph.EdgeReferences, graph.OriginASTResolved)
			}

		case "marker_annotation", "annotation":
			// `@Foo` / `@Foo(...)` — reference the annotation type. This
			// is complementary to the EdgeAnnotated edge the declaration
			// passes emit (which targets a synthetic annotation:: node):
			// here we link to the user's type def so find_usages(Foo)
			// surfaces the annotation site too.
			if nameNode := n.ChildByFieldName("name"); nameNode != nil {
				emit(nameNode.Content(src), line, "static_access", graph.EdgeReferences, graph.OriginASTResolved)
			}

		case "superclass":
			// `extends Foo` — the named superclass.
			emitJavaTypeChildRefs(n, src, line, "inherit", graph.EdgeReferences, graph.OriginASTResolved, emit)

		case "super_interfaces":
			// `implements Bar, Baz` — every interface in the type_list.
			for i := 0; i < int(n.NamedChildCount()); i++ {
				c := n.NamedChild(i)
				if c == nil {
					continue
				}
				if c.Type() == "type_list" {
					emitJavaTypeChildRefs(c, src, line, "inherit", graph.EdgeReferences, graph.OriginASTResolved, emit)
				} else {
					emitJavaTypeChildRefs(n, src, line, "inherit", graph.EdgeReferences, graph.OriginASTResolved, emit)
				}
			}
		}
	})
}

// javaTypeNodeTypes are the tree-sitter node types that name a type in
// expression / clause position.
func isJavaTypeNode(t string) bool {
	switch t {
	case "type_identifier", "scoped_type_identifier", "generic_type":
		return true
	}
	return false
}

// emitJavaTypeChildRefs emits a reference for every direct named type
// child of node (and their generic arguments). Used for cast /
// instanceof / extends / implements clauses, where the type(s) sit as
// direct children.
func emitJavaTypeChildRefs(node *sitter.Node, src []byte, line int, useKind string, kind graph.EdgeKind, origin string, emit func(string, int, string, graph.EdgeKind, string)) {
	for i := 0; i < int(node.NamedChildCount()); i++ {
		c := node.NamedChild(i)
		if c == nil || !isJavaTypeNode(c.Type()) {
			continue
		}
		emit(strings.TrimSpace(c.Content(src)), line, useKind, kind, origin)
		emitJavaTypeArgRefs(c, src, line, useKind, kind, origin, emit)
	}
}

// emitJavaTypeArgRefs walks the `type_arguments` subtree of node and
// emits a reference for each named type argument (`<Request>` →
// Request). Recurses through nested generics (`Map<K, List<V>>`).
func emitJavaTypeArgRefs(node *sitter.Node, src []byte, line int, useKind string, kind graph.EdgeKind, origin string, emit func(string, int, string, graph.EdgeKind, string)) {
	walkNodes(node, func(n *sitter.Node) {
		if n == nil || n.Type() != "type_arguments" {
			return
		}
		for i := 0; i < int(n.NamedChildCount()); i++ {
			c := n.NamedChild(i)
			if c == nil || !isJavaTypeNode(c.Type()) {
				continue
			}
			emit(strings.TrimSpace(c.Content(src)), line, useKind, kind, origin)
		}
	})
}

// javaCreatedTypeText returns the raw type text of an
// object_creation_expression's instantiated type — the first named
// type child (`new Foo()` → "Foo", `new Outer.Inner()` → "Outer.Inner",
// `new List<Req>()` → "List<Req>").
func javaCreatedTypeText(node *sitter.Node, src []byte) string {
	return javaFirstTypeChildText(node, src)
}

// javaFirstTypeChildText returns the raw text of the first named child
// of node that names a type (type_identifier / scoped_type_identifier /
// generic_type), or "".
func javaFirstTypeChildText(node *sitter.Node, src []byte) string {
	for i := 0; i < int(node.NamedChildCount()); i++ {
		c := node.NamedChild(i)
		if c == nil || !isJavaTypeNode(c.Type()) {
			continue
		}
		return strings.TrimSpace(c.Content(src))
	}
	return ""
}

// javaStaticScopeIdent returns the scope identifier of a field_access /
// method_invocation when that scope is a bare Capitalized identifier —
// i.e. a static member access off a type (`Foo.CONST`, `Foo.method()`).
// Returns "" when the scope is `this`, a lowercase variable, a chained
// instance expression, or absent (a bare call), so instance accesses
// never produce a type reference.
func javaStaticScopeIdent(node *sitter.Node, src []byte) string {
	obj := node.ChildByFieldName("object")
	if obj == nil || obj.Type() != "identifier" {
		return ""
	}
	name := strings.TrimSpace(obj.Content(src))
	if !isJavaTypeName(name) {
		return ""
	}
	return name
}

// isJavaTypeName reports whether name reads like a Java type reference:
// a PascalCase identifier whose first rune is an uppercase ASCII letter.
// This is the capitalization gate that keeps the expression-position
// reference pass from binding lowercase method names, local variables,
// and package fragments as if they were types. It is intentionally
// conservative — a few SCREAMING_CASE constants used bare as a scope
// (rare in Java) are accepted, but those would have been dropped earlier
// by isJavaPrimitive / canonicalizeJavaTypeRef when they aren't a real
// type, and a stray false positive resolves to nothing harmlessly.
func isJavaTypeName(name string) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return false
	}
	// Strip any residual package prefix the caller didn't canonicalize.
	if i := strings.LastIndex(name, "."); i >= 0 {
		name = name[i+1:]
	}
	if name == "" {
		return false
	}
	c := name[0]
	return c >= 'A' && c <= 'Z'
}
