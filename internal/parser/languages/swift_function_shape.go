package languages

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// emitSwiftTypeUseEdges emits an EdgeTypedAs from ownerID to
// "unresolved::"+<bare type name> for the type named in typeText, once
// per canonicalized leaf type. It mirrors the *_function_shape.go idiom
// of the other language extractors: it splits wrapper / sugar types,
// canonicalizes to the bare named type via swiftBaseTypeName (stripping
// optionals `?`/`!`, arrays `[T]`, dictionaries `[K:V]`, generics
// `Foo<Bar>`, opaque/existential `some`/`any`, and module qualification),
// skips Swift primitives, and stamps Origin graph.OriginASTInferred so
// the edge participates in LSP-free type resolution / find_usages.
//
// A dictionary type contributes both its key and value leaf type; a
// generic type contributes the constructor name and each generic
// argument (`Result<Value, Error>` → Result, Value, Error). Empty,
// primitive, and already-emitted targets are dropped.
func emitSwiftTypeUseEdges(ownerID, typeText, filePath string, line int, result *parser.ExtractionResult) {
	if ownerID == "" || result == nil {
		return
	}
	for _, leaf := range swiftTypeLeafNames(typeText) {
		if leaf == "" || isSwiftPrimitive(leaf) {
			continue
		}
		result.Edges = append(result.Edges, &graph.Edge{
			From:     ownerID,
			To:       "unresolved::" + leaf,
			Kind:     graph.EdgeTypedAs,
			FilePath: filePath,
			Line:     line,
			Origin:   graph.OriginASTInferred,
		})
	}
}

// swiftTypeLeafNames decomposes a Swift type expression into every named
// leaf type it references. It walks composite forms — dictionaries
// (`[K: V]` → K, V), generic arguments (`Foo<A, B>` → Foo, A, B),
// optionals / arrays / module qualification — so each component lands a
// separate type-use edge. Tuples and protocol compositions (`A & B`)
// split on their separators too. Order-preserving, de-duplicated.
func swiftTypeLeafNames(typeText string) []string {
	typeText = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(typeText), ":"))
	if typeText == "" {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	add := func(name string) {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			return
		}
		seen[name] = true
		out = append(out, name)
	}
	var walk func(t string)
	walk = func(t string) {
		t = strings.TrimSpace(t)
		t = strings.TrimPrefix(t, "some ")
		t = strings.TrimPrefix(t, "any ")
		t = strings.TrimRight(t, "?!")
		t = strings.TrimSpace(t)
		if t == "" {
			return
		}
		// Array / dictionary / tuple sugar: strip the outer bracket pair
		// and recurse on the comma- or colon-separated inner types.
		if (strings.HasPrefix(t, "[") && strings.HasSuffix(t, "]")) ||
			(strings.HasPrefix(t, "(") && strings.HasSuffix(t, ")")) {
			inner := t[1 : len(t)-1]
			// Dictionary `K: V` and tuple `A, B` both split into parts.
			parts := splitSwiftTopLevelCommas(inner)
			for _, p := range parts {
				if ci := swiftTopLevelColon(p); ci >= 0 {
					walk(p[:ci])
					walk(p[ci+1:])
				} else {
					walk(p)
				}
			}
			return
		}
		// Protocol composition `A & B`.
		if amp := swiftTopLevelByte(t, '&'); amp >= 0 {
			walk(t[:amp])
			walk(t[amp+1:])
			return
		}
		// Generic `Foo<A, B>`: constructor + each argument.
		if lt := strings.IndexByte(t, '<'); lt >= 0 && strings.HasSuffix(t, ">") {
			add(swiftBaseTypeName(t[:lt]))
			for _, arg := range splitSwiftTopLevelCommas(t[lt+1 : len(t)-1]) {
				walk(arg)
			}
			return
		}
		add(swiftBaseTypeName(t))
	}
	walk(typeText)
	return out
}

