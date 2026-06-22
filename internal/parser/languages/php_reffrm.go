package languages

import (
	"strconv"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// emitPHPReferenceForms emits the PHP reference edges that the symbol /
// type-use walk (EdgeTypedAs on typed properties, parameters, and return
// types) does not cover — the *expression-site* and *inheritance-clause*
// uses of a class / interface name:
//
//   - INSTANTIATION   `new Foo(...)` (object_creation_expression) whose
//     type is a Capitalized name / qualified_name → EdgeInstantiates
//   - INHERITANCE     `class X extends Base` (base_clause) /
//     `class X implements I, J` (class_interface_clause)
//     → EdgeReferences, Meta["ref_context"]="inherit"
//   - TYPE-TEST       `$x instanceof Foo` (binary_expression with the
//     `instanceof` operator) → EdgeReferences, Meta["ref_context"]="cast"
//   - STATIC / CONST  `Foo::CONST` / `Foo::class`
//     (class_constant_access_expression), `Foo::method()`
//     (scoped_call_expression) whose scope is a Capitalized name
//     → EdgeReferences, Meta["ref_context"]="static_access"
//   - ATTRIBUTE       `#[Foo]` / `#[Foo(...)]` (attribute, PHP 8) names the
//     attribute type Foo → EdgeReferences, Meta["ref_context"]="static_access"
//
// The inheritance forms are emitted here as EdgeReferences (ref_context
// "inherit") rather than EdgeExtends / EdgeImplements: #143's
// extractPhpTraitUse already owns trait composition (EdgeExtends via:trait),
// and scope_parent on the class node carries the single base class for the
// scope resolver. These ref edges give find_usages a "used as a base /
// conformed interface" surface without changing the inheritance modelling.
//
// Each expression-site edge attributes to the enclosing function / method
// (file node when nothing encloses the line); each inheritance edge
// attributes to the declaring class / interface node, so find_usages lands
// the reference without a language server. Targets are left at the canonical
// bare-name placeholder `unresolved::Foo` for the resolver to bind.
//
// Origin is load-bearing. Every edge rides graph.OriginASTResolved: the
// cross-package guard (internal/resolver/cross_pkg_guard.go) reverts
// EdgeReferences / EdgeCalls edges to their unresolved placeholder *only* at
// the two weakest tiers (text_matched / ast_inferred), so an ast_resolved
// edge is out of its scope and survives. EdgeInstantiates is not a call-like
// edge the guard polices at all, but it carries the same origin for
// consistency.
//
// Scope discipline keeps the graph clean: only Capitalized leaf type names
// survive (lowercase locals / variables and PHP builtins are skipped via the
// Capitalization gate + phpBuiltinType), the leading namespace separator and
// any namespace qualification are reduced to the bare class name by
// canonicalizePHPTypeRef (`\App\Foo` → `Foo`), and the relative scopes
// `self` / `static` / `parent` are excluded — a `self::x` scoped call uses a
// relative_scope node (not a name), and the explicit name guard backstops it.
func emitPHPReferenceForms(root *sitter.Node, src []byte, filePath, fileID string, result *parser.ExtractionResult) {
	if root == nil {
		return
	}
	funcRanges := buildFuncRanges(result)
	typeRanges := buildPHPTypeRanges(result)

	// Dedup across the whole file on (owner, type, line, ref_context) so a
	// type referenced twice in the same role on the same line emits one edge.
	seen := map[string]bool{}

	ownerFor := func(line int) string {
		if owner := findEnclosingFunc(funcRanges, line); owner != "" {
			return owner
		}
		// Fall back to the enclosing class (a class-level attribute /
		// inheritance clause is not inside any method) then the file node.
		if owner := findEnclosingFunc(typeRanges, line); owner != "" {
			return owner
		}
		return fileID
	}

	// emit appends one reference-form edge for a single Capitalized,
	// non-builtin, non-relative type name. owner "" means "derive from the
	// enclosing function / class / file at this line".
	emit := func(rawType string, line int, owner string, kind graph.EdgeKind, refContext string) {
		canon := canonicalizePHPTypeRef(rawType)
		if canon == "" || phpBuiltinType(canon) || !isPHPTypeNameCapitalized(canon) || isPHPRelativeScope(canon) {
			return
		}
		if owner == "" {
			owner = ownerFor(line)
		}
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

		case "object_creation_expression":
			// `new Foo(...)` / `new \App\Http\Client()` — the constructed type
			// is the leading type-shaped named child. `new $klass()` (a
			// variable) and `new class {}` (anonymous) have no name child.
			if name := phpCreationTypeName(n, src); name != "" {
				emit(name, line, "", graph.EdgeInstantiates, "")
			}

		case "base_clause":
			// `class X extends Base` — emit one inherit reference per base
			// name (PHP single inheritance for classes, but interfaces may
			// extend several). Attributed to the enclosing class node.
			owner := ownerFor(line)
			for _, name := range phpClauseTypeNames(n, src) {
				emit(name, int(n.StartPoint().Row)+1, owner, graph.EdgeReferences, graph.RefContextInherit)
			}

		case "class_interface_clause":
			// `class X implements I, J` / `interface X extends A, B` — one
			// inherit reference per named interface.
			owner := ownerFor(line)
			for _, name := range phpClauseTypeNames(n, src) {
				emit(name, int(n.StartPoint().Row)+1, owner, graph.EdgeReferences, graph.RefContextInherit)
			}

		case "binary_expression":
			// `$x instanceof Foo` — only the instanceof operator names a type.
			if op := n.ChildByFieldName("operator"); op != nil && op.Type() == "instanceof" {
				if name := phpInstanceofTypeName(n, src); name != "" {
					emit(name, line, "", graph.EdgeReferences, graph.RefContextCast)
				}
			}

		case "class_constant_access_expression":
			// `Foo::CONST` / `Foo::class` — the scope (first named child) is
			// the referenced type when it is a bare Capitalized name.
			if name := phpScopeTypeName(n, src); name != "" {
				emit(name, line, "", graph.EdgeReferences, graph.RefContextStaticAccess)
			}

		case "scoped_call_expression":
			// `Foo::method()` — same scope rule. `self::` / `parent::` /
			// `static::` use a relative_scope node, so phpScopeTypeName
			// returns "" for them.
			if name := phpScopeTypeName(n, src); name != "" {
				emit(name, line, "", graph.EdgeReferences, graph.RefContextStaticAccess)
			}

		case "attribute":
			// `#[Foo]` / `#[Foo(...)]` names the attribute type Foo.
			if name := phpAttributeRefName(n, src); name != "" {
				emit(name, line, "", graph.EdgeReferences, graph.RefContextStaticAccess)
			}
		}
	})
}

