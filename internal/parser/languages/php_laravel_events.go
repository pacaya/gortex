package languages

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// Laravel event binding. A Laravel app dispatches an event
// (`event(new OrderShipped($o))`, `OrderShipped::dispatch($o)`) handled by a
// listener's typed `handle(OrderShipped $e)` method — discovered either by
// the `App\Listeners` convention or by the `$listen` map in an
// EventServiceProvider. The dispatch is keyed on the event type, which the
// static call graph cannot see. This pass tags listeners (two sources) and
// stamps a placeholder per dispatch site; the resolver fans out to every
// matching listener.

// laravelEventViaTag must match resolver.laravelEventVia — the languages
// package does not import resolver, so the two agree by value.
const laravelEventViaTag = "laravel-event"

// captureLaravelEvents tags listener handle methods (Source 1: typed handle
// under a Listeners namespace; Source 2: the EventServiceProvider $listen
// map) and stamps event-dispatch placeholders. Runs at the tail of Extract.
func captureLaravelEvents(result *parser.ExtractionResult, root *sitter.Node, filePath string, src []byte) {
	if root == nil || result == nil {
		return
	}
	nodesByLine := map[int][]*graph.Node{}
	classNodeByName := map[string]*graph.Node{}
	for _, n := range result.Nodes {
		if n == nil {
			continue
		}
		if n.Kind == graph.KindMethod || n.Kind == graph.KindFunction {
			nodesByLine[n.StartLine] = append(nodesByLine[n.StartLine], n)
		}
		if n.Kind == graph.KindType {
			classNodeByName[laravelSimpleName(n.Name)] = n
		}
	}

	fileNamespace := laravelFileNamespace(root, src)

	laravelWalk(root, func(n *sitter.Node) {
		switch n.Type() {
		case "method_declaration":
			// Source 1: a typed handle method under a Listeners namespace.
			if laravelMethodName(n, src) != "handle" {
				return
			}
			if !strings.Contains(fileNamespace, "Listeners") {
				return
			}
			if t := laravelFirstParamType(n, src); t != "" {
				laravelTagHandle(nodesByLine, n, src, t)
			}
		case "property_declaration":
			// Source 2: the $listen map of an EventServiceProvider.
			laravelTagListenMap(n, src, classNodeByName)
		}
	})

	// Dispatch sites.
	funcRanges := buildFuncRanges(result)
	seen := map[string]bool{}
	laravelWalk(root, func(n *sitter.Node) {
		evType := ""
		switch n.Type() {
		case "function_call_expression":
			// event(new XEvent(...))
			if fn := n.ChildByFieldName("function"); fn == nil || fn.Content(src) != "event" {
				return
			}
			evType = laravelFirstNewArgType(n, src)
		case "scoped_call_expression":
			// Event::dispatch(new XEvent()) or XEvent::dispatch(...)
			name := ""
			if nm := n.ChildByFieldName("name"); nm != nil {
				name = nm.Content(src)
			}
			if name != "dispatch" {
				return
			}
			scope := ""
			if sc := n.ChildByFieldName("scope"); sc != nil {
				scope = laravelSimpleName(sc.Content(src))
			}
			if scope == "Event" {
				evType = laravelFirstNewArgType(n, src)
			} else {
				evType = scope // the event dispatches itself
			}
		default:
			return
		}
		if evType == "" {
			return
		}
		line := int(n.StartPoint().Row) + 1
		from := findEnclosingFunc(funcRanges, line)
		if from == "" {
			return
		}
		k := from + "\x00" + evType
		if seen[k] {
			return
		}
		seen[k] = true
		result.Edges = append(result.Edges, &graph.Edge{
			From:     from,
			To:       "unresolved::*.handle",
			Kind:     graph.EdgeCalls,
			FilePath: filePath,
			Line:     line,
			Meta:     map[string]any{"via": laravelEventViaTag, "laravel_event_type": evType},
		})
	})
}

// laravelTagHandle stamps Meta["laravel_listener_type"] on a handle method.
func laravelTagHandle(nodesByLine map[int][]*graph.Node, method *sitter.Node, src []byte, evType string) {
	line := int(method.StartPoint().Row) + 1
	for _, n := range nodesByLine[line] {
		if n.Name != "handle" {
			continue
		}
		if n.Meta == nil {
			n.Meta = map[string]any{}
		}
		n.Meta["laravel_listener_type"] = evType
		return
	}
}

// laravelTagListenMap parses a `$listen = [Event::class => [Listener::class,
// ...]]` property and stamps the encoded map on the enclosing class node so
// the resolver can bind listeners declared in another file.
func laravelTagListenMap(prop *sitter.Node, src []byte, classNodeByName map[string]*graph.Node) {
	if !laravelPropertyNamed(prop, "listen", src) {
		return
	}
	arr := laravelFindDescendant(prop, "array_creation_expression")
	if arr == nil {
		return
	}
	var entries []string
	for i, _nc := 0, int(arr.NamedChildCount()); i < _nc; i++ {
		el := arr.NamedChild(i)
		if el == nil || el.Type() != "array_element_initializer" || el.NamedChildCount() < 2 {
			continue
		}
		event := laravelSimpleName(phpExtractClassRef(el.NamedChild(0), src))
		listeners := laravelClassRefList(el.NamedChild(1), src)
		if event == "" || len(listeners) == 0 {
			continue
		}
		entries = append(entries, event+"=>"+strings.Join(listeners, ","))
	}
	if len(entries) == 0 {
		return
	}
	className := laravelEnclosingClassName(prop, src)
	cn := classNodeByName[laravelSimpleName(className)]
	if cn == nil {
		return
	}
	if cn.Meta == nil {
		cn.Meta = map[string]any{}
	}
	cn.Meta["laravel_listen_map"] = strings.Join(entries, ";")
}

