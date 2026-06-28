package languages

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// C++ reference-form edges. Beyond declaration-position type uses (which
// collectCppTypeUseEdges already materialises as EdgeTypedAs), a C++ type
// name appears in several other positions that are genuine cross-file
// references but leave no graph edge without a language server:
//
//   - construction — `new Foo(...)` (new_expression) or stack
//     `Foo x(...)` / `Foo x{...}` (a declaration whose initializer is a
//     constructor call) → graph.EdgeInstantiates.
//   - inheritance — `class X : public Base, private Mixin`
//     (base_class_clause) → graph.EdgeReferences, ref_context=inherit.
//   - casts — `static_cast<Foo>(x)` / `dynamic_cast<Foo>(x)` /
//     `reinterpret_cast<Foo>(x)` / `const_cast<Foo>(x)` (a call through a
//     template_function) and C-style `(Foo)x` (cast_expression) →
//     EdgeReferences, ref_context=cast.
//   - scope / static access — `Foo::BAR`, `Foo::method()`
//     (qualified_identifier whose scope is a Capitalized namespace / type)
//     → EdgeReferences, ref_context=static_access.
//   - generic / template arguments — every named type inside a
//     `template_argument_list` (`std::vector<Foo>`, `std::map<K, Bar>`,
//     `Foo<Bar>`, nested `std::map<std::string, std::vector<Widget>>`) →
//     EdgeReferences, ref_context=generic_arg. The declaration-position
//     canonicaliser keeps only the template head (or unwraps a single
//     container arg), so the element types of multi-argument / user
//     generics were otherwise dropped.
//
// emitCppReferenceForms runs one post-pass tree walk (mirroring
// collectCppTypeUseEdges) and emits these edges, attributed to the
// enclosing function/method (construction, cast, static access) or to the
// enclosing class/struct node (inheritance). Targets are bare
// `unresolved::<Type>`; the cpp resolver lands them on a real type node.
//
// LOAD-BEARING: structural EdgeReferences edges carrying a bare
// `unresolved::Name` target are reverted by the cross-package guard
// (internal/resolver/cross_pkg_guard.go) at the weak OriginASTInferred /
// OriginTextMatched tiers when the target isn't import-reachable. These
// edges are AST-grounded facts (the syntax says "X inherits Base"), so
// they are stamped graph.OriginASTResolved, which the guard skips. The
// EdgeInstantiates edges aren't policed by the guard.
//
// A strict Capitalization gate keeps the noise out: only leaf type names
// that begin with an uppercase letter are emitted, and `std::`-qualified
// paths reduce to their trailing Capitalized type (so `std::vector<int>`
// and `std::move` contribute nothing, while `std::String` would reduce to
// String). Primitives and unparseable spellings are dropped via the same
// canonicalizeCppTypeRef + isCppPrimitive helpers the type-use pass uses.

// cppCastFunctions are the named C++ cast operators whose sole template
// argument is the cast-target type. A `call_expression` through a
// `template_function` named one of these is a cast, not an ordinary call.
var cppCastFunctions = map[string]bool{
	"static_cast":      true,
	"dynamic_cast":     true,
	"reinterpret_cast": true,
	"const_cast":       true,
}

// emitCppReferenceForms walks the parsed tree once and emits the
// construction / inheritance / cast / static-access reference edges
// described above. funcRanges attributes body-level forms to their
// enclosing function/method; inheritance is attributed to the class node
// by name. Deduplicated per (owner, type, line, ref_context) so a form
// repeated on one line contributes a single edge.
func emitCppReferenceForms(root *sitter.Node, src []byte, filePath, fileID string, funcRanges []funcRange, result *parser.ExtractionResult) {
	if root == nil {
		return
	}
	seen := make(map[string]bool)
	var walk func(n *sitter.Node)
	walk = func(n *sitter.Node) {
		if n == nil {
			return
		}
		switch n.Type() {
		case "base_class_clause":
			emitCppInheritance(n, src, filePath, result, seen)
		case "new_expression":
			emitCppConstruction(n, src, filePath, funcRanges, result, seen)
		case "declaration":
			emitCppStackConstruction(n, src, filePath, funcRanges, result, seen)
		case "cast_expression":
			emitCppCCast(n, src, filePath, funcRanges, result, seen)
		case "call_expression":
			emitCppNamedCast(n, src, filePath, funcRanges, result, seen)
			emitCppQualifiedCall(n, src, filePath, funcRanges, result, seen)
		case "qualified_identifier":
			emitCppStaticAccess(n, src, filePath, funcRanges, result, seen)
		case "template_argument_list":
			emitCppGenericArgs(n, src, filePath, funcRanges, result, seen)
		}
		for i, _nc := 0, int(n.NamedChildCount()); i < _nc; i++ {
			walk(n.NamedChild(i))
		}
	}
	walk(root)
}

