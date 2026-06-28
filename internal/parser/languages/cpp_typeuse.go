package languages

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// C++ type-use edges. A C++ type named in a declaration position —
// local variable (`Foo x;`, `Foo* p;`, `std::shared_ptr<Foo> p;`),
// member/field, function parameter, or return type — is a cross-file
// reference to that type. The base extractor stamps return / parameter
// types into Meta for overload ranking but never materialises a graph
// edge, so `find_usages` on a type used only in declaration position
// lands nothing without a language server.
//
// collectCppTypeUseEdges runs one post-pass tree walk and emits an
// EdgeTypedAs from the owning symbol (enclosing function / method for
// locals, parameters and return types; the class / struct node for
// fields) to "unresolved::<Type>", at OriginASTInferred — the binding
// is a tree-sitter inference, not an LSP-checked fact. The cpp resolver
// later lands the unresolved target onto a real type node by name.
//
// Owner attribution reuses the extractor's funcRanges mechanism
// (line → enclosing function/method); fields are attributed by the
// caller, which already holds the class/struct node ID. Edges are
// de-duplicated per (owner, type) so a type used many times in one
// function contributes a single reference.

// emitCppTypeUseEdges canonicalises typeText to its bare named type
// (stripping const/volatile/&/*, unwrapping smart-pointer / container
// generics, dropping namespace qualifiers) and appends one EdgeTypedAs
// from ownerID to unresolved::<type>. Primitives and unparseable
// spellings are skipped. seen, when non-nil, de-duplicates per
// (owner, type) across a single Extract pass.
func emitCppTypeUseEdges(ownerID, typeText, filePath string, line int, result *parser.ExtractionResult, seen map[string]bool) {
	if ownerID == "" {
		return
	}
	t := canonicalizeCppTypeRef(typeText)
	if t == "" || isCppPrimitive(t) {
		return
	}
	if seen != nil {
		key := ownerID + "\x00" + t
		if seen[key] {
			return
		}
		seen[key] = true
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

// cppTypeWrappers are the smart-pointer / container generics whose sole
// type argument is the "real" type a declaration refers to. A
// `std::shared_ptr<Foo>` field is, for reference purposes, a use of
// Foo — so the canonicaliser unwraps one level (recursively) before
// reducing to the bare name. Multi-arg generics (map, pair) are left
// alone: there's no single inner type to pick, and the outer template
// name is itself a meaningful type reference.
var cppTypeWrappers = map[string]bool{
	"shared_ptr":        true,
	"unique_ptr":        true,
	"weak_ptr":          true,
	"auto_ptr":          true,
	"vector":            true,
	"list":              true,
	"deque":             true,
	"set":               true,
	"unordered_set":     true,
	"optional":          true,
	"reference_wrapper": true,
	"atomic":            true,
	"initializer_list":  true,
}

// canonicalizeCppTypeRef reduces a C++ type spelling to the bare named
// type the resolver can match against a type node:
//
//   - strip leading cv-qualifiers (const / volatile)
//   - strip trailing & / && / * indirection and cv-qualifiers
//   - unwrap a single-argument smart-pointer / container generic
//     (shared_ptr<Foo> → Foo, vector<Bar> → Bar) recursively
//   - for any other generic, drop the `<…>` template-argument tail and
//     keep the template name (Map<K,V> → Map)
//   - drop namespace / class qualifiers (ns::Foo → Foo, A::B::C → C)
//
// Returns "" when the result doesn't reduce to a plausible single
// identifier — a defensive guard against macro / template noise where
// the "type" text isn't a real type name. The caller additionally skips
// primitives via isCppPrimitive.
func canonicalizeCppTypeRef(raw string) string {
	t := strings.TrimSpace(raw)
	if t == "" {
		return ""
	}
	// Strip leading cv-qualifiers and storage keywords that can prefix a
	// declaration's type field text.
	for {
		trimmed := false
		for _, kw := range []string{"const ", "volatile ", "constexpr ", "static ", "mutable ", "typename ", "struct ", "class ", "enum "} {
			if strings.HasPrefix(t, kw) {
				t = strings.TrimSpace(t[len(kw):])
				trimmed = true
			}
		}
		if !trimmed {
			break
		}
	}
	// Strip trailing indirection / cv tokens (`*`, `&`, `&&`, ` const`).
	for {
		old := t
		t = strings.TrimSpace(t)
		t = strings.TrimSuffix(t, "&&")
		t = strings.TrimRight(t, " *&")
		t = strings.TrimSpace(t)
		t = strings.TrimSuffix(t, " const")
		t = strings.TrimSuffix(t, " volatile")
		if t == old {
			break
		}
	}
	t = strings.TrimSpace(t)
	if t == "" {
		return ""
	}
	// Unwrap / strip a template-argument tail.
	if open := strings.IndexByte(t, '<'); open >= 0 && strings.HasSuffix(t, ">") {
		head := strings.TrimSpace(t[:open])
		inner := strings.TrimSpace(t[open+1 : len(t)-1])
		base := lastCppSegment(head)
		if cppTypeWrappers[base] && inner != "" {
			// Single-argument wrapper: recurse on the inner type. For a
			// multi-argument inner spelling (`map<K,V>` reached via a
			// wrapper, unusual) keep only the first top-level argument.
			if first := firstTopLevelTemplateArg(inner); first != "" {
				inner = first
			}
			return canonicalizeCppTypeRef(inner)
		}
		// Non-wrapper generic: keep the template name itself.
		t = head
	}
	t = lastCppSegment(t)
	if !isPlausibleCppTypeName(t) {
		return ""
	}
	return t
}

// lastCppSegment drops namespace / nested-class qualifiers, keeping the
// final `::`-separated segment (ns::Foo → Foo, A::B::C → C).
func lastCppSegment(t string) string {
	t = strings.TrimSpace(t)
	if i := strings.LastIndex(t, "::"); i >= 0 {
		t = t[i+2:]
	}
	return strings.TrimSpace(t)
}

// firstTopLevelTemplateArg returns the first comma-separated argument of
// a template-argument list, respecting nested <…> so `map<int,Foo>`
// yields `map<int,Foo>` rather than `map<int`. Returns the whole string
// when there's no top-level comma.
func firstTopLevelTemplateArg(args string) string {
	depth := 0
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case '<':
			depth++
		case '>':
			if depth > 0 {
				depth--
			}
		case ',':
			if depth == 0 {
				return strings.TrimSpace(args[:i])
			}
		}
	}
	return strings.TrimSpace(args)
}

