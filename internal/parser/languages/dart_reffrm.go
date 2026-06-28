package languages

import (
	"strconv"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// emitDartReferenceForms emits the Dart reference edges that the symbol and
// type-use passes miss — the *expression-site* uses of a type that the
// declaration-position type-use pass (EdgeTypedAs) and the symbol extractors
// do not cover:
//
//   - INSTANTIATION   `new Foo(...)` (new_expression), `const Foo(...)` /
//     `const Foo.named(...)` (const_object_expression), and an unadorned
//     `Foo(...)` whose callee is a Capitalized identifier (Dart omits `new`)
//     → EdgeInstantiates
//   - INHERITANCE     `class X extends Base with M implements I` — the
//     superclass (`extends`), mixin (`with`), and interface (`implements`)
//     clauses → EdgeReferences, Meta["ref_context"]="inherit"
//   - CAST / TYPE-TEST `x as Foo` (type_cast), `x is Foo` / `x is! Foo`
//     (type_test) → EdgeReferences, Meta["ref_context"]="cast"
//   - STATIC ACCESS   `Foo.constant` / `Foo.staticMethod()` / `Foo.named()`
//     — a member/static access whose head is a bare Capitalized identifier
//     → EdgeReferences, Meta["ref_context"]="static_access"
//   - GENERIC ARG     the element type(s) of a parameterised type
//     `List<Foo>` / `Map<String, Bar>` / `Future<Response>` / a supertype
//     `extends Base<Foo>`, in any position (variable annotation, parameter,
//     return, supertype, or nested) → EdgeReferences,
//     Meta["ref_context"]="generic_arg"
//
// Each edge attributes to the enclosing function / method (the file node when
// nothing encloses the line) so find_usages lands the reference without a
// language server. Targets are left at the canonical bare-name placeholder
// `unresolved::Foo` for the resolver to bind.
//
// Origin is load-bearing. The structural reference edges (inherit / cast /
// static_access) ride graph.OriginASTResolved: the cross-package guard
// (internal/resolver/cross_pkg_guard.go) reverts EdgeReferences / EdgeCalls
// edges to their unresolved placeholder *only* at the two weakest tiers
// (text_matched / ast_inferred), so an ast_resolved edge is out of its scope
// and survives. EdgeInstantiates is not a call-like edge the guard polices at
// all, but it carries the same origin for consistency.
//
// Scope discipline keeps the graph clean and avoids double-emitting:
//
//   - Only Capitalized, non-primitive leaf type names produce an edge — the
//     Capitalization gate (isDartTypeNameCapitalized) drops lowercase locals,
//     functions, and `this`; isDartPrimitive drops int/String/void/….  This
//     is what stops a lowercase function call `foo()` from being mistaken for
//     a construction: only a Capitalized callee instantiates.
//   - Bare `Foo(...)` instantiation for a *local* type is already emitted by
//     extractCalls (it owns the same-file type case). This pass only emits the
//     bare-call instantiation for types NOT defined in the file, so an
//     imported `Client()` surfaces while a same-file `Widget()` is not
//     double-counted.
//   - Static access only counts when the head is a *bare* Capitalized
//     identifier that begins a selector chain — `this.x`, `local.Foo`, and a
//     chained `a.b.C` (whose head identifier is `a`) are all excluded, so a
//     field read on an instance is never read as a static type reference.
//   - The mixin / interface / superclass type identifiers are taken from the
//     dedicated grammar clauses (superclass / mixins / interfaces), so the
//     inherit edge names the supertype itself and not its annotations. The
//     supertype's *generic arguments* (`extends Base<Foo>`) are surfaced
//     separately by the position-independent type_arguments walk as
//     generic_arg references, not as inherit references.
func (e *DartExtractor) emitDartReferenceForms(
	root *sitter.Node, src []byte, filePath string, fileNode *graph.Node,
	result *parser.ExtractionResult,
) {
	if root == nil {
		return
	}
	funcRanges := buildFuncRanges(result)

	// Local type names — a bare call to one of these is a construction that
	// extractCalls already emits as EdgeInstantiates. Skip it here so the
	// instantiation is counted once.
	localTypes := map[string]bool{}
	for _, n := range result.Nodes {
		if n != nil && (n.Kind == graph.KindType || n.Kind == graph.KindInterface) {
			localTypes[n.Name] = true
		}
	}

	// Dedup across the whole file on (owner, type, line, ref_context) so a type
	// referenced twice in the same role on the same line emits one edge.
	seen := map[string]bool{}

	ownerFor := func(line int) string {
		if owner := findEnclosingFunc(funcRanges, line); owner != "" {
			return owner
		}
		return fileNode.ID
	}

	// emit appends one reference edge for a single Capitalized, non-primitive
	// type. refContext "" pairs with EdgeInstantiates; a non-empty refContext
	// pairs with EdgeReferences and rides in Meta["ref_context"].
	emit := func(rawType string, line int, kind graph.EdgeKind, refContext string) {
		canon := dartBareTypeName(rawType)
		if canon == "" || isDartPrimitive(canon) || !isDartTypeNameCapitalized(canon) {
			return
		}
		owner := ownerFor(line)
		if owner == "" {
			return
		}
		key := owner + "\x00" + canon + "\x00" + strconv.Itoa(line) + "\x00" + refContext
		if seen[key] {
			return
		}
		seen[key] = true
		edge := &graph.Edge{
			From:     owner,
			To:       "unresolved::" + canon,
			Kind:     kind,
			FilePath: filePath,
			Line:     line,
			Origin:   graph.OriginASTResolved,
		}
		if refContext != "" {
			edge.Meta = map[string]any{"ref_context": refContext}
		}
		result.Edges = append(result.Edges, edge)
	}

	walkNodes(root, func(n *sitter.Node) {
		line := int(n.StartPoint().Row) + 1
		switch n.Type() {

		case "class_definition":
			// Inheritance clauses — superclass (`extends` + `with`), interfaces
			// (`implements`). The grammar nests the mixin list inside the
			// superclass node, so a single class_definition carries all three.
			emitDartInheritanceClauses(n, src, emit)

		case "new_expression":
			// `new Foo(...)` — the type is the leading type_identifier child.
			if name := dartFirstTypeIdentifier(n, src); name != "" {
				emit(name, line, graph.EdgeInstantiates, "")
			}

		case "const_object_expression":
			// `const Foo(...)` / `const Foo.named(...)` — the constructed type is
			// the leading type_identifier (the optional `.named` selector that
			// follows names a constructor, not a distinct type).
			if name := dartFirstTypeIdentifier(n, src); name != "" {
				emit(name, line, graph.EdgeInstantiates, "")
			}

		case "type_cast":
			// `x as Foo` — `as_operator` then the target type_identifier.
			if name := dartFirstTypeIdentifier(n, src); name != "" {
				emit(name, line, graph.EdgeReferences, graph.RefContextCast)
			}

		case "type_test":
			// `x is Foo` / `x is! Foo` — `is_operator` then the tested
			// type_identifier.
			if name := dartFirstTypeIdentifier(n, src); name != "" {
				emit(name, line, graph.EdgeReferences, graph.RefContextCast)
			}

		case "type_arguments":
			// `<Foo>` / `<String, Bar>` — generic argument list. This is the
			// position-independent generic-arg surface: a `type_arguments` node
			// appears verbatim in every position a parameterised type can occur
			// (variable annotation `List<Foo>`, parameter `Set<Account>`, return
			// `Future<Response>`, a supertype clause `extends Base<Foo>` /
			// `with Mix<Bar>` / `implements Iface<Baz>`, and nested inside another
			// type_arguments `Map<String, List<Bar>>`). Each direct
			// type_identifier child is an element type used here; a nested
			// type_arguments child is its own node that walkNodes visits
			// separately, so emitting only the *direct* element types avoids
			// double-counting while still surfacing every level. The grammar's
			// declaration-position node for `<T>` is type_parameters (a distinct
			// node), so a generic *parameter* declaration is never misread as a
			// use. The emit closure applies the Capitalization + primitive gate
			// and the per-(owner,type,line,ref_context) dedup.
			emitDartGenericArgs(n, src, line, emit)

		case "identifier":
			// A bare identifier that heads an expression: it is either an
			// unadorned construction `Foo(...)` or a static access `Foo.member`
			// — distinguished by the selector chain that follows.
			emitDartIdentifierHead(n, src, line, localTypes, emit)
		}
	})
}

// emitDartInheritanceClauses walks a class_definition's superclass / interfaces
// clauses and emits an inherit reference for each named supertype, mixin, and
// interface. The grammar shape (verified against the Dart tree-sitter
// grammar) is:
//
//	class_definition
//	  superclass
//	    extends
//	    type_identifier        <- superclass
//	    mixins
//	      with
//	      type_identifier ...  <- mixins
//	  interfaces
//	    implements
//	    type_identifier ...    <- interfaces
//
// Only the dedicated clause type_identifiers are taken as inherit references,
// so a supertype's generic arguments and the class's own name are never pulled
// in here — the supertype's type_arguments (`extends Base<Foo>`) ride the
// generic_arg walk instead. The emit closure applies the Capitalization +
// primitive gate, so a malformed clause yields nothing.
func emitDartInheritanceClauses(classNode *sitter.Node, src []byte, emit func(rawType string, line int, kind graph.EdgeKind, refContext string)) {
	for i, _nc := 0, int(classNode.ChildCount()); i < _nc; i++ {
		clause := classNode.Child(i)
		if clause == nil {
			continue
		}
		switch clause.Type() {
		case "superclass":
			for j, _nc := 0, int(clause.ChildCount()); j < _nc; j++ {
				c := clause.Child(j)
				if c == nil {
					continue
				}
				switch c.Type() {
				case "type_identifier":
					// The `extends` supertype is a direct child of superclass.
					emit(c.Content(src), int(c.StartPoint().Row)+1, graph.EdgeReferences, graph.RefContextInherit)
				case "mixins":
					// `with M, N` — each mixin is a type_identifier child.
					emitDartClauseTypes(c, src, emit)
				}
			}
		case "interfaces":
			// `implements I, J` — each interface is a type_identifier child.
			emitDartClauseTypes(clause, src, emit)
		}
	}
}

// emitDartClauseTypes emits an inherit reference for every type_identifier
// child of a clause node (mixins / interfaces).
func emitDartClauseTypes(clause *sitter.Node, src []byte, emit func(rawType string, line int, kind graph.EdgeKind, refContext string)) {
	for i, _nc := 0, int(clause.ChildCount()); i < _nc; i++ {
		c := clause.Child(i)
		if c != nil && c.Type() == "type_identifier" {
			emit(c.Content(src), int(c.StartPoint().Row)+1, graph.EdgeReferences, graph.RefContextInherit)
		}
	}
}

// emitDartIdentifierHead handles a bare identifier that heads an expression and
// classifies it as a construction or a static access by scanning the selector
// chain that follows it.
//
// The identifier must be the *leading* identifier of the chain — one that is
// not itself a child of a selector (the same anchor extractCalls uses). This
// excludes the `B` in `a.B.c` (whose chain head is `a`) so an instance field
// access is never misread as a static type reference.
//
// Two shapes are emitted:
//
//   - `Foo(...)`        an identifier immediately followed by a selector that
//     carries an argument_part, with NO intervening `.member` selector → an
//     unadorned construction. Only emitted for a non-local type (extractCalls
//     owns the local-type case).
//   - `Foo.member` /
//     `Foo.method(...)`  an identifier followed by a `.member` selector
//     (unconditional_assignable_selector) → a static access referencing Foo,
//     regardless of trailing arguments.
//
// The Capitalization gate inside emit() is what rejects a lowercase callee
// `foo()` (a free-function call, not a construction) and a lowercase head
// `obj.method()` (an instance call, not a static access).
func emitDartIdentifierHead(n *sitter.Node, src []byte, line int, localTypes map[string]bool, emit func(rawType string, line int, kind graph.EdgeKind, refContext string)) {
	// Skip identifiers that live inside a selector chain — only the chain head
	// is classified (mirrors extractCalls's anchor guard).
	if p := n.Parent(); p != nil {
		switch p.Type() {
		case "unconditional_assignable_selector",
			"conditional_assignable_selector",
			"selector":
			return
		}
	}

	head := n.Content(src)

	// Scan forward through selector siblings. The first selector decides the
	// shape: a leading `.member` selector → static access; a leading
	// argument_part selector → construction.
	for sib := n.NextSibling(); sib != nil; sib = sib.NextSibling() {
		if sib.Type() != "selector" {
			break
		}
		isMember := false
		isCall := false
		for j, _nc := 0, int(sib.ChildCount()); j < _nc; j++ {
			switch sib.Child(j).Type() {
			case "unconditional_assignable_selector",
				"conditional_assignable_selector":
				isMember = true
			case "argument_part", "arguments":
				isCall = true
			}
		}
		if isMember {
			// `Foo.member` — static access referencing the head type.
			emit(head, line, graph.EdgeReferences, graph.RefContextStaticAccess)
			return
		}
		if isCall {
			// `Foo(...)` — unadorned construction. Local types are emitted by
			// extractCalls; only surface non-local capitalized callees here.
			if !localTypes[head] {
				emit(head, line, graph.EdgeInstantiates, "")
			}
			return
		}
	}
}

// emitDartGenericArgs emits a generic_arg reference for every direct
// type_identifier child of a type_arguments node. Each element type rides
// EdgeReferences with Meta["ref_context"]="generic_arg" and OriginASTResolved.
//
// Only *direct* children are read: a nested generic (`List<Bar>` inside
// `Map<String, List<Bar>>`) is its own type_arguments node that the walk visits
// independently, so descending here would double-count it. The line is the
// outer type_arguments node's start row (the use site of these element types).
func emitDartGenericArgs(targs *sitter.Node, src []byte, line int, emit func(rawType string, line int, kind graph.EdgeKind, refContext string)) {
	if targs == nil {
		return
	}
	for i, _nc := 0, int(targs.NamedChildCount()); i < _nc; i++ {
		c := targs.NamedChild(i)
		if c != nil && c.Type() == "type_identifier" {
			emit(c.Content(src), line, graph.EdgeReferences, graph.RefContextGenericArg)
		}
	}
}

// dartFirstTypeIdentifier returns the content of the first type_identifier
// child of node, or "". Used to pull the constructed / cast / tested type out
// of new_expression, const_object_expression, type_cast, and type_test nodes.
func dartFirstTypeIdentifier(node *sitter.Node, src []byte) string {
	if node == nil {
		return ""
	}
	for i, _nc := 0, int(node.ChildCount()); i < _nc; i++ {
		c := node.Child(i)
		if c != nil && c.Type() == "type_identifier" {
			return c.Content(src)
		}
	}
	return ""
}

// isDartTypeNameCapitalized reports whether a bare type name begins with an
// uppercase ASCII letter — the Capitalization gate that keeps lowercase
// locals, parameters, functions, and keywords (`this`, `foo`) out of the
// reference surface. dartBareTypeName has already stripped library prefixes,
// nullable markers, and whitespace, so the first byte of the bare name is the
// discriminator.
func isDartTypeNameCapitalized(name string) bool {
	if name == "" {
		return false
	}
	c := name[0]
	return c >= 'A' && c <= 'Z'
}
