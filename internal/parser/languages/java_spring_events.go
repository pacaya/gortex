package languages

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// Spring application-event binding. A Spring component publishes a typed
// event (`applicationEventPublisher.publishEvent(new OrderPlaced(id))`) that
// is handled by every method annotated `@EventListener void on(OrderPlaced
// e)` or by an `ApplicationListener<OrderPlaced>.onApplicationEvent`. The
// dispatch is keyed on the Java event type, which the string-topic
// event-channel pass cannot see. This pass tags each listener with its
// event type and stamps a placeholder per publishEvent site, which the
// resolver's ResolveSpringEventCalls fans out to every matching listener.

// springEventViaTag must match resolver.springEventVia — the languages
// package does not import resolver, so the two agree by value.
const springEventViaTag = "spring-event"

// captureSpringEvents tags @EventListener / ApplicationListener methods and
// stamps publishEvent placeholders. Runs at the tail of Extract so the
// method nodes already exist.
func captureSpringEvents(result *parser.ExtractionResult, root *sitter.Node, filePath string, src []byte) {
	if root == nil || result == nil {
		return
	}
	nodesByLine := map[int][]*graph.Node{}
	for _, n := range result.Nodes {
		if n != nil && (n.Kind == graph.KindMethod || n.Kind == graph.KindFunction) {
			nodesByLine[n.StartLine] = append(nodesByLine[n.StartLine], n)
		}
	}

	// Pass 1: tag listeners.
	springWalk(root, func(n *sitter.Node) {
		switch n.Type() {
		case "method_declaration":
			if springHasEventListener(javaCollectAnnotations(n, src)) {
				if t := springFirstParamType(n, src); t != "" {
					springTagListener(nodesByLine, n, src, t)
				}
			}
		case "class_declaration":
			evType := springApplicationListenerArg(n, src)
			if evType == "" {
				return
			}
			if body := n.ChildByFieldName("body"); body != nil {
				for i, _nc := 0, int(body.NamedChildCount()); i < _nc; i++ {
					m := body.NamedChild(i)
					if m != nil && m.Type() == "method_declaration" && springMethodName(m, src) == "onApplicationEvent" {
						springTagListener(nodesByLine, m, src, evType)
					}
				}
			}
		}
	})

	// Pass 2: publishEvent sites.
	funcRanges := buildFuncRanges(result)
	seen := map[string]bool{}
	springWalk(root, func(call *sitter.Node) {
		if call.Type() != "method_invocation" {
			return
		}
		name := call.ChildByFieldName("name")
		if name == nil || name.Content(src) != "publishEvent" {
			return
		}
		evType := springPublishedType(call, src)
		if evType == "" {
			return
		}
		line := int(call.StartPoint().Row) + 1
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
			To:       "unresolved::*." + evType,
			Kind:     graph.EdgeCalls,
			FilePath: filePath,
			Line:     line,
			Meta:     map[string]any{"via": springEventViaTag, "spring_event_type": evType},
		})
	})
}

func springTagListener(nodesByLine map[int][]*graph.Node, methodDecl *sitter.Node, src []byte, evType string) {
	name := springMethodName(methodDecl, src)
	if name == "" {
		return
	}
	line := int(methodDecl.StartPoint().Row) + 1
	for _, n := range nodesByLine[line] {
		if n.Name != name {
			continue
		}
		if n.Meta == nil {
			n.Meta = map[string]any{}
		}
		n.Meta["spring_listener_type"] = evType
		return
	}
}

// springHasEventListener reports whether any annotation is @EventListener.
func springHasEventListener(anns []javaAnnotation) bool {
	for _, a := range anns {
		if springSimpleTypeName(a.name) == "EventListener" {
			return true
		}
	}
	return false
}

// springFirstParamType returns the simple type name of a method's first
// formal parameter.
func springFirstParamType(method *sitter.Node, src []byte) string {
	params := method.ChildByFieldName("parameters")
	if params == nil {
		return ""
	}
	for i, _nc := 0, int(params.NamedChildCount()); i < _nc; i++ {
		p := params.NamedChild(i)
		if p == nil || p.Type() != "formal_parameter" {
			continue
		}
		if t := p.ChildByFieldName("type"); t != nil {
			return springSimpleTypeName(t.Content(src))
		}
		return ""
	}
	return ""
}

// springApplicationListenerArg returns the generic argument of an
// `implements ApplicationListener<X>` clause, or "".
func springApplicationListenerArg(classDecl *sitter.Node, src []byte) string {
	ifaces := classDecl.ChildByFieldName("interfaces")
	if ifaces == nil {
		return ""
	}
	found := ""
	springWalk(ifaces, func(n *sitter.Node) {
		if found != "" || n.Type() != "generic_type" {
			return
		}
		var base, args *sitter.Node
		for i, _nc := 0, int(n.NamedChildCount()); i < _nc; i++ {
			c := n.NamedChild(i)
			switch {
			case c == nil:
			case c.Type() == "type_arguments":
				args = c
			case base == nil:
				base = c
			}
		}
		if base == nil || args == nil || springSimpleTypeName(base.Content(src)) != "ApplicationListener" {
			return
		}
		for i, _nc := 0, int(args.NamedChildCount()); i < _nc; i++ {
			a := args.NamedChild(i)
			if a != nil && a.IsNamed() {
				found = springSimpleTypeName(a.Content(src))
				return
			}
		}
	})
	return found
}

// springPublishedType returns the event type of a publishEvent(new X(...))
// call's first argument, or "".
func springPublishedType(call *sitter.Node, src []byte) string {
	args := call.ChildByFieldName("arguments")
	if args == nil {
		return ""
	}
	for i, _nc := 0, int(args.NamedChildCount()); i < _nc; i++ {
		a := args.NamedChild(i)
		if a == nil {
			continue
		}
		if a.Type() == "object_creation_expression" {
			return inferTypeFromJavaNewExpr(a, src)
		}
		return "" // first arg is not a constructor — type not statically known
	}
	return ""
}

func springMethodName(method *sitter.Node, src []byte) string {
	if n := method.ChildByFieldName("name"); n != nil {
		return n.Content(src)
	}
	return ""
}

// springSimpleTypeName strips a Java type to its simple name: generics and
// package/scope qualifiers removed (com.foo.OrderPlaced<X> → OrderPlaced).
func springSimpleTypeName(t string) string {
	t = strings.TrimSpace(t)
	if i := strings.IndexByte(t, '<'); i >= 0 {
		t = t[:i]
	}
	if i := strings.LastIndexByte(t, '.'); i >= 0 {
		t = t[i+1:]
	}
	return strings.TrimSpace(t)
}

// springWalk visits n and all its named descendants.
func springWalk(n *sitter.Node, fn func(*sitter.Node)) {
	if n == nil {
		return
	}
	fn(n)
	for i, _nc := 0, int(n.NamedChildCount()); i < _nc; i++ {
		springWalk(n.NamedChild(i), fn)
	}
}
