package languages

import (
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// emitKotlinReferenceForms captures the Kotlin reference forms that the
// type-annotation / parameter / return-type passes don't cover, so a
// find_usages of a type surfaces every place the type is mentioned — not
// just the declarations that name it in a type position.
//
// Four forms, each emitted as an edge to `unresolved::<TypeName>` carrying
// Meta["ref_context"], attributed to the enclosing function (or the file node
// for top-level expressions):
//
//   - instantiate    `OkHttpClient()` / `OkHttpClient.Builder()` — Kotlin has
//     no `new`, so a call whose callee is a Capitalized simple_identifier (or a
//     navigation_expression ending in a Capitalized name) is a constructor call.
//     EdgeInstantiates lands on the type via resolveTypeOrFunc.
//   - cast           `x as T` / `x as? T` (as_expression) and `x is T` / `x !is T`
//     (check_expression). EdgeReferences to the named type.
//   - static_access  `Type.Member` (navigation_expression) where Type is a
//     Capitalized simple_identifier — a companion / static / nested-type access.
//     EdgeReferences to the head Type.
//   - inherit        `class X : Bar(), Iface` (delegation_specifier) — the
//     supertype / interface. EdgeExtends for a constructor_invocation superclass,
//     EdgeReferences for a bare user_type supertype.
//
// Scope/shadow safety: only Capitalized names are treated as types (so a
// lowercase function call `foo()` or a local variable read never produces a
// type reference); normalizeKotlinTypeName additionally drops Kotlin primitives
// (Int, String, …) and any lowercase name. Constructor callees that are bound
// lambda parameters in scope are skipped — but those are lowercase by Kotlin
// convention and already excluded by the capitalization gate. Edges are
// deduped per (owner, type, line, ref_context).
func emitKotlinReferenceForms(root *sitter.Node, src []byte, filePath, fileID string, funcRanges []funcRange, result *parser.ExtractionResult) {
	if root == nil {
		return
	}
	seen := map[string]bool{}
	owner := func(line int) string {
		id := findEnclosingFunc(funcRanges, line)
		if id == "" {
			id = fileID
		}
		return id
	}
	emit := func(ownerID, typeName, useKind string, kind graph.EdgeKind, line int) {
		t := normalizeKotlinTypeName(typeName)
		if t == "" || ownerID == "" {
			return
		}
		key := ownerID + "\x00" + t + "\x00" + useKind + "\x00" + intKey(line)
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
			// OriginASTResolved (not Inferred): these are structural type
			// references read straight off the AST — a constructor call, a
			// cast/`is` target, a static `Type.Member` head, or a supertype
			// in the delegation list. The name binds to the (unique) type of
			// that name in the repo with no ambiguity, exactly like the
			// type-position EdgeTypedAs / EdgeExtends edges. Stamping
			// ast_resolved keeps the cross-package name-match guard — which
			// reverts weak-tier (text_matched / ast_inferred) EdgeReferences
			// whose target isn't import-reachable — from dropping these as if
			// they were name-only call guesses.
			Origin: graph.OriginASTResolved,
			Meta:   map[string]any{"ref_context": useKind},
		})
	}

	walkNodes(root, func(n *sitter.Node) {
		switch n.Type() {

		case "call_expression":
			// Constructor call detection. The callee is the first named child.
			if n.NamedChildCount() == 0 {
				return
			}
			callee := n.NamedChild(0)
			line := int(n.StartPoint().Row) + 1
			switch callee.Type() {
			case "simple_identifier":
				// `OkHttpClient()` — capitalized bare callee is a constructor.
				name := callee.Content(src)
				if isKotlinTypeName(name) {
					emit(owner(line), name, "instantiate", graph.EdgeInstantiates, line)
				}
			case "navigation_expression":
				// `OkHttpClient.Builder()` — the constructed type is the last
				// Capitalized segment (Builder); the head (OkHttpClient) is a
				// static reference, handled by the navigation_expression case
				// below as it is itself walked.
				if last := kotlinNavLastIdent(callee, src); isKotlinTypeName(last) {
					emit(owner(line), last, "instantiate", graph.EdgeInstantiates, line)
				}
			}

		case "navigation_expression":
			// `Type.Member` / `Type.Companion.X` — a static / companion / nested
			// access. The head names the type. Lowercase heads (`obj.method`)
			// are receiver-variable accesses, not type references, and are
			// dropped by the capitalization gate.
			head := kotlinNavHeadIdent(n, src)
			if isKotlinTypeName(head) {
				line := int(n.StartPoint().Row) + 1
				emit(owner(line), head, "static_access", graph.EdgeReferences, line)
			}

		case "as_expression":
			// `x as T` / `x as? T` — the user_type child is the cast target.
			if ut := kotlinFirstUserType(n); ut != nil {
				line := int(n.StartPoint().Row) + 1
				emit(owner(line), ut.Content(src), "cast", graph.EdgeReferences, line)
			}

		case "check_expression":
			// `x is T` / `x !is T` — the user_type child is the tested type.
			if ut := kotlinFirstUserType(n); ut != nil {
				line := int(n.StartPoint().Row) + 1
				emit(owner(line), ut.Content(src), "cast", graph.EdgeReferences, line)
			}

		case "delegation_specifier":
			// `class X : Bar(), Iface` supertype/interface list. A
			// constructor_invocation child is a superclass call (EdgeExtends);
			// a bare user_type is a supertype/interface (EdgeReferences). The
			// owner is the enclosing class node, resolved via funcRanges would
			// be wrong (it is not a function), so attribute inheritance to the
			// enclosing class def node — looked up by walking to the
			// class_declaration's type_identifier.
			line := int(n.StartPoint().Row) + 1
			ownerID := kotlinEnclosingClassID(n, src, filePath)
			if ownerID == "" {
				ownerID = fileID
			}
			for i := 0; i < int(n.NamedChildCount()); i++ {
				c := n.NamedChild(i)
				switch c.Type() {
				case "constructor_invocation":
					if ut := kotlinFirstUserType(c); ut != nil {
						emit(ownerID, ut.Content(src), "inherit", graph.EdgeExtends, line)
					}
				case "user_type":
					emit(ownerID, c.Content(src), "inherit", graph.EdgeReferences, line)
				}
			}
		}
	})
}

