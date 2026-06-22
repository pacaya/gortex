package languages

import (
	"strconv"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// emitScalaReferenceForms walks a parsed Scala compilation unit and emits the
// structural reference edges that #143's type-annotation pass did not cover:
//
//   - INSTANTIATION — `new Foo(...)` (instance_expression) and a Capitalized
//     apply `Foo(...)` (call_expression on a Capitalized identifier) emit
//     graph.EdgeInstantiates to the constructed type.
//   - INHERITANCE — every supertype / mixin in a class/object/trait's
//     `extends_clause` (`class X extends Base with T1 with T2`) emits
//     EdgeReferences with Meta["ref_context"]=RefContextInherit, attributed to
//     the defining type.
//   - CASTS / TYPE-TESTS — `x.isInstanceOf[Foo]`, `x.asInstanceOf[Foo]`, and a
//     `case _: Foo` typed match pattern emit EdgeReferences with
//     ref_context=RefContextCast.
//   - STATIC / OBJECT access — member access whose head is a bare Capitalized
//     identifier (`Foo.CONST`, `Foo.apply`, `Foo.method`) emits EdgeReferences
//     with ref_context=RefContextStaticAccess to the access root.
//
// All edges are stamped Origin=graph.OriginASTResolved so the cross-package
// name-match guard (which only reverts the two weakest tiers) leaves the bare
// `unresolved::X` placeholders intact for resolveTypeOrFunc / resolveTypeRef to
// bind. Type names are canonicalized with the shared Scala normalizer (square
// bracket generics stripped, container wrappers unwrapped, dotted prefix
// dropped) and primitives are filtered, exactly as the annotation pass does.
//
// Scope rules that keep the pass from double-emitting #143's val/var/param/
// return annotation edges or flooding the graph with noise:
//   - only Capitalized leaf type names survive (a `Foo`, never a `foo`);
//   - static access fires only when the access head is a bare Capitalized
//     identifier — `this.x`, `super.x`, and lowercase-val selects are excluded;
//   - per-(owner, type, line, ref_context) dedup drops repeats.
func emitScalaReferenceForms(root *sitter.Node, filePath string, src []byte, result *parser.ExtractionResult) {
	if root == nil {
		return
	}
	funcRanges := buildFuncRanges(result)
	emitted := make(map[string]bool)

	// emit appends one reference-form edge, deduplicated per
	// (owner, type, line, ref_context). kind is EdgeInstantiates for
	// construction and EdgeReferences for the structural ref_context forms.
	emit := func(ownerID, typeName, filePath string, line int, kind graph.EdgeKind, refCtx string) {
		if ownerID == "" || typeName == "" {
			return
		}
		canon := canonicalizeScalaTypeRef(typeName)
		if canon == "" || isScalaPrimitive(canon) || !isScalaCapitalized(canon) {
			return
		}
		key := ownerID + "\x00" + canon + "\x00" + strconv.Itoa(line) + "\x00" + refCtx
		if emitted[key] {
			return
		}
		emitted[key] = true
		e := &graph.Edge{
			From:     ownerID,
			To:       "unresolved::" + canon,
			Kind:     kind,
			FilePath: filePath,
			Line:     line,
			Origin:   graph.OriginASTResolved,
		}
		if refCtx != "" {
			e.Meta = map[string]any{"ref_context": refCtx}
		}
		result.Edges = append(result.Edges, e)
	}

	// ownerAt returns the enclosing function/method node id for an
	// expression-position reference, or the file node when none.
	ownerAt := func(line int) string {
		if id := findEnclosingFunc(funcRanges, line); id != "" {
			return id
		}
		return filePath
	}

	walkNodes(root, func(n *sitter.Node) {
		switch n.Type() {
		case "class_definition", "object_definition", "trait_definition", "enum_definition":
			emitScalaInheritEdges(n, filePath, src, emit)
		case "instance_expression":
			// `new Foo(...)` — the constructed type is the type_identifier /
			// generic_type child directly under the `new`.
			line := int(n.StartPoint().Row) + 1
			owner := ownerAt(line)
			if t := scalaInstanceType(n, src); t != "" {
				emit(owner, t, filePath, line, graph.EdgeInstantiates, "")
			}
		case "call_expression":
			// A Capitalized apply `Foo(...)` is companion-apply construction.
			// (`foo()` and method-selector calls are left to extractCall.)
			callee := n.NamedChild(0)
			if callee == nil || callee.Type() != "identifier" {
				return
			}
			name := strings.TrimSpace(callee.Content(src))
			if !isScalaCapitalized(name) {
				return
			}
			line := int(n.StartPoint().Row) + 1
			emit(ownerAt(line), name, filePath, line, graph.EdgeInstantiates, "")
		case "generic_function":
			// `x.isInstanceOf[Foo]` / `x.asInstanceOf[Foo]` — the cast target
			// is the lone type_arguments entry; only fire for the two builtin
			// type-test methods.
			if !scalaIsInstanceCheck(n, src) {
				return
			}
			line := int(n.StartPoint().Row) + 1
			owner := ownerAt(line)
			for _, t := range scalaTypeArgNames(n, src) {
				emit(owner, t, filePath, line, graph.EdgeReferences, graph.RefContextCast)
			}
		case "typed_pattern":
			// `case _: Foo` / `case d: Foo` — the matched-against type is the
			// type_identifier / generic_type child of the pattern.
			line := int(n.StartPoint().Row) + 1
			owner := ownerAt(line)
			if t := scalaPatternType(n, src); t != "" {
				emit(owner, t, filePath, line, graph.EdgeReferences, graph.RefContextCast)
			}
		case "field_expression":
			// `Foo.CONST` / `Foo.apply` — static / companion-object access when
			// the head is a bare Capitalized identifier. Excludes `this.x`,
			// `super.x`, lowercase-val selects, and the receiver of an
			// isInstanceOf/asInstanceOf check (its head is lowercase anyway).
			head := n.NamedChild(0)
			if head == nil || head.Type() != "identifier" {
				return
			}
			name := strings.TrimSpace(head.Content(src))
			if !isScalaCapitalized(name) {
				return
			}
			line := int(n.StartPoint().Row) + 1
			emit(ownerAt(line), name, filePath, line, graph.EdgeReferences, graph.RefContextStaticAccess)
		}
	})
}

// emitScalaInheritEdges emits an inherit-context EdgeReferences from a defining
// type to every supertype / mixin listed in its extends_clause. The owner is
// the type's own node id (`filePath::Name`), matching the id the extractors
// mint for the class/object/trait/enum.
func emitScalaInheritEdges(def *sitter.Node, filePath string, src []byte, emit func(ownerID, typeName, filePath string, line int, kind graph.EdgeKind, refCtx string)) {
	name := scalaFindChildIdentifier(def, src)
	if name == "" {
		return
	}
	ownerID := filePath + "::" + name
	for i := 0; i < int(def.NamedChildCount()); i++ {
		c := def.NamedChild(i)
		if c == nil || c.Type() != "extends_clause" {
			continue
		}
		for j := 0; j < int(c.NamedChildCount()); j++ {
			t := c.NamedChild(j)
			if t == nil {
				continue
			}
			switch t.Type() {
			case "type_identifier", "generic_type", "stable_type_identifier", "projected_type", "compound_type":
				line := int(t.StartPoint().Row) + 1
				emit(ownerID, strings.TrimSpace(t.Content(src)), filePath, line, graph.EdgeReferences, graph.RefContextInherit)
			}
		}
	}
}

// scalaInstanceType returns the constructed type text of a `new Foo(...)`
// instance_expression — the type_identifier / generic_type child following the
// `new` keyword, generics intact — or "".
func scalaInstanceType(n *sitter.Node, src []byte) string {
	for i := 0; i < int(n.NamedChildCount()); i++ {
		c := n.NamedChild(i)
		if c == nil {
			continue
		}
		switch c.Type() {
		case "type_identifier", "generic_type", "stable_type_identifier", "projected_type", "compound_type":
			return strings.TrimSpace(c.Content(src))
		}
	}
	return ""
}

// scalaIsInstanceCheck reports whether a generic_function node is an
// `x.isInstanceOf[...]` / `x.asInstanceOf[...]` invocation, by reading the
// method-name identifier of its field_expression callee.
func scalaIsInstanceCheck(n *sitter.Node, src []byte) bool {
	for i := 0; i < int(n.NamedChildCount()); i++ {
		fe := n.NamedChild(i)
		if fe == nil || fe.Type() != "field_expression" {
			continue
		}
		// The method name is the last identifier child of the selector.
		for j := int(fe.NamedChildCount()) - 1; j >= 0; j-- {
			id := fe.NamedChild(j)
			if id == nil || id.Type() != "identifier" {
				continue
			}
			switch strings.TrimSpace(id.Content(src)) {
			case "isInstanceOf", "asInstanceOf":
				return true
			}
			return false
		}
	}
	return false
}

// scalaTypeArgNames returns the named type entries of a node's `type_arguments`
// child (`[Foo, Bar]` -> ["Foo", "Bar"]), generics intact, or nil.
func scalaTypeArgNames(n *sitter.Node, src []byte) []string {
	var out []string
	for i := 0; i < int(n.NamedChildCount()); i++ {
		ta := n.NamedChild(i)
		if ta == nil || ta.Type() != "type_arguments" {
			continue
		}
		for j := 0; j < int(ta.NamedChildCount()); j++ {
			t := ta.NamedChild(j)
			if t == nil {
				continue
			}
			switch t.Type() {
			case "type_identifier", "generic_type", "stable_type_identifier", "projected_type", "compound_type":
				out = append(out, strings.TrimSpace(t.Content(src)))
			}
		}
	}
	return out
}

// scalaPatternType returns the type a typed_pattern matches against
// (`case d: Foo` -> "Foo"), generics intact, or "".
func scalaPatternType(n *sitter.Node, src []byte) string {
	for i := 0; i < int(n.NamedChildCount()); i++ {
		c := n.NamedChild(i)
		if c == nil {
			continue
		}
		switch c.Type() {
		case "type_identifier", "generic_type", "stable_type_identifier", "projected_type", "compound_type":
			return strings.TrimSpace(c.Content(src))
		}
	}
	return ""
}

// isScalaCapitalized reports whether name's first rune is an uppercase ASCII
// letter — the lexical gate that separates a type / object reference (`Foo`)
// from a value / method (`foo`, `this`, lowercase vals). Empty / non-letter
// leading names are rejected.
func isScalaCapitalized(name string) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return false
	}
	c := name[0]
	return c >= 'A' && c <= 'Z'
}
