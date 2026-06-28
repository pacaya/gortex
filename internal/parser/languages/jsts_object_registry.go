package languages

import (
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// Object-literal command/handler registry dispatch detection. A registry
// maps keys to handler classes in an object literal
// (`this.commands = {[Cmd.ADD]: AddCommand, [Cmd.RM]: RmCommand}`) and is
// invoked by computed member + construction + method call
// (`new this.commands[cmd]().execute()`). The static call graph cannot see
// which handler runs because the key is dynamic. This pass records the
// registered classes and stamps a placeholder EdgeCalls from the
// dispatching function to each handler class; the resolver's
// ResolveObjectRegistryCalls binds each to the class's invoked method.
// Shared by the JavaScript and TypeScript extractors.

// objectRegistryViaTag must match resolver.objectRegistryVia — the
// languages package does not import resolver, so the two agree by value.
const objectRegistryViaTag = "object-registry"

// objectRegistryMethods are the handler entry-point methods a registry
// dispatch invokes on the constructed handler.
var objectRegistryMethods = map[string]bool{
	"execute": true,
	"run":     true,
	"handle":  true,
}

// captureObjectRegistryDispatches finds object-literal handler registries
// and the computed-member dispatch sites that invoke them, stamping a
// placeholder EdgeCalls per registered class. Runs at the tail of Extract
// (after all nodes exist) so the dispatcher's enclosing function resolves.
// Minified bundles are skipped — their single-line, mangled shape produces
// no reliable registry.
func captureObjectRegistryDispatches(result *parser.ExtractionResult, root *sitter.Node, filePath string, src []byte) {
	if root == nil || result == nil || objectRegistryLooksMinified(src) {
		return
	}

	// 1. Collect registries: binding name → registered handler classes.
	registries := map[string][]string{}
	objectRegistryWalk(root, func(n *sitter.Node) {
		if n.Type() != "object" {
			return
		}
		binding, ok := objectRegistryBinding(n, src)
		if !ok {
			return
		}
		if classes := objectRegistryEntries(n, src); len(classes) > 0 {
			registries[binding] = append(registries[binding], classes...)
		}
	})
	if len(registries) == 0 {
		return
	}

	// 2. Find dispatch sites: a computed member on a registry binding,
	// invoked (constructed + method, or directly called).
	funcRanges := buildFuncRanges(result)
	seen := map[string]bool{}
	tagged := map[string]bool{}
	objectRegistryWalk(root, func(n *sitter.Node) {
		if n.Type() != "subscript_expression" {
			return
		}
		binding := objectRegistrySubscriptName(n, src)
		classes := registries[binding]
		if len(classes) == 0 {
			return
		}
		method, isCall := objectRegistryDispatchMethod(n, src)
		if !isCall {
			return
		}
		line := int(n.StartPoint().Row) + 1
		from := findEnclosingFunc(funcRanges, line)
		if from == "" {
			return
		}
		for _, c := range classes {
			k := from + "\x00" + c + "\x00" + method
			if seen[k] {
				continue
			}
			seen[k] = true
			result.Edges = append(result.Edges, &graph.Edge{
				From:     from,
				To:       "unresolved::*." + c,
				Kind:     graph.EdgeCalls,
				FilePath: filePath,
				Line:     line,
				Meta: map[string]any{
					"via":              objectRegistryViaTag,
					"registry_binding": binding,
					"registry_value":   c,
					"registry_method":  method,
				},
			})
		}
		tagged[binding] = true
	})

	// 3. Tag the registry var/field node so the registry is discoverable.
	if len(tagged) == 0 {
		return
	}
	for _, nd := range result.Nodes {
		if nd == nil || !tagged[nd.Name] {
			continue
		}
		switch nd.Kind {
		case graph.KindVariable, graph.KindConstant, graph.KindField:
			if nd.Meta == nil {
				nd.Meta = map[string]any{}
			}
			nd.Meta["registry"] = nd.Name
		}
	}
}

// objectRegistryBinding returns the variable/field name an object literal
// is assigned to: `const commands = {...}`, `commands = {...}`, or
// `this.commands = {...}`.
func objectRegistryBinding(obj *sitter.Node, src []byte) (string, bool) {
	p := obj.Parent()
	if p == nil {
		return "", false
	}
	switch p.Type() {
	case "variable_declarator":
		if n := p.ChildByFieldName("name"); n != nil && n.Type() == "identifier" {
			return n.Content(src), true
		}
	case "assignment_expression":
		left := p.ChildByFieldName("left")
		if left == nil {
			return "", false
		}
		switch left.Type() {
		case "identifier":
			return left.Content(src), true
		case "member_expression":
			if prop := left.ChildByFieldName("property"); prop != nil {
				return prop.Content(src), true
			}
		}
	}
	return "", false
}

// objectRegistryEntries returns the handler-class names of a registry
// object literal — the identifier values of its top-level (depth-1) pairs,
// for both plain (`add: AddCommand`) and computed (`[Cmd.ADD]: AddCommand`)
// keys. Non-identifier values (nested objects, calls) are skipped.
func objectRegistryEntries(obj *sitter.Node, src []byte) []string {
	var out []string
	for i, _nc := 0, int(obj.NamedChildCount()); i < _nc; i++ {
		pair := obj.NamedChild(i)
		if pair == nil || pair.Type() != "pair" {
			continue
		}
		if val := pair.ChildByFieldName("value"); val != nil && val.Type() == "identifier" {
			out = append(out, val.Content(src))
		}
	}
	return out
}

// objectRegistrySubscriptName returns the last identifier of a computed
// member's object: `commands` for `commands[k]` and for `this.commands[k]`.
func objectRegistrySubscriptName(sub *sitter.Node, src []byte) string {
	obj := sub.ChildByFieldName("object")
	if obj == nil {
		return ""
	}
	switch obj.Type() {
	case "identifier":
		return obj.Content(src)
	case "member_expression":
		if prop := obj.ChildByFieldName("property"); prop != nil {
			return prop.Content(src)
		}
	}
	return ""
}

// objectRegistryDispatchMethod walks up from a registry subscript to the
// invoking call, returning the entry-point method (execute/run/handle) and
// whether the subscript is actually invoked. It climbs through the
// constructor (`new registry[k]()`) to the `.execute()` member call, and
// also accepts a bare `registry[k]()` direct call.
func objectRegistryDispatchMethod(sub *sitter.Node, src []byte) (method string, isCall bool) {
	cur := sub
	for i := 0; i < 4 && cur != nil; i++ {
		p := cur.Parent()
		if p == nil {
			return "", false
		}
		switch p.Type() {
		case "new_expression", "parenthesized_expression":
			cur = p
			continue
		case "member_expression":
			prop := p.ChildByFieldName("property")
			gp := p.Parent()
			if prop != nil && gp != nil && gp.Type() == "call_expression" {
				if m := prop.Content(src); objectRegistryMethods[m] {
					return m, true
				}
			}
			return "", false
		case "call_expression":
			// Bare `registry[k]()` / `new registry[k]()()` direct dispatch.
			return "", true
		default:
			return "", false
		}
	}
	return "", false
}

// objectRegistryLooksMinified reports whether the source looks like a
// minified bundle (one giant line or almost no newlines), where a
// best-effort registry parse would be noise.
func objectRegistryLooksMinified(src []byte) bool {
	if len(src) == 0 {
		return false
	}
	maxLine, cur, newlines := 0, 0, 0
	for _, b := range src {
		if b == '\n' {
			if cur > maxLine {
				maxLine = cur
			}
			cur = 0
			newlines++
			continue
		}
		cur++
	}
	if cur > maxLine {
		maxLine = cur
	}
	if maxLine > 2000 {
		return true
	}
	return newlines == 0 && len(src) > 500
}

// objectRegistryWalk visits n and all its named descendants.
func objectRegistryWalk(n *sitter.Node, fn func(*sitter.Node)) {
	if n == nil {
		return
	}
	fn(n)
	for i, _nc := 0, int(n.NamedChildCount()); i < _nc; i++ {
		objectRegistryWalk(n.NamedChild(i), fn)
	}
}