// laravelClassRefList collects the simple class names from an array of
// `Listener::class` entries (or a single bare class ref).
func laravelClassRefList(n *sitter.Node, src []byte) []string {
	var out []string
	if n == nil {
		return out
	}
	if n.Type() == "array_creation_expression" {
		for i, _nc := 0, int(n.NamedChildCount()); i < _nc; i++ {
			el := n.NamedChild(i)
			if el == nil {
				continue
			}
			ref := el
			if el.Type() == "array_element_initializer" && el.NamedChildCount() > 0 {
				ref = el.NamedChild(int(el.NamedChildCount()) - 1)
			}
			if c := laravelSimpleName(phpExtractClassRef(ref, src)); c != "" {
				out = append(out, c)
			}
		}
		return out
	}
	if c := laravelSimpleName(phpExtractClassRef(n, src)); c != "" {
		out = append(out, c)
	}
	return out
}

// laravelFirstNewArgType returns the constructed type of a call's first
// `new X(...)` argument.
func laravelFirstNewArgType(call *sitter.Node, src []byte) string {
	args := call.ChildByFieldName("arguments")
	if args == nil {
		return ""
	}
	for i, _nc := 0, int(args.NamedChildCount()); i < _nc; i++ {
		a := args.NamedChild(i)
		if a == nil {
			continue
		}
		expr := a
		if a.Type() == "argument" && a.NamedChildCount() > 0 {
			expr = a.NamedChild(0)
		}
		if expr != nil && expr.Type() == "object_creation_expression" {
			if t := laravelNewType(expr, src); t != "" {
				return t
			}
		}
		return ""
	}
	return ""
}

// laravelNewType returns the simple class name of a `new X(...)`.
func laravelNewType(newExpr *sitter.Node, src []byte) string {
	for i, _nc := 0, int(newExpr.NamedChildCount()); i < _nc; i++ {
		c := newExpr.NamedChild(i)
		if c == nil {
			continue
		}
		switch c.Type() {
		case "name", "qualified_name":
			return laravelSimpleName(c.Content(src))
		}
	}
	return ""
}

// laravelFirstParamType returns the simple type name of a method's first
// formal parameter.
func laravelFirstParamType(method *sitter.Node, src []byte) string {
	params := method.ChildByFieldName("parameters")
	if params == nil {
		return ""
	}
	for i, _nc := 0, int(params.NamedChildCount()); i < _nc; i++ {
		p := params.NamedChild(i)
		if p == nil || p.Type() != "simple_parameter" {
			continue
		}
		if t := p.ChildByFieldName("type"); t != nil {
			return laravelSimpleName(strings.TrimPrefix(strings.TrimSpace(t.Content(src)), "?"))
		}
		return ""
	}
	return ""
}

func laravelMethodName(method *sitter.Node, src []byte) string {
	if n := method.ChildByFieldName("name"); n != nil {
		return n.Content(src)
	}
	return ""
}

// laravelPropertyNamed reports whether a property_declaration declares
// $<name>.
func laravelPropertyNamed(prop *sitter.Node, name string, src []byte) bool {
	found := false
	laravelWalk(prop, func(n *sitter.Node) {
		if found || n.Type() != "property_element" {
			return
		}
		if vn := n.ChildByFieldName("name"); vn != nil {
			if strings.TrimPrefix(strings.TrimSpace(vn.Content(src)), "$") == name {
				found = true
			}
		} else if strings.TrimPrefix(strings.TrimSpace(n.Content(src)), "$") == name {
			found = true
		}
	})
	return found
}

// laravelEnclosingClassName walks up to the nearest class_declaration name.
func laravelEnclosingClassName(node *sitter.Node, src []byte) string {
	for n := node; n != nil; n = n.Parent() {
		if n.Type() == "class_declaration" {
			if nm := n.ChildByFieldName("name"); nm != nil {
				return nm.Content(src)
			}
			return ""
		}
	}
	return ""
}

// laravelFileNamespace returns the file's namespace name, or "".
func laravelFileNamespace(root *sitter.Node, src []byte) string {
	ns := ""
	laravelWalk(root, func(n *sitter.Node) {
		if ns != "" || n.Type() != "namespace_definition" {
			return
		}
		if nm := laravelFindDescendant(n, "namespace_name"); nm != nil {
			ns = nm.Content(src)
		}
	})
	return ns
}

// laravelFindDescendant returns the first descendant of n of the given type.
func laravelFindDescendant(n *sitter.Node, typ string) *sitter.Node {
	var found *sitter.Node
	laravelWalk(n, func(c *sitter.Node) {
		if found == nil && c.Type() == typ {
			found = c
		}
	})
	return found
}

// laravelSimpleName returns the last segment of a `\`- or `::`-qualified
// PHP name.
func laravelSimpleName(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndex(s, "::"); i >= 0 {
		s = s[i+2:]
	}
	if i := strings.LastIndexByte(s, '\\'); i >= 0 {
		s = s[i+1:]
	}
	return strings.TrimSpace(s)
}

// laravelWalk visits n and all its named descendants.
func laravelWalk(n *sitter.Node, fn func(*sitter.Node)) {
	if n == nil {
		return
	}
	fn(n)
	for i, _nc := 0, int(n.NamedChildCount()); i < _nc; i++ {
		laravelWalk(n.NamedChild(i), fn)
	}
}