// isPlausibleCppTypeName guards against macro / template noise: a real
// type name reduces to a single identifier (letters, digits, underscore;
// not starting with a digit). Anything with leftover punctuation,
// whitespace, or template/operator residue is rejected so the graph
// isn't flooded with bogus "unresolved::" targets.
func isPlausibleCppTypeName(t string) bool {
	if t == "" {
		return false
	}
	for i := 0; i < len(t); i++ {
		c := t[i]
		switch {
		case c >= 'a' && c <= 'z':
		case c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9':
			if i == 0 {
				return false
			}
		case c == '_':
		default:
			return false
		}
	}
	return true
}

// isCppPrimitive reports whether t names a C++ builtin / stdlib alias
// that doesn't warrant an EdgeTypedAs target — emitting these would
// clutter the graph with unresolved::int / unresolved::string edges that
// never land on a user-defined type node.
func isCppPrimitive(t string) bool {
	switch t {
	case "", "void", "bool", "char", "char8_t", "char16_t", "char32_t",
		"wchar_t", "short", "int", "long", "float", "double", "signed",
		"unsigned", "auto", "decltype", "nullptr_t",
		"int8_t", "int16_t", "int32_t", "int64_t",
		"uint8_t", "uint16_t", "uint32_t", "uint64_t",
		"size_t", "ssize_t", "ptrdiff_t", "intptr_t", "uintptr_t",
		"string", "wstring", "string_view", "basic_string":
		return true
	}
	return false
}

// collectCppTypeUseEdges walks the parsed tree once and emits type-use
// edges for declaration positions the base extractor leaves edge-less:
//
//   - parameter_declaration  → enclosing function/method (param type)
//   - local declaration      → enclosing function/method (variable type)
//   - function_definition    → enclosing function/method (return type)
//
// Owner is the enclosing function/method, found by line against
// funcRanges. Positions with no enclosing function (e.g. a field, or a
// file-scope global) are skipped here — fields are emitted by the
// class/struct body walk, which holds the owning type's node ID.
func collectCppTypeUseEdges(root *sitter.Node, funcRanges []funcRange, filePath string, src []byte, result *parser.ExtractionResult) {
	if root == nil || len(funcRanges) == 0 {
		return
	}
	seen := make(map[string]bool)
	var walk func(n *sitter.Node)
	walk = func(n *sitter.Node) {
		if n == nil {
			return
		}
		switch n.Type() {
		case "parameter_declaration", "optional_parameter_declaration":
			emitCppDeclTypeUse(n, funcRanges, filePath, src, result, seen)
		case "declaration":
			// A `declaration` at namespace / file scope with a
			// function_declarator is a prototype, not a typed value — its
			// type is a return type covered by the function-node pass when
			// it has a body. Plain typed locals (and typed file globals,
			// which fall outside funcRanges and are skipped by the owner
			// lookup) flow through emitCppDeclTypeUse.
			emitCppDeclTypeUse(n, funcRanges, filePath, src, result, seen)
		case "function_definition":
			// Return type: the `type` field of the definition.
			line := int(n.StartPoint().Row) + 1
			owner := findEnclosingFunc(funcRanges, line)
			if owner != "" {
				if tn := n.ChildByFieldName("type"); tn != nil {
					emitCppTypeUseEdges(owner, tn.Content(src), filePath, line, result, seen)
				}
			}
		}
		for i, _nc := 0, int(n.NamedChildCount()); i < _nc; i++ {
			walk(n.NamedChild(i))
		}
	}
	walk(root)
}

// emitCppDeclTypeUse pulls the `type` field off a declaration /
// parameter node and attributes its canonicalised type to the enclosing
// function/method. Declarations outside any function (file-scope
// globals, prototypes) get owner "" and are skipped.
func emitCppDeclTypeUse(n *sitter.Node, funcRanges []funcRange, filePath string, src []byte, result *parser.ExtractionResult, seen map[string]bool) {
	tn := n.ChildByFieldName("type")
	if tn == nil {
		return
	}
	line := int(n.StartPoint().Row) + 1
	owner := findEnclosingFunc(funcRanges, line)
	if owner == "" {
		return
	}
	emitCppTypeUseEdges(owner, tn.Content(src), filePath, line, result, seen)
}
