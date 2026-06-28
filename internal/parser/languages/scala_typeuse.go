package languages

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// emitScalaTypeUseEdges emits a single EdgeTypedAs from ownerID to the named
// type used in a type-annotation position (`val x: Foo`, `def f(p: Foo)`,
// a `def`'s return type, …). It canonicalizes the raw annotation text to its
// bare named type — stripping square-bracket generics, unwrapping the common
// container/effect wrappers (Option / Seq / List / Future / …) to their inner
// element, and dropping any dotted package prefix — then skips primitives so
// the graph isn't flooded with unresolved::Int / unresolved::String edges that
// never land. LSP-free: a Scala type used only in annotation position still
// produces a usage edge the resolver can later bind to a workspace type node.
func emitScalaTypeUseEdges(ownerID, typeText, filePath string, line int, result *parser.ExtractionResult) {
	if ownerID == "" {
		return
	}
	t := canonicalizeScalaTypeRef(typeText)
	if t == "" || isScalaPrimitive(t) {
		return
	}
	result.Edges = append(result.Edges, &graph.Edge{
		From:     ownerID,
		To:       "unresolved::" + t,
		Kind:     graph.EdgeTypedAs,
		FilePath: filePath,
		Line:     line,
		Origin:   graph.OriginASTInferred,
	})
}

// scalaContainerWrappers are the generic container / effect types whose single
// type argument is the "interesting" referenced type — `Option[User]` is, for
// type-usage purposes, a use of `User`. Recursing through them lets a type
// reachable only through such a wrapper still surface a usage edge.
var scalaContainerWrappers = []string{
	"Option", "Some", "Seq", "List", "Vector", "Set", "Array",
	"Future", "Try", "Iterable", "IndexedSeq",
}

// canonicalizeScalaTypeRef reduces a Scala type expression to its bare named
// type. Scala uses square brackets for generics (`Map[K, V]`, `Option[T]`), so
// the generic argument list is `[...]`, not `<...>`. The function:
//   - unwraps a known single-argument container/effect wrapper to its element
//     type (`Option[User]` -> `User`, `Future[Seq[Repo]]` -> `Repo`);
//   - otherwise strips the generic argument list (`Map[K, V]` -> `Map`);
//   - strips any dotted package prefix (`pkg.Foo` -> `Foo`).
//
// It mirrors scalaBaseType for the non-wrapper path but additionally unwraps
// containers, so `val xs: List[Widget]` references `Widget` rather than `List`.
func canonicalizeScalaTypeRef(t string) string {
	t = strings.TrimSpace(t)
	t = strings.TrimPrefix(t, ":")
	t = strings.TrimSpace(t)
	if t == "" {
		return ""
	}
	// Unwrap a known container wrapper around a single type argument.
	if open := strings.IndexByte(t, '['); open > 0 && strings.HasSuffix(t, "]") {
		head := strings.TrimSpace(t[:open])
		head = bareScalaName(head)
		inner := t[open+1 : len(t)-1]
		for _, w := range scalaContainerWrappers {
			if head == w {
				// Only unwrap when the single argument is itself a single
				// type (no top-level comma — a Map-like K,V is not unwrapped).
				if !hasTopLevelComma(inner) {
					return canonicalizeScalaTypeRef(inner)
				}
			}
		}
		// Not a wrapper (or multi-arg): the named type is the head.
		return head
	}
	return bareScalaName(t)
}

// bareScalaName strips a dotted package/object prefix, leaving the simple name.
func bareScalaName(t string) string {
	t = strings.TrimSpace(t)
	if i := strings.IndexByte(t, '['); i >= 0 {
		t = t[:i]
	}
	if i := strings.LastIndexByte(t, '.'); i >= 0 {
		t = t[i+1:]
	}
	return strings.TrimSpace(t)
}

// hasTopLevelComma reports whether s contains a comma outside any nested
// `[...]` bracket group — i.e. the generic argument list has more than one
// argument (`K, V`) rather than a single nested type (`Seq[Repo]`).
func hasTopLevelComma(s string) bool {
	depth := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '[':
			depth++
		case ']':
			if depth > 0 {
				depth--
			}
		case ',':
			if depth == 0 {
				return true
			}
		}
	}
	return false
}

// isScalaPrimitive reports whether t names a Scala builtin scalar / top-bottom
// type that doesn't warrant a usage edge.
func isScalaPrimitive(t string) bool {
	switch t {
	case "", "Int", "Long", "Short", "Byte", "Float", "Double",
		"Boolean", "Char", "String", "Unit", "Any", "AnyRef", "AnyVal",
		"Nothing", "Null", "Object":
		return true
	}
	return false
}