// emitCppReferenceEdge appends one EdgeReferences from ownerID to
// unresolved::<type> at OriginASTResolved, stamping ref_context. The type
// text is canonicalised + Capitalization-gated; primitives, lowercase
// leaves, and unparseable spellings are skipped. Deduplicated per
// (owner, type, line, ref_context).
func emitCppReferenceEdge(ownerID, typeText, refContext, filePath string, line int, result *parser.ExtractionResult, seen map[string]bool) {
	if ownerID == "" {
		return
	}
	t := canonicalizeCppTypeRef(typeText)
	if t == "" || isCppPrimitive(t) || !isCapitalizedCppType(t) {
		return
	}
	key := ownerID + "\x00" + t + "\x00" + refContext + "\x00" + lineKey(line)
	if seen[key] {
		return
	}
	seen[key] = true
	result.Edges = append(result.Edges, &graph.Edge{
		From:     ownerID,
		To:       "unresolved::" + t,
		Kind:     graph.EdgeReferences,
		FilePath: filePath,
		Line:     line,
		Origin:   graph.OriginASTResolved,
		Meta:     map[string]any{"ref_context": refContext},
	})
}

// emitCppInstantiateEdge appends one EdgeInstantiates from ownerID to
// unresolved::<type>. EdgeInstantiates is not policed by the cross-package
// guard, so it carries the OriginASTInferred tier (an AST inference, not
// an LSP-checked fact) like the type-use pass. Deduplicated per
// (owner, type, line, instantiate).
func emitCppInstantiateEdge(ownerID, typeText, filePath string, line int, result *parser.ExtractionResult, seen map[string]bool) {
	if ownerID == "" {
		return
	}
	t := canonicalizeCppTypeRef(typeText)
	if t == "" || isCppPrimitive(t) || !isCapitalizedCppType(t) {
		return
	}
	key := ownerID + "\x00" + t + "\x00instantiate\x00" + lineKey(line)
	if seen[key] {
		return
	}
	seen[key] = true
	result.Edges = append(result.Edges, &graph.Edge{
		From:     ownerID,
		To:       "unresolved::" + t,
		Kind:     graph.EdgeInstantiates,
		FilePath: filePath,
		Line:     line,
		Origin:   graph.OriginASTInferred,
	})
}

// emitCppInheritance handles a base_class_clause: each base type
// (`type_identifier`, `template_type`, or `qualified_identifier` child,
// skipping the leading access_specifier tokens) is an inherit-context
// reference from the enclosing class/struct node.
func emitCppInheritance(clause *sitter.Node, src []byte, filePath string, result *parser.ExtractionResult, seen map[string]bool) {
	owner := cppEnclosingTypeID(clause, src, filePath)
	if owner == "" {
		return
	}
	line := int(clause.StartPoint().Row) + 1
	for i, _nc := 0, int(clause.NamedChildCount()); i < _nc; i++ {
		ch := clause.NamedChild(i)
		switch ch.Type() {
		case "type_identifier", "template_type", "qualified_identifier", "dependent_type":
			emitCppReferenceEdge(owner, ch.Content(src), graph.RefContextInherit, filePath, line, result, seen)
		}
	}
}

// cppEnclosingTypeID returns the node ID of the class/struct that owns a
// base_class_clause — its parent class_specifier/struct_specifier's name
// field, formed as filePath::Name to match emitClass / emitStruct.
func cppEnclosingTypeID(clause *sitter.Node, src []byte, filePath string) string {
	parent := clause.Parent()
	if parent == nil {
		return ""
	}
	switch parent.Type() {
	case "class_specifier", "struct_specifier":
		if name := parent.ChildByFieldName("name"); name != nil {
			return filePath + "::" + strings.TrimSpace(name.Content(src))
		}
	}
	return ""
}

