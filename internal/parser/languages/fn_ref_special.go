package languages

import (
	"strings"

	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// normalizeFnRefSpecial rewrites a bespoke whole-node function-reference syntax
// into a resolvable (member name, receiver hint) pair so the gate can bind it.
// It returns ok=false for a node that is not a special form.
//
// recvHint is "<self>" when the reference is scoped to the enclosing type
// (`this.m` / `self.m` / Kotlin `::m`), a concrete type name when the syntax
// names one (`Foo::bar`), or "" when the name resolves repo-wide (a selector).
//
// Forms handled: Java `method_reference` (`Foo::bar`, `this::bar`), Kotlin
// `callable_reference` (`::m`, `Foo::m`), `this.`/`self.` member access in
// JS/TS/Python/C#, Ruby `method(:sym)` and `&:sym`, and Swift/ObjC
// `#selector(...)` / `@selector(...)`.
func normalizeFnRefSpecial(n *sitter.Node, src []byte) (refName, recvHint string, ok bool) {
	switch n.Type() {
	case "method_reference":
		return splitColonColonRef(n, src)
	case "callable_reference":
		return splitColonColonRef(n, src)
	case "member_expression": // JS / TS
		return selfMemberAccess(n, src, "object", "property")
	case "attribute": // Python
		return selfMemberAccess(n, src, "object", "attribute")
	case "member_access_expression": // C#
		return selfMemberAccess(n, src, "expression", "name")
	case "call": // Ruby method(:sym)
		return rubyMethodSymbol(n, src)
	case "block_argument", "unary": // Ruby &:sym
		return rubySymbolProc(n, src)
	case "selector_expression":
		// Swift/ObjC #selector(m) / @selector(m:) — distinguished from a Go
		// selector by the leading '#' / '@'.
		return swiftSelector(n, src)
	}
	return "", "", false
}

// splitColonColonRef handles a `::`-style callable reference: the trailing
// identifier is the member, an optional leading type/expression the qualifier.
func splitColonColonRef(n *sitter.Node, src []byte) (refName, recvHint string, ok bool) {
	var first, last *sitter.Node
	for i, _nc := 0, int(n.NamedChildCount()); i < _nc; i++ {
		c := n.NamedChild(i)
		if c == nil {
			continue
		}
		if first == nil {
			first = c
		}
		last = c
	}
	if last == nil {
		return "", "", false
	}
	member := strings.TrimSpace(last.Content(src))
	if member == "" || member == "new" {
		return "", "", false
	}
	// Only a leading qualifier distinct from the member is a receiver hint;
	// a bare `::member` (Kotlin) resolves in the enclosing scope.
	if first != nil && first != last {
		q := strings.TrimSpace(first.Content(src))
		if q == "this" || q == "self" {
			return member, "<self>", true
		}
		return member, q, true
	}
	return member, "<self>", true
}

// selfMemberAccess handles a `this.m` / `self.m` member access: the object must
// be `this` or `self`, the property names the method.
func selfMemberAccess(n *sitter.Node, src []byte, objField, propField string) (refName, recvHint string, ok bool) {
	obj := n.ChildByFieldName(objField)
	prop := n.ChildByFieldName(propField)
	if obj == nil || prop == nil {
		return "", "", false
	}
	switch strings.TrimSpace(obj.Content(src)) {
	case "this", "self":
		name := strings.TrimSpace(prop.Content(src))
		if name == "" {
			return "", "", false
		}
		return name, "<self>", true
	}
	return "", "", false
}

// rubyMethodSymbol handles Ruby `method(:sym)` — a call to `method` whose sole
// argument is a `:sym` literal naming the referenced method.
func rubyMethodSymbol(n *sitter.Node, src []byte) (refName, recvHint string, ok bool) {
	m := n.ChildByFieldName("method")
	if m == nil || strings.TrimSpace(m.Content(src)) != "method" {
		return "", "", false
	}
	args := n.ChildByFieldName("arguments")
	if args == nil {
		return "", "", false
	}
	for i, _nc := 0, int(args.NamedChildCount()); i < _nc; i++ {
		if a := args.NamedChild(i); a != nil && a.Type() == "simple_symbol" {
			if name := strings.TrimPrefix(strings.TrimSpace(a.Content(src)), ":"); name != "" {
				return name, "<self>", true
			}
		}
	}
	return "", "", false
}

// rubySymbolProc handles Ruby `&:sym` symbol-to-proc — the symbol names the
// method invoked on each element; resolved repo-wide.
func rubySymbolProc(n *sitter.Node, src []byte) (refName, recvHint string, ok bool) {
	for i, _nc := 0, int(n.NamedChildCount()); i < _nc; i++ {
		if c := n.NamedChild(i); c != nil && c.Type() == "simple_symbol" {
			if name := strings.TrimPrefix(strings.TrimSpace(c.Content(src)), ":"); name != "" {
				return name, "", true
			}
		}
	}
	return "", "", false
}

// swiftSelector handles Swift/ObjC `#selector(m)` / `@selector(m:)` — the inner
// identifier names the selector method; resolved within the enclosing type.
func swiftSelector(n *sitter.Node, src []byte) (refName, recvHint string, ok bool) {
	text := strings.TrimSpace(n.Content(src))
	if !strings.HasPrefix(text, "#selector") && !strings.HasPrefix(text, "@selector") {
		return "", "", false
	}
	// Last identifier child is the selector base name.
	var last *sitter.Node
	walkNodes(n, func(c *sitter.Node) {
		if c.Type() == "simple_identifier" || c.Type() == "identifier" {
			last = c
		}
	})
	if last == nil {
		return "", "", false
	}
	if name := strings.TrimSpace(last.Content(src)); name != "" {
		return name, "<self>", true
	}
	return "", "", false
}
