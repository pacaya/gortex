package languages

import (
	"strconv"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// Rust reference-form edges.
//
// The base extractor (#143) already projects the *declaration* surface —
// `let x: T`, parameter, and return types — into EdgeTypedAs / EdgeReturns
// so a type used only in an annotation is visible to find_usages without
// a language server. It does NOT, however, cover the *usage* surface a
// Rust type also participates in:
//
//   - construction:  `Foo::new(...)`, `Foo { .. }`, `Variant(1, 2)`
//   - trait / inheritance: `impl Foo`, `impl Trait for Foo`, `T: Bound`,
//     `where T: Bound`, `dyn Trait`, `Box<dyn Trait>`, `trait X: Y`
//   - casts: `x as Foo`
//   - path / static access: `Foo::CONST`, `Foo::method()`, `Foo::Variant`
//   - attributes: `#[derive(Foo)]`
//
// emitRustReferenceForms runs one post-pass tree walk and materialises a
// graph edge for each, attributed to the enclosing function/method (file
// node fallback). Constructions emit graph.EdgeInstantiates; the trait,
// cast, and path-access forms emit graph.EdgeReferences carrying a
// `ref_context` Meta tag (inherit / cast / static_access) so consumers can
// tell the reference role apart.
//
// All reference edges are stamped Origin = graph.OriginASTResolved. This
// is load-bearing: the cross-package name-match guard
// (internal/resolver/cross_pkg_guard.go) reverts weak-tier EdgeReferences
// with bare `unresolved::X` targets, and these are structural projections
// of an explicit source spelling, not name-only call guesses. EdgeInstantiates
// is outside the guard's scope entirely.
//
// Scope rules (false-positive control):
//   - Only Capitalized leaf type names survive (lowercase = a value /
//     module / fn, never a type). Primitives (i32, String, Self, …) and
//     stdlib generic wrappers are dropped by isRustPrimitive +
//     canonicalizeRustTypeRef, which the helpers reuse.
//   - Path access only fires when the path resolves to a Capitalized
//     trailing segment: `std::io::Error` → Error, but a lowercase-only
//     path (`self::helper`, `crate::util::run`) emits nothing.
//   - The base extractor already owns let/param/return EdgeTypedAs, so
//     this pass never re-emits a declaration-position type.
//
// Edges are de-duplicated per (owner, type, line, ref_context) so a single
// source line contributes one edge per role.
func emitRustReferenceForms(root *sitter.Node, funcRanges []funcRange, fileID, filePath string, src []byte, result *parser.ExtractionResult) {
	if root == nil {
		return
	}
	seen := make(map[string]bool)

	// File-wide set of declared type-parameter names (`<T>`, `<K, V>` on
	// any fn / impl / struct / enum / trait). A name in this set is a
	// type-variable, not a real type, so a `type_arguments` member that
	// names one (`HashMap<K, Bar>` → K) must not become a type reference.
	// This is the precise signal Rust's grammar otherwise hides: a
	// type-param is spelled with the same `type_identifier` node as a real
	// type, so only the declaration site distinguishes them.
	typeParams := rustDeclaredTypeParams(root, src)

	owner := func(line int) string {
		if id := findEnclosingFunc(funcRanges, line); id != "" {
			return id
		}
		return fileID
	}

	// emitInstantiate appends an EdgeInstantiates from the enclosing owner
	// to the constructed type. typeName is the raw spelling; it is
	// canonicalized and primitive-gated before the edge is created.
	emitInstantiate := func(ownerID, typeName string, line int) {
		canon := canonicalizeRustTypeRef(typeName)
		if canon == "" || isRustPrimitive(canon) || !isRustCapitalized(canon) {
			return
		}
		key := ownerID + "\x00" + canon + "\x00" + strconv.Itoa(line) + "\x00inst"
		if seen[key] {
			return
		}
		seen[key] = true
		result.Edges = append(result.Edges, &graph.Edge{
			From:     ownerID,
			To:       "unresolved::" + canon,
			Kind:     graph.EdgeInstantiates,
			FilePath: filePath,
			Line:     line,
			Origin:   graph.OriginASTResolved,
		})
	}

	// emitRef appends an EdgeReferences carrying a ref_context role.
	emitRef := func(ownerID, typeName, useKind string, line int) {
		canon := canonicalizeRustTypeRef(typeName)
		if canon == "" || isRustPrimitive(canon) || !isRustCapitalized(canon) {
			return
		}
		key := ownerID + "\x00" + canon + "\x00" + strconv.Itoa(line) + "\x00" + useKind
		if seen[key] {
			return
		}
		seen[key] = true
		result.Edges = append(result.Edges, &graph.Edge{
			From:     ownerID,
			To:       "unresolved::" + canon,
			Kind:     graph.EdgeReferences,
			FilePath: filePath,
			Line:     line,
			Origin:   graph.OriginASTResolved,
			Meta:     map[string]any{"ref_context": useKind},
		})
	}

	walkRustNodes(root, func(n *sitter.Node) bool {
		line := int(n.StartPoint().Row) + 1
		switch n.Type() {

		case "struct_expression":
			// `Foo { field: .. }` — the type is the first child.
			if tn := n.NamedChild(0); tn != nil &&
				(tn.Type() == "type_identifier" || tn.Type() == "scoped_type_identifier" || tn.Type() == "generic_type") {
				emitInstantiate(owner(line), tn.Content(src), line)
			}

		case "call_expression":
			fn := n.ChildByFieldName("function")
			if fn == nil {
				break
			}
			switch fn.Type() {
			case "identifier":
				// `Variant(1, 2)` — a Capitalized bare callee is a
				// tuple-struct / enum-variant construction. Lowercase
				// (`foo()`) is an ordinary function call, owned by the
				// base extractor's call pass; skip it here.
				name := fn.Content(src)
				if isRustCapitalized(name) {
					emitInstantiate(owner(line), name, line)
				}
			case "scoped_identifier":
				// `Foo::new(...)` / `Foo::method(...)` — the receiver type
				// is the path before the final `::method`. A constructor
				// method (new / default / from / with_*) is a construction
				// view (EdgeInstantiates); any other associated function is
				// a static-access reference. The base extractor's call pass
				// already emits the EdgeCalls for the method itself.
				recv := rustScopedReceiverType(fn, src)
				if recv == "" {
					break
				}
				method := ""
				if nm := fn.ChildByFieldName("name"); nm != nil {
					method = nm.Content(src)
				}
				if isRustConstructorMethod(method) {
					emitInstantiate(owner(line), recv, line)
				} else {
					emitRef(owner(line), recv, "static_access", line)
				}
			}

		case "impl_item":
			// `impl Foo`, `impl Trait for Foo` — both the trait (if
			// present) and the implementing type are inheritance refs.
			if tr := n.ChildByFieldName("trait"); tr != nil {
				emitRef(owner(line), tr.Content(src), "inherit", line)
			}
			if ty := n.ChildByFieldName("type"); ty != nil {
				emitRef(owner(line), ty.Content(src), "inherit", line)
			}

		case "trait_bounds":
			// `: Bound + Other` on a type parameter, supertrait
			// (`trait X: Y`), or where-predicate. Each named type child is
			// a bound the bearer inherits.
			for i, _nc := 0, int(n.NamedChildCount()); i < _nc; i++ {
				c := n.NamedChild(i)
				if c == nil {
					continue
				}
				switch c.Type() {
				case "type_identifier", "scoped_type_identifier", "generic_type":
					emitRef(owner(line), c.Content(src), "inherit", line)
				}
			}

		case "dynamic_type":
			// `dyn Trait` (incl. inside `Box<dyn Trait>`). canonicalize
			// strips the `dyn `, leaving the trait name.
			emitRef(owner(line), n.Content(src), "inherit", line)

		case "type_cast_expression":
			// `x as Foo` — the `type` field is the cast target.
			if ty := n.ChildByFieldName("type"); ty != nil {
				emitRef(owner(line), ty.Content(src), "cast", line)
			}

		case "scoped_identifier", "scoped_type_identifier":
			// Path access: `Foo::CONST`, `Foo::Variant`, `std::io::Error`.
			// The construction (`Foo::new(...)`) and method-call cases are
			// handled by the call_expression branch above, which owns the
			// scoped_identifier in `function` position; descending here
			// would double-count, so skip a scoped node that is a call's
			// function child.
			if isRustCalleePath(n) {
				break
			}
			if name := rustPathAccessType(n, src); name != "" {
				emitRef(owner(line), name, "static_access", line)
			}

		case "attribute_item":
			// `#[derive(Foo, Bar)]` — each derive macro name is a static
			// reference to a trait/derive. Other attributes are ignored.
			emitRustDeriveRefs(n, owner(line), src, emitRef, line)

		case "type_arguments":
			// Generic argument list `<Foo>`, `<K, Bar>`, `<Foo, MyError>`,
			// `<dyn Trait>` — the element types the head wrapper carries.
			// The annotation / param / return passes only project the head
			// type of a `generic_type` (and canonicalize drops every arg of
			// a non-wrapper like HashMap, plus the error arm of Result), so
			// each arg type a generic mentions is otherwise lost. Emit a
			// `generic_arg` reference for every named type child here.
			//
			// Nesting is handled by the walker: `HashMap<K, Vec<Bar>>` has
			// an inner `Vec<Bar>` whose own `type_arguments` node this walk
			// visits separately, so for a `generic_type` arg we record only
			// its head spelling (Vec) and let that nested visit record Bar.
			// emitRef's canonicalize + primitive/capitalization gates drop
			// type-params (K), primitives (i32/str/String), lifetimes, and
			// lowercase names exactly as the other Rust reference forms do.
			emitArg := func(spelling string) {
				canon := canonicalizeRustTypeRef(spelling)
				if canon == "" || typeParams[canon] {
					return
				}
				emitRef(owner(line), canon, graph.RefContextGenericArg, line)
			}
			for i, _nc := 0, int(n.NamedChildCount()); i < _nc; i++ {
				c := n.NamedChild(i)
				if c == nil {
					continue
				}
				switch c.Type() {
				case "type_identifier", "scoped_type_identifier":
					emitArg(c.Content(src))
				case "generic_type":
					// Record the head only; the nested type_arguments visit
					// records the inner args. Strip the `<...>` tail so
					// canonicalize doesn't unwrap a wrapper head to its inner.
					head := c.Content(src)
					if idx := strings.IndexByte(head, '<'); idx > 0 {
						head = head[:idx]
					}
					emitArg(head)
				case "dynamic_type":
					// `<dyn Trait>` — the dynamic_type case of the outer
					// switch also fires for this node, but that is the
					// `inherit` role; record the generic_arg role too.
					emitArg(c.Content(src))
				}
			}
		}
		return true
	})
}

// rustDeclaredTypeParams returns the set of type-parameter names declared
// anywhere in the tree (`fn f<T>`, `impl<K, V>`, `struct S<E>`, …). A
// `type_arguments` member whose name is in this set is a type-variable
// reference, not a real type, and the generic_arg pass drops it so it
// never produces a spurious `unresolved::<param>` edge. The set is
// file-wide rather than per-scope: a `T` used as a generic arg in one fn
// is overwhelmingly the same `T` the enclosing item declares, and a real
// one-letter type that happens to collide with a param name elsewhere is
// vanishingly rare next to the false positives a per-letter heuristic
// would cause. Lifetimes carry their own node type and are ignored.
func rustDeclaredTypeParams(root *sitter.Node, src []byte) map[string]bool {
	params := map[string]bool{}
	walkRustNodes(root, func(n *sitter.Node) bool {
		if n.Type() != "type_parameters" {
			return true
		}
		for i, _nc := 0, int(n.NamedChildCount()); i < _nc; i++ {
			tp := n.NamedChild(i)
			if tp == nil {
				continue
			}
			switch tp.Type() {
			case "lifetime", "const_parameter":
				continue
			case "type_identifier":
				params[strings.TrimSpace(tp.Content(src))] = true
			default:
				// Constrained (`<T: Clone>`, `<T = u8>`) — the param name is
				// the first type_identifier child.
				for j, _nc := 0, int(tp.NamedChildCount()); j < _nc; j++ {
					c := tp.NamedChild(j)
					if c != nil && c.Type() == "type_identifier" {
						params[strings.TrimSpace(c.Content(src))] = true
						break
					}
				}
			}
		}
		return true
	})
	return params
}

// emitRustDeriveRefs pulls the derive-macro names out of a
// `#[derive(A, B)]` attribute_item and emits a static_access reference for
// each Capitalized name. Non-derive attributes contribute nothing.
func emitRustDeriveRefs(attrItem *sitter.Node, ownerID string, src []byte, emitRef func(ownerID, typeName, useKind string, line int), line int) {
	for i, _nc := 0, int(attrItem.NamedChildCount()); i < _nc; i++ {
		attr := attrItem.NamedChild(i)
		if attr == nil || attr.Type() != "attribute" {
			continue
		}
		// First named child is the attribute path identifier.
		head := attr.NamedChild(0)
		if head == nil || head.Content(src) != "derive" {
			continue
		}
		for j, _nc := 0, int(attr.NamedChildCount()); j < _nc; j++ {
			c := attr.NamedChild(j)
			if c == nil || c.Type() != "token_tree" {
				continue
			}
			for k, _nc := 0, int(c.NamedChildCount()); k < _nc; k++ {
				id := c.NamedChild(k)
				if id == nil {
					continue
				}
				switch id.Type() {
				case "identifier", "scoped_identifier", "type_identifier", "scoped_type_identifier":
					emitRef(ownerID, id.Content(src), "static_access", line)
				}
			}
		}
	}
}

// rustScopedReceiverType recovers the type a scoped call's path names —
// the segment before the final `::method`. For `Foo::new` the path is the
// bare `Foo`; for `std::io::Error::new` it is `std::io::Error` and the
// trailing Capitalized segment `Error` is the receiver type. Returns ""
// when the path holds no Capitalized type (e.g. `self::helper`,
// `crate::util::run`).
func rustScopedReceiverType(fn *sitter.Node, src []byte) string {
	path := fn.ChildByFieldName("path")
	if path == nil {
		return ""
	}
	switch path.Type() {
	case "scoped_identifier", "scoped_type_identifier":
		return rustPathAccessType(path, src)
	case "identifier", "type_identifier":
		seg := strings.TrimSpace(path.Content(src))
		if isRustCapitalized(seg) {
			return seg
		}
	}
	return ""
}

// isRustConstructorMethod reports whether an associated-function name is a
// conventional constructor — the call then reads as a construction of the
// receiver type rather than a plain static access. The set is the common
// idiomatic constructors; everything else (accessor / builder-terminal /
// free associated fn) stays a static_access reference.
func isRustConstructorMethod(name string) bool {
	switch name {
	case "new", "default", "from", "with_capacity", "from_str",
		"from_iter", "builder", "create", "init", "open", "connect":
		return true
	}
	return strings.HasPrefix(name, "with_") || strings.HasPrefix(name, "from_")
}

// rustScopedHead returns the leading segment of a scoped_identifier /
// scoped_type_identifier as a bare string — `Foo::new` → "Foo",
// `std::io::Error` → "std" (the outermost head). Empty when the head is
// not a simple identifier (e.g. a `<T as Trait>` qualified path).
func rustScopedHead(n *sitter.Node, src []byte) string {
	cur := n
	for cur != nil && (cur.Type() == "scoped_identifier" || cur.Type() == "scoped_type_identifier") {
		path := cur.ChildByFieldName("path")
		if path == nil {
			// No `path` field → the head identifier is the first child.
			if first := cur.NamedChild(0); first != nil &&
				(first.Type() == "identifier" || first.Type() == "type_identifier") {
				return strings.TrimSpace(first.Content(src))
			}
			return ""
		}
		cur = path
	}
	if cur != nil && (cur.Type() == "identifier" || cur.Type() == "type_identifier") {
		return strings.TrimSpace(cur.Content(src))
	}
	return ""
}

// rustPathAccessType decides the referenced type of a bare path-access
// node (`Foo::CONST`, `std::io::Error`, `self::helper`). The rule:
//
//   - if the head segment is a bare Capitalized identifier, the path
//     itself names a member of that type → the head IS the type
//     (`Foo::CONST` → Foo, `Color::Red` → Color);
//   - otherwise the path is module-qualified (lowercase head: std::, crate::,
//     self::, a module). Keep the trailing Capitalized type segment if one
//     exists (`std::io::Error` → Error) and drop the path otherwise
//     (`self::helper`, `crate::util::run` → nothing).
//
// Returns "" when no Capitalized type can be recovered.
func rustPathAccessType(n *sitter.Node, src []byte) string {
	head := rustScopedHead(n, src)
	if head != "" && isRustCapitalized(head) {
		return head
	}
	// Module-qualified path: recover the trailing Capitalized segment.
	full := strings.TrimSpace(n.Content(src))
	segs := strings.Split(full, "::")
	for i := len(segs) - 1; i >= 0; i-- {
		seg := strings.TrimSpace(segs[i])
		// Strip a generic tail on the segment (Error<T> → Error).
		if idx := strings.IndexByte(seg, '<'); idx > 0 {
			seg = seg[:idx]
		}
		if isRustCapitalized(seg) {
			return seg
		}
	}
	return ""
}

// isRustCalleePath reports whether n is part of an enclosing
// call_expression's callee — either the `function` child directly, or a
// `path` segment of it (`std::io::Error` inside `std::io::Error::new()`).
// Such nodes are already represented by the construction / call branch,
// so the standalone path-access walk must skip them to avoid emitting a
// duplicate static_access alongside the construction view.
func isRustCalleePath(n *sitter.Node) bool {
	// Climb the scoped_identifier `path` chain to its outermost scoped
	// parent, then ask whether that node is a call's function child.
	cur := n
	for {
		p := cur.Parent()
		if p == nil {
			return false
		}
		switch p.Type() {
		case "scoped_identifier", "scoped_type_identifier":
			// Only follow the path-spine upward (n is the `path` of p),
			// not the `name` side.
			if pp := p.ChildByFieldName("path"); pp != nil && pp.Equal(cur) {
				cur = p
				continue
			}
			return false
		case "call_expression":
			fn := p.ChildByFieldName("function")
			return fn != nil && fn.Equal(cur)
		default:
			return false
		}
	}
}

// isRustCapitalized reports whether name's first rune is an ASCII
// uppercase letter — the syntactic marker that distinguishes a Rust type
// / variant / trait (UpperCamelCase) from a value, module, or function
// (snake_case). A leading `_` or digit is not a type.
func isRustCapitalized(name string) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return false
	}
	c := name[0]
	return c >= 'A' && c <= 'Z'
}
