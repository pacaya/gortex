package languages

import (
	"strings"
	"unicode"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// emitPythonReferenceForms walks a parsed Python file and emits the
// reference edges that the call/annotation passes don't already cover, so
// find_usages on a class name surfaces every site a class is *used* as a
// type — not just where it is annotated. The forms covered:
//
//   - instantiation     `HttpResponse(...)`        -> EdgeInstantiates
//   - inheritance        `class V(HttpResponse):`   -> EdgeReferences
//   - isinstance/issubclass `isinstance(x, Foo)`    -> EdgeReferences
//   - static / class access `HttpResponse.status`   -> EdgeReferences
//   - decorator          `@HttpResponse` / `@a.Foo` -> EdgeReferences
//
// Python has no `new`: any call whose callee is a Capitalized identifier
// or attribute is treated as a class construction. The capitalization gate
// (isPythonTypeName) is what keeps lowercase function calls, method calls
// on instances, and kwargs out of the reference set.
//
// EdgeReferences targets (inherit / cast / static_access / decorator) are
// stamped OriginASTResolved so the cross-package name-match guard in the
// resolver does not revert them — that guard only polices the two weakest
// confidence tiers, and a structural type reference carries AST-grade
// evidence. EdgeInstantiates is not policed by the guard.
//
// Every edge is attributed to the enclosing function (findEnclosingFunc)
// with the file node as the fallback owner, and carries
// Meta["ref_context"] ∈ {instantiate, inherit, cast, static_access}.
func emitPythonReferenceForms(root *sitter.Node, src []byte, filePath, fileID string, funcRanges []funcRange, result *parser.ExtractionResult) {
	if root == nil {
		return
	}
	// Dedup per (owner, type, line, ref_context).
	type refKey struct {
		owner, typ, useKind string
		line                int
	}
	seen := map[refKey]bool{}

	owner := func(line int) string {
		if id := findEnclosingFunc(funcRanges, line); id != "" {
			return id
		}
		return fileID
	}

	emit := func(typeName string, line int, kind graph.EdgeKind, useKind string) {
		typeName = canonicalizePyTypeRef(typeName)
		if !isPythonTypeName(typeName) {
			return
		}
		from := owner(line)
		k := refKey{owner: from, typ: typeName, useKind: useKind, line: line}
		if seen[k] {
			return
		}
		seen[k] = true
		result.Edges = append(result.Edges, &graph.Edge{
			From:     from,
			To:       "unresolved::" + typeName,
			Kind:     kind,
			FilePath: filePath,
			Line:     line,
			Origin:   graph.OriginASTResolved,
			Meta:     map[string]any{"ref_context": useKind},
		})
	}

	walkPyNodes(root, func(n *sitter.Node) bool {
		switch n.Type() {
		case "call":
			emitPyCallReference(n, src, emit)
		case "class_definition":
			emitPyInheritanceReferences(n, src, emit)
		case "attribute":
			emitPyStaticAccessReference(n, src, emit)
		case "decorator":
			emitPyDecoratorReference(n, src, emit)
		case "type_parameter":
			// Element types inside a type-annotation subscript:
			// `List[Foo]`, `Dict[str, Bar]`, `Optional[Baz]`,
			// `Callable[..., Result]`. The annotation pass records only the
			// canonicalized head (List / Dict / …) and drops the argument
			// types, so without this they never appear as references.
			emitPyGenericArgReferences(n, src, emit)
		}
		return true
	})
}

// emitPyGenericArgReferences emits a "generic_arg" reference for each named
// type appearing as a direct argument of a type-annotation subscript. The
// node passed in is always a `type_parameter` — the grammar only produces
// that inside a `generic_type`, which only appears inside a `type`
// annotation. That structural nesting is the zero-false-positive gate: a
// runtime subscript like `arr[0]` / `d[key]` parses as a `subscript` node
// (never inside a `type`), so this case never fires for it.
//
// Each direct child is a `type` node wrapping an identifier (`Foo`), a
// dotted name (`mod.Qux` — the leaf is the type), an ellipsis (`...` in
// `Callable[..., R]` — no identifier, naturally skipped), a primitive
// (`str`/`int` — dropped by the builtin filter at the emit site), or a
// nested subscript (`List[Foo]` inside `Dict[str, List[Foo]]`). Nesting is
// handled by the outer walk visiting each nested `type_parameter`
// independently; here every direct child is run through the shared
// canonicalizer + capitalization/builtin filter at the emit site, which
// for a wrapper like `List[Foo]` yields the inner element (`Foo`).
func emitPyGenericArgReferences(tparam *sitter.Node, src []byte, emit func(string, int, graph.EdgeKind, string)) {
	line := int(tparam.StartPoint().Row) + 1
	for i, _nc := 0, int(tparam.NamedChildCount()); i < _nc; i++ {
		c := tparam.NamedChild(i)
		if c == nil || c.Type() != "type" {
			continue
		}
		// The argument's name: an identifier or the leaf of a dotted
		// attribute. Subscript wrappers / ellipses are left to the
		// canonicalizer + filter (Optional[X] → X, ... → dropped).
		name := strings.TrimSpace(c.Content(src))
		if inner := c.NamedChild(0); inner != nil && inner.Type() == "attribute" {
			if a := inner.ChildByFieldName("attribute"); a != nil {
				name = a.Content(src)
			}
		}
		emit(name, line, graph.EdgeReferences, "generic_arg")
	}
}

// emitPyCallReference handles a `call` node. Two cases:
//   - the callee is a Capitalized name → class instantiation.
//   - the callee is `isinstance` / `issubclass` → the 2nd argument
//     (possibly a tuple) names types under test (ref_context "cast").
func emitPyCallReference(call *sitter.Node, src []byte, emit func(string, int, graph.EdgeKind, string)) {
	fn := call.ChildByFieldName("function")
	if fn == nil {
		return
	}
	line := int(call.StartPoint().Row) + 1

	if fn.Type() == "identifier" {
		name := fn.Content(src)
		if name == "isinstance" || name == "issubclass" {
			emitPyTypeTestArgs(call, src, line, emit)
			return
		}
	}

	// Instantiation: callee is a Capitalized identifier or the trailing
	// attribute of `a.b.Foo(...)` is Capitalized.
	if name := pyConstructorName(fn, src); name != "" {
		emit(name, line, graph.EdgeInstantiates, "instantiate")
	}
}

// pyConstructorName returns the class name from a call's `function` node
// when it denotes a Capitalized type (bare `Foo` or attribute `mod.Foo`),
// else "". A bare lowercase callee (`foo()`), a method-style attribute on
// a lowercase / instance receiver (`obj.method()`), and builtins are
// rejected by the Capitalization gate at the emit site, but we still
// require the *leaf* name to be Capitalized here so `obj.method()` —
// whose leaf `method` is lowercase — never reaches emit.
func pyConstructorName(fn *sitter.Node, src []byte) string {
	switch fn.Type() {
	case "identifier":
		return fn.Content(src)
	case "attribute":
		// `pkg.mod.Foo(...)` — the leaf attribute is the constructed type.
		if a := fn.ChildByFieldName("attribute"); a != nil {
			leaf := a.Content(src)
			if leaf != "" && isPyCapitalized(leaf) {
				return leaf
			}
		}
	}
	return ""
}

// emitPyTypeTestArgs reads the 2nd positional argument of an
// isinstance/issubclass call and emits a "cast" reference for each named
// type. The arg may be a single name/attribute or a tuple of them.
func emitPyTypeTestArgs(call *sitter.Node, src []byte, line int, emit func(string, int, graph.EdgeKind, string)) {
	args := call.ChildByFieldName("arguments")
	if args == nil {
		return
	}
	// Collect positional args (skip keyword_argument and the separators).
	var positional []*sitter.Node
	for i, _nc := 0, int(args.NamedChildCount()); i < _nc; i++ {
		c := args.NamedChild(i)
		if c == nil {
			continue
		}
		if c.Type() == "keyword_argument" {
			continue
		}
		positional = append(positional, c)
	}
	if len(positional) < 2 {
		return
	}
	second := positional[1]
	emitPyTypeOperand(second, src, line, emit)
}

// emitPyTypeOperand emits a "cast" reference for a type operand that is an
// identifier, an attribute, or a (possibly nested) tuple of those.
func emitPyTypeOperand(n *sitter.Node, src []byte, line int, emit func(string, int, graph.EdgeKind, string)) {
	switch n.Type() {
	case "identifier":
		emit(n.Content(src), line, graph.EdgeReferences, "cast")
	case "attribute":
		if a := n.ChildByFieldName("attribute"); a != nil {
			emit(a.Content(src), line, graph.EdgeReferences, "cast")
		}
	case "tuple", "parenthesized_expression":
		for i, _nc := 0, int(n.NamedChildCount()); i < _nc; i++ {
			emitPyTypeOperand(n.NamedChild(i), src, line, emit)
		}
	}
}

// emitPyInheritanceReferences emits an "inherit" reference per base class
// listed in a class_definition's superclasses argument_list. Keyword
// bases (`metaclass=...`) are skipped — they are not supertypes.
func emitPyInheritanceReferences(class *sitter.Node, src []byte, emit func(string, int, graph.EdgeKind, string)) {
	supers := class.ChildByFieldName("superclasses")
	if supers == nil {
		return
	}
	line := int(class.StartPoint().Row) + 1
	for i, _nc := 0, int(supers.NamedChildCount()); i < _nc; i++ {
		c := supers.NamedChild(i)
		if c == nil {
			continue
		}
		switch c.Type() {
		case "identifier":
			emit(c.Content(src), line, graph.EdgeReferences, "inherit")
		case "attribute":
			// `class V(http.HttpResponse):` — leaf names the base.
			if a := c.ChildByFieldName("attribute"); a != nil {
				emit(a.Content(src), line, graph.EdgeReferences, "inherit")
			}
		case "subscript":
			// Generic base `class V(Generic[T]):` / `Mapping[str, int]`.
			// The base type is the subscript's value.
			if v := c.ChildByFieldName("value"); v != nil {
				switch v.Type() {
				case "identifier":
					emit(v.Content(src), line, graph.EdgeReferences, "inherit")
				case "attribute":
					if a := v.ChildByFieldName("attribute"); a != nil {
						emit(a.Content(src), line, graph.EdgeReferences, "inherit")
					}
				}
			}
		}
	}
}

// emitPyStaticAccessReference handles `Foo.BAR` / `Foo.method` — an
// attribute access whose object is a Capitalized name (class static /
// class-method / class-constant access). Attribute chains where the
// object is itself an attribute (`a.B.c`) only fire when the immediate
// object segment is Capitalized; the outer walk visits the inner
// attribute separately. Method calls on instances (`obj.method()`) and
// module access (`os.path`) are excluded by the Capitalization gate on
// the object.
func emitPyStaticAccessReference(attr *sitter.Node, src []byte, emit func(string, int, graph.EdgeKind, string)) {
	obj := attr.ChildByFieldName("object")
	if obj == nil || obj.Type() != "identifier" {
		return
	}
	name := obj.Content(src)
	if !isPyCapitalized(name) {
		return
	}
	emit(name, int(attr.StartPoint().Row)+1, graph.EdgeReferences, "static_access")
}

// emitPyDecoratorReference emits a "static_access" reference when a
// decorator names a Capitalized type — `@HttpResponse`, `@a.Validator`,
// or call-form `@Validator(...)`. Lowercase decorators (`@property`,
// `@app.route`) are excluded by the Capitalization gate.
func emitPyDecoratorReference(dec *sitter.Node, src []byte, emit func(string, int, graph.EdgeKind, string)) {
	line := int(dec.StartPoint().Row) + 1
	for i, _nc := 0, int(dec.NamedChildCount()); i < _nc; i++ {
		c := dec.NamedChild(i)
		if c == nil {
			continue
		}
		switch c.Type() {
		case "identifier":
			emit(c.Content(src), line, graph.EdgeReferences, "static_access")
		case "attribute":
			if a := c.ChildByFieldName("attribute"); a != nil {
				emit(a.Content(src), line, graph.EdgeReferences, "static_access")
			}
		case "call":
			// `@Validator(...)` — the callee names the referenced type.
			if fn := c.ChildByFieldName("function"); fn != nil {
				if name := pyConstructorName(fn, src); name != "" {
					emit(name, line, graph.EdgeReferences, "static_access")
				}
			}
		}
		// Only the first named child carries the decorator expression.
		break
	}
}

// isPythonTypeName is the Capitalization gate: a PascalCase / Capitalized
// name is treated as a class. Lowercase names (functions, locals, kwargs)
// and lowercase builtins (`dict`, `list`, `int`, …) are rejected, which is
// what keeps the reference set free of false positives. Builtin
// *Capitalized* type-likes that aren't user classes are not specially
// excluded — they simply never resolve to a def node, so they cost
// nothing.
func isPythonTypeName(name string) bool {
	if name == "" {
		return false
	}
	if !isPyCapitalized(name) {
		return false
	}
	// canonicalizePyTypeRef already strips wrappers/qualifiers; guard the
	// few all-caps / typing sentinels that are not real classes.
	switch name {
	case "None", "True", "False", "Any", "Never", "Optional", "Union", "Type", "Self":
		return false
	}
	return true
}

// isPyCapitalized reports whether the first rune is an uppercase letter.
func isPyCapitalized(name string) bool {
	if name == "" {
		return false
	}
	r := []rune(name)[0]
	return unicode.IsUpper(r)
}