// isKotlinTypeName reports whether name is a non-empty Capitalized identifier
// that survives Kotlin primitive/lowercase filtering — i.e. a plausible type
// name for a constructor call or static access.
func isKotlinTypeName(name string) bool {
	if name == "" || name[0] < 'A' || name[0] > 'Z' {
		return false
	}
	return normalizeKotlinTypeName(name) != ""
}

// kotlinNavLastIdent returns the trailing selector identifier of a
// navigation_expression (`A.B.C` → "C"), or "".
func kotlinNavLastIdent(nav *sitter.Node, src []byte) string {
	if nav == nil {
		return ""
	}
	for i := int(nav.NamedChildCount()) - 1; i >= 0; i-- {
		c := nav.NamedChild(i)
		if c == nil {
			continue
		}
		if c.Type() == "navigation_suffix" {
			for j := 0; j < int(c.NamedChildCount()); j++ {
				cc := c.NamedChild(j)
				if cc != nil && cc.Type() == "simple_identifier" {
					return cc.Content(src)
				}
			}
		}
	}
	return ""
}

// kotlinNavHeadIdent returns the head identifier of a navigation_expression
// (`A.B.C` → "A"), descending through nested navigation_expressions to the
// leftmost simple_identifier, or "" when the head is not a bare identifier
// (e.g. a call result `foo().bar`).
func kotlinNavHeadIdent(nav *sitter.Node, src []byte) string {
	if nav == nil || nav.NamedChildCount() == 0 {
		return ""
	}
	head := nav.NamedChild(0)
	for head != nil && head.Type() == "navigation_expression" {
		if head.NamedChildCount() == 0 {
			return ""
		}
		head = head.NamedChild(0)
	}
	if head != nil && head.Type() == "simple_identifier" {
		return head.Content(src)
	}
	return ""
}

// kotlinFirstUserType returns the first user_type descendant child of node
// (direct or one level deep), or nil.
func kotlinFirstUserType(node *sitter.Node) *sitter.Node {
	if node == nil {
		return nil
	}
	for i := 0; i < int(node.NamedChildCount()); i++ {
		c := node.NamedChild(i)
		if c == nil {
			continue
		}
		if c.Type() == "user_type" {
			return c
		}
	}
	return nil
}

// kotlinEnclosingClassID walks up from a delegation_specifier to its
// class_declaration / object_declaration and returns the def node id
// (`<file>::<TypeName>`), or "" when no named enclosing type is found.
func kotlinEnclosingClassID(n *sitter.Node, src []byte, filePath string) string {
	cur := n.Parent()
	for cur != nil {
		switch cur.Type() {
		case "class_declaration", "object_declaration":
			if name := kotlinTypeIdentifierChild(cur, src); name != "" {
				return filePath + "::" + name
			}
			return ""
		}
		cur = cur.Parent()
	}
	return ""
}

// intKey renders an int as a short dedup-key string without importing strconv
// at the call sites that only need a key fragment.
func intKey(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}