// emitCppConstruction handles `new Foo(...)`: the new_expression's `type`
// field names the constructed type. Attributed to the enclosing
// function/method via funcRanges.
func emitCppConstruction(n *sitter.Node, src []byte, filePath string, funcRanges []funcRange, result *parser.ExtractionResult, seen map[string]bool) {
	tn := n.ChildByFieldName("type")
	if tn == nil {
		return
	}
	line := int(n.StartPoint().Row) + 1
	owner := findEnclosingFunc(funcRanges, line)
	emitCppInstantiateEdge(owner, tn.Content(src), filePath, line, result, seen)
}

// emitCppStackConstruction handles stack construction
// `Foo x(...)` / `Foo x{...}`: a declaration whose `type` is a named type
// and whose init_declarator initialiser is a constructor call
// (argument_list) or brace-init (initializer_list). A plain `Foo x;` (no
// initialiser) or `int x = 5;` is left to the type-use pass / skipped.
func emitCppStackConstruction(n *sitter.Node, src []byte, filePath string, funcRanges []funcRange, result *parser.ExtractionResult, seen map[string]bool) {
	tn := n.ChildByFieldName("type")
	if tn == nil {
		return
	}
	if !cppDeclHasCtorInit(n) {
		return
	}
	line := int(n.StartPoint().Row) + 1
	owner := findEnclosingFunc(funcRanges, line)
	emitCppInstantiateEdge(owner, tn.Content(src), filePath, line, result, seen)
}

// cppDeclHasCtorInit reports whether a declaration has at least one
// init_declarator initialised by a constructor argument list or brace
// initializer — the marker that distinguishes a stack construction
// (`Foo x(1)`, `Foo x{1}`) from a plain typed local (`Foo x;`, `int x = 5`).
func cppDeclHasCtorInit(decl *sitter.Node) bool {
	for i, _nc := 0, int(decl.NamedChildCount()); i < _nc; i++ {
		ch := decl.NamedChild(i)
		if ch.Type() != "init_declarator" {
			continue
		}
		if v := ch.ChildByFieldName("value"); v != nil {
			switch v.Type() {
			case "argument_list", "initializer_list":
				return true
			}
		}
	}
	return false
}

// emitCppCCast handles a C-style cast `(Foo)x`: the cast_expression's
// `type` field is a type_descriptor naming the cast target.
func emitCppCCast(n *sitter.Node, src []byte, filePath string, funcRanges []funcRange, result *parser.ExtractionResult, seen map[string]bool) {
	tn := n.ChildByFieldName("type")
	if tn == nil {
		return
	}
	line := int(n.StartPoint().Row) + 1
	owner := findEnclosingFunc(funcRanges, line)
	emitCppReferenceEdge(owner, tn.Content(src), graph.RefContextCast, filePath, line, result, seen)
}

// emitCppNamedCast handles `static_cast<Foo>(x)` and the other named cast
// operators: a call_expression whose function is a template_function named
// one of cppCastFunctions, whose single template argument is the cast
// target type.
func emitCppNamedCast(n *sitter.Node, src []byte, filePath string, funcRanges []funcRange, result *parser.ExtractionResult, seen map[string]bool) {
	fn := n.ChildByFieldName("function")
	if fn == nil || fn.Type() != "template_function" {
		return
	}
	name := fn.ChildByFieldName("name")
	if name == nil || !cppCastFunctions[strings.TrimSpace(name.Content(src))] {
		return
	}
	args := fn.ChildByFieldName("arguments")
	if args == nil {
		return
	}
	line := int(n.StartPoint().Row) + 1
	owner := findEnclosingFunc(funcRanges, line)
	// The first template argument is the cast target.
	for i, _nc := 0, int(args.NamedChildCount()); i < _nc; i++ {
		ch := args.NamedChild(i)
		if ch.Type() == "type_descriptor" {
			emitCppReferenceEdge(owner, ch.Content(src), graph.RefContextCast, filePath, line, result, seen)
			return
		}
	}
}

// emitCppQualifiedCall handles `Thing::method()`: a call_expression whose
// function is a qualified_identifier with a Capitalized scope — the scope
// type is a static-access reference. The trailing member name is left to
// the call resolver.
func emitCppQualifiedCall(n *sitter.Node, src []byte, filePath string, funcRanges []funcRange, result *parser.ExtractionResult, seen map[string]bool) {
	fn := n.ChildByFieldName("function")
	if fn == nil || fn.Type() != "qualified_identifier" {
		return
	}
	scope := cppQualifiedScopeType(fn, src)
	if scope == "" {
		return
	}
	line := int(n.StartPoint().Row) + 1
	owner := findEnclosingFunc(funcRanges, line)
	emitCppReferenceEdge(owner, scope, graph.RefContextStaticAccess, filePath, line, result, seen)
}