// swiftTopLevelColon returns the index of the first `:` in s that is not
// nested inside <>, (), [], or {} (a dictionary key/value separator), or
// -1 when none. Distinct from a `::` module path, which Swift does not use.
func swiftTopLevelColon(s string) int {
	depth := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '(', '<', '[', '{':
			depth++
		case ')', '>', ']', '}':
			if depth > 0 {
				depth--
			}
		case ':':
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// swiftTopLevelByte returns the index of the first b in s that is not
// nested inside <>, (), [], or {}, or -1 when none.
func swiftTopLevelByte(s string, b byte) int {
	depth := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '(', '<', '[', '{':
			depth++
		case ')', '>', ']', '}':
			if depth > 0 {
				depth--
			}
		case b:
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// emitSwiftFunctionTypeEdges emits EdgeTypedAs edges for a function /
// method declaration's parameter and return type annotations, attributed
// to ownerID (the function or method node). Type-parameter names declared
// on the function itself (`func g<T>(x: T)`) are skipped so a generic
// placeholder doesn't synthesise a bogus `unresolved::T` edge.
//
// The Swift grammar exposes each parameter's type as a type-bearing child
// of a `parameter` node and the return type as a type-bearing sibling of
// `function_declaration` that follows the parameters and precedes the
// `function_body` / `type_constraints`. Both are read verbatim and lowered
// through emitSwiftTypeUseEdges.
func emitSwiftFunctionTypeEdges(ownerID string, decl *sitter.Node, src []byte, filePath string, line int, result *parser.ExtractionResult) {
	if decl == nil || ownerID == "" {
		return
	}
	skip := swiftTypeParameterNames(decl, src)

	emit := func(typeNode *sitter.Node) {
		if typeNode == nil {
			return
		}
		text := strings.TrimSpace(typeNode.Content(src))
		for _, leaf := range swiftTypeLeafNames(text) {
			if leaf == "" || isSwiftPrimitive(leaf) || skip[leaf] {
				continue
			}
			result.Edges = append(result.Edges, &graph.Edge{
				From:     ownerID,
				To:       "unresolved::" + leaf,
				Kind:     graph.EdgeTypedAs,
				FilePath: filePath,
				Line:     line,
				Origin:   graph.OriginASTInferred,
			})
		}
	}

	for i, _nc := 0, int(decl.NamedChildCount()); i < _nc; i++ {
		c := decl.NamedChild(i)
		if c == nil {
			continue
		}
		switch c.Type() {
		case "parameter":
			emit(swiftParamTypeNode(c))
		case "user_type", "optional_type", "array_type", "dictionary_type",
			"tuple_type", "protocol_composition_type", "function_type", "metatype":
			// A type-bearing child of the function_declaration that is not
			// inside a `parameter` is the return type (the grammar places it
			// between the parameter list and the body).
			emit(c)
		}
	}
}

// swiftParamTypeNode returns the type-bearing child of a `parameter`
// node — the node carrying the declared type after the argument
// label(s). Returns nil for an untyped parameter.
func swiftParamTypeNode(param *sitter.Node) *sitter.Node {
	if param == nil {
		return nil
	}
	for i, _nc := 0, int(param.NamedChildCount()); i < _nc; i++ {
		c := param.NamedChild(i)
		if c == nil {
			continue
		}
		switch c.Type() {
		case "user_type", "optional_type", "array_type", "dictionary_type",
			"tuple_type", "protocol_composition_type", "function_type", "metatype",
			"type_identifier":
			return c
		}
	}
	return nil
}

// swiftTypeParameterNames returns the set of generic type-parameter names
// declared on a function declaration's `type_parameters` clause
// (`func g<T, U>` → {T, U}), so they can be excluded from type-use edges.
func swiftTypeParameterNames(decl *sitter.Node, src []byte) map[string]bool {
	out := map[string]bool{}
	if decl == nil {
		return out
	}
	for i, _nc := 0, int(decl.NamedChildCount()); i < _nc; i++ {
		c := decl.NamedChild(i)
		if c == nil || c.Type() != "type_parameters" {
			continue
		}
		for j, _nc := 0, int(c.NamedChildCount()); j < _nc; j++ {
			tp := c.NamedChild(j)
			if tp == nil || tp.Type() != "type_parameter" {
				continue
			}
			for k, _nc := 0, int(tp.NamedChildCount()); k < _nc; k++ {
				id := tp.NamedChild(k)
				if id != nil && id.Type() == "type_identifier" {
					out[strings.TrimSpace(id.Content(src))] = true
					break
				}
			}
		}
	}
	return out
}

// isSwiftPrimitive reports whether t names a Swift built-in scalar /
// standard-library primitive that should not produce a cross-file
// type-use edge — emitting these would only clutter the graph with
// unresolved::Int / unresolved::String targets that never land on a
// user-defined type node.
func isSwiftPrimitive(t string) bool {
	switch t {
	case "", "Int", "Int8", "Int16", "Int32", "Int64",
		"UInt", "UInt8", "UInt16", "UInt32", "UInt64",
		"Float", "Float16", "Float32", "Float64", "Double", "CGFloat",
		"Bool", "String", "Character", "Substring",
		"Void", "Never", "Any", "AnyObject", "AnyClass",
		"Self", "Optional", "true", "false":
		return true
	}
	return false
}