// buildPHPTypeRanges returns the line ranges of every class / interface /
// type node in the extraction result, so an inheritance clause or
// class-level attribute (not inside any method) is attributed to its
// declaring type rather than the file.
func buildPHPTypeRanges(result *parser.ExtractionResult) []funcRange {
	var ranges []funcRange
	for _, n := range result.Nodes {
		if n.Kind == graph.KindType || n.Kind == graph.KindInterface {
			ranges = append(ranges, funcRange{id: n.ID, startLine: n.StartLine, endLine: n.EndLine})
		}
	}
	return ranges
}

// isPHPTypeNameCapitalized reports whether a canonicalized type name begins
// with an uppercase ASCII letter — the Capitalization gate that keeps
// lowercase locals / variables / keywords out of the reference surface.
// canonicalizePHPTypeRef has already stripped the nullable marker and
// namespace qualification, so the first rune of the bare name discriminates.
func isPHPTypeNameCapitalized(name string) bool {
	if name == "" {
		return false
	}
	c := name[0]
	return c >= 'A' && c <= 'Z'
}

// isPHPRelativeScope reports whether a name is one of PHP's relative class
// scopes — these never name a concrete type to reference. They are normally
// caught earlier (relative_scope is a distinct grammar node, not a name),
// but the guard backstops the Capitalized-name path defensively.
func isPHPRelativeScope(name string) bool {
	switch strings.ToLower(name) {
	case "self", "static", "parent":
		return true
	}
	return false
}

// phpCreationTypeName returns the constructed type's spelling from an
// object_creation_expression (`new Foo()` → "Foo", `new \App\Client()` →
// "\App\Client"). The type is the leading type-shaped named child; `new
// $klass()` (dynamic) and `new class {}` (anonymous) have none and yield "".
func phpCreationTypeName(n *sitter.Node, src []byte) string {
	for i := 0; i < int(n.NamedChildCount()); i++ {
		c := n.NamedChild(i)
		if c == nil {
			continue
		}
		switch c.Type() {
		case "name", "qualified_name":
			return strings.TrimSpace(c.Content(src))
		case "arguments":
			// Reached the call arguments without a type name (dynamic /
			// anonymous creation) — nothing to reference.
			return ""
		}
	}
	return ""
}

// phpClauseTypeNames returns every class / interface name listed in a
// base_clause (`extends Base`) or class_interface_clause (`implements I, J`).
func phpClauseTypeNames(clause *sitter.Node, src []byte) []string {
	var out []string
	for i := 0; i < int(clause.NamedChildCount()); i++ {
		c := clause.NamedChild(i)
		if c == nil {
			continue
		}
		switch c.Type() {
		case "name", "qualified_name":
			if t := strings.TrimSpace(c.Content(src)); t != "" {
				out = append(out, t)
			}
		}
	}
	return out
}

// phpInstanceofTypeName returns the right-operand type of an instanceof
// binary_expression (`$x instanceof Foo` → "Foo"). The grammar tags the type
// as the `right` field. A dynamic test (`$x instanceof $klass`) yields a
// variable_name on the right and is skipped by the type-shape check.
func phpInstanceofTypeName(n *sitter.Node, src []byte) string {
	right := n.ChildByFieldName("right")
	if right == nil {
		return ""
	}
	switch right.Type() {
	case "name", "qualified_name":
		return strings.TrimSpace(right.Content(src))
	}
	return ""
}

// phpScopeTypeName returns the scope type name of a
// class_constant_access_expression (`Foo::CONST`, `Foo::class`) or a
// scoped_call_expression (`Foo::method()`) — the first named child when it is
// a bare class name. `self::` / `static::` / `parent::` use a relative_scope
// node and `$obj::method()` a variable_name, both of which return "".
func phpScopeTypeName(n *sitter.Node, src []byte) string {
	scope := n.ChildByFieldName("scope")
	if scope == nil && n.NamedChildCount() > 0 {
		scope = n.NamedChild(0)
	}
	if scope == nil {
		return ""
	}
	switch scope.Type() {
	case "name", "qualified_name":
		return strings.TrimSpace(scope.Content(src))
	}
	return ""
}

// phpAttributeRefName returns the bare attribute type name from an `attribute`
// node (`#[Route("/x")]` → "Route"). The name is the `name` field, or the
// first name / qualified_name child.
func phpAttributeRefName(n *sitter.Node, src []byte) string {
	if nm := n.ChildByFieldName("name"); nm != nil {
		return strings.TrimSpace(nm.Content(src))
	}
	for i := 0; i < int(n.NamedChildCount()); i++ {
		c := n.NamedChild(i)
		if c == nil {
			continue
		}
		switch c.Type() {
		case "name", "qualified_name":
			return strings.TrimSpace(c.Content(src))
		}
	}
	return ""
}