// emitCppStaticAccess handles a bare `Foo::BAR` qualified_identifier (a
// static-member / scoped-constant read) with a Capitalized scope. Skipped
// when the qualified_identifier is the function of a call_expression
// (handled by emitCppQualifiedCall) so a `Thing::method()` scope isn't
// double-emitted.
func emitCppStaticAccess(n *sitter.Node, src []byte, filePath string, funcRanges []funcRange, result *parser.ExtractionResult, seen map[string]bool) {
	if p := n.Parent(); p != nil && p.Type() == "call_expression" {
		if fn := p.ChildByFieldName("function"); fn != nil && fn.Equal(n) {
			return
		}
	}
	scope := cppQualifiedScopeType(n, src)
	if scope == "" {
		return
	}
	line := int(n.StartPoint().Row) + 1
	owner := findEnclosingFunc(funcRanges, line)
	emitCppReferenceEdge(owner, scope, graph.RefContextStaticAccess, filePath, line, result, seen)
}

// emitCppGenericArgs handles a `template_argument_list` (`<Foo>`,
// `<std::string, Bar>`, `<int, 5>`): every `type_descriptor` child names a
// type argument, which is a genuine reference to that type. Each is
// attributed to the enclosing function/method via funcRanges (a top-level
// generic outside any function — e.g. a global `std::vector<Foo> g;` — has
// no owner and is skipped, matching the type-use pass). Non-type arguments
// (integer constants, which the grammar nests as `number_literal` rather
// than `type_descriptor`) are not iterated, and primitives / lowercase
// leaves are dropped by emitCppReferenceEdge's canonicaliser +
// Capitalization gate, so `<int, 5>` and `<std::string>` contribute
// nothing. Nesting is handled by the enclosing tree walk, which reaches the
// inner `template_argument_list` of `std::map<K, std::vector<Widget>>` as
// its own node.
func emitCppGenericArgs(n *sitter.Node, src []byte, filePath string, funcRanges []funcRange, result *parser.ExtractionResult, seen map[string]bool) {
	line := int(n.StartPoint().Row) + 1
	owner := findEnclosingFunc(funcRanges, line)
	if owner == "" {
		return
	}
	for i, _nc := 0, int(n.NamedChildCount()); i < _nc; i++ {
		ch := n.NamedChild(i)
		if ch == nil || ch.Type() != "type_descriptor" {
			continue
		}
		emitCppReferenceEdge(owner, ch.Content(src), graph.RefContextGenericArg, filePath, line, result, seen)
	}
}

// cppQualifiedScopeType returns the Capitalized type that names the scope
// of a qualified_identifier, or "" when the scope isn't a plausible type
// reference. The scope is the qualified_identifier's `scope` field (a
// namespace_identifier or nested qualified_identifier). `std::`-only and
// other lowercase scopes reduce to "" via the canonicalizer's
// Capitalization gate; a `std::Foo` style scope reduces to its trailing
// Capitalized segment.
func cppQualifiedScopeType(qid *sitter.Node, src []byte) string {
	scope := qid.ChildByFieldName("scope")
	if scope == nil {
		return ""
	}
	t := canonicalizeCppTypeRef(scope.Content(src))
	if t == "" || isCppPrimitive(t) || !isCapitalizedCppType(t) {
		return ""
	}
	return t
}

// isCapitalizedCppType reports whether a (already-canonicalised) type name
// begins with an uppercase ASCII letter — the heuristic that separates a
// user type (`Foo`, `Widget`) from a namespace / lowercase identifier
// (`std`, `detail`, `move`). C++ has no enforced convention, but the
// overwhelming majority of user types are PascalCase, and the gate keeps
// the structural reference edges from flooding the graph with namespace /
// free-function noise.
func isCapitalizedCppType(t string) bool {
	if t == "" {
		return false
	}
	c := t[0]
	return c >= 'A' && c <= 'Z'
}

// lineKey renders a line number for the dedup key without an fmt import.
func lineKey(line int) string {
	if line == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	neg := line < 0
	if neg {
		line = -line
	}
	for line > 0 {
		i--
		b[i] = byte('0' + line%10)
		line /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