// scalaTypeAnnotationRaw returns the raw type-annotation text of a val/var
// declaration (`val x: Option[User] = …` -> `Option[User]`), generics intact,
// by reading the type node directly from the declaration's children. The type
// sits as a sibling of the bound identifier in the tree-sitter shape. Returns
// "" when the val/var has no declared type (inferred).
func scalaTypeAnnotationRaw(member *sitter.Node, src []byte) string {
	for i, _nc := 0, int(member.NamedChildCount()); i < _nc; i++ {
		c := member.NamedChild(i)
		if c == nil {
			continue
		}
		switch c.Type() {
		case "type_identifier", "generic_type", "tuple_type", "compound_type",
			"function_type", "stable_type_identifier", "projected_type":
			return strings.TrimSpace(c.Content(src))
		}
	}
	return ""
}

// scalaParamTypeText returns the raw type-annotation text of a `parameter` or
// `class_parameter` node (the first type_identifier / generic_type child), or
// "". The node shape is `name: Type` with the type as a sibling of the bound
// identifier.
func scalaParamTypeText(param *sitter.Node, src []byte) string {
	for i, _nc := 0, int(param.NamedChildCount()); i < _nc; i++ {
		c := param.NamedChild(i)
		if c == nil {
			continue
		}
		switch c.Type() {
		case "type_identifier", "generic_type", "tuple_type", "compound_type",
			"function_type", "stable_type_identifier", "projected_type":
			return strings.TrimSpace(c.Content(src))
		}
	}
	return ""
}

// scalaReturnTypeNode returns the return-type node of a `def` — the
// type_identifier / generic_type child that follows the parameter list. Returns
// nil when the def has no declared return type.
func scalaReturnTypeNode(fn *sitter.Node) *sitter.Node {
	seenParams := false
	for i, _nc := 0, int(fn.NamedChildCount()); i < _nc; i++ {
		c := fn.NamedChild(i)
		if c == nil {
			continue
		}
		switch c.Type() {
		case "parameters", "class_parameters", "type_parameters":
			seenParams = true
			continue
		case "type_identifier", "generic_type", "tuple_type", "compound_type",
			"function_type", "stable_type_identifier", "projected_type":
			// The first type node after the parameter list is the return
			// type. (A def with no params still has its return type as the
			// first type child, so the seenParams gate is permissive.)
			_ = seenParams
			return c
		}
	}
	return nil
}

// emitScalaDefTypeUses emits EdgeTypedAs for every type used in a `def`'s
// signature: each parameter's declared type and the declared return type. The
// edges are attributed to ownerID (the function/method node). Parameters and
// returns annotated only with a primitive emit nothing.
func emitScalaDefTypeUses(fn *sitter.Node, ownerID, filePath string, src []byte, result *parser.ExtractionResult) {
	if fn == nil || ownerID == "" {
		return
	}
	for i, _nc := 0, int(fn.NamedChildCount()); i < _nc; i++ {
		c := fn.NamedChild(i)
		if c == nil || c.Type() != "parameters" {
			continue
		}
		for j, _nc := 0, int(c.NamedChildCount()); j < _nc; j++ {
			p := c.NamedChild(j)
			if p == nil || p.Type() != "parameter" {
				continue
			}
			if txt := scalaParamTypeText(p, src); txt != "" {
				emitScalaTypeUseEdges(ownerID, txt, filePath, int(p.StartPoint().Row)+1, result)
			}
		}
	}
	if rt := scalaReturnTypeNode(fn); rt != nil {
		emitScalaTypeUseEdges(ownerID, strings.TrimSpace(rt.Content(src)), filePath, int(rt.StartPoint().Row)+1, result)
	}
}

// emitScalaClassParamTypeUses emits EdgeTypedAs for each constructor parameter
// type of a class/case-class, attributed to the class's type node (ownerID).
// `class User(repo: Repository)` references Repository.
func emitScalaClassParamTypeUses(classNode *sitter.Node, ownerID, filePath string, src []byte, result *parser.ExtractionResult) {
	if classNode == nil || ownerID == "" {
		return
	}
	for i, _nc := 0, int(classNode.NamedChildCount()); i < _nc; i++ {
		c := classNode.NamedChild(i)
		if c == nil || c.Type() != "class_parameters" {
			continue
		}
		for j, _nc := 0, int(c.NamedChildCount()); j < _nc; j++ {
			p := c.NamedChild(j)
			if p == nil || p.Type() != "class_parameter" {
				continue
			}
			if txt := scalaParamTypeText(p, src); txt != "" {
				emitScalaTypeUseEdges(ownerID, txt, filePath, int(p.StartPoint().Row)+1, result)
			}
		}
	}
}
