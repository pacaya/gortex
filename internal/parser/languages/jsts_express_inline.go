package languages

import (
	"strconv"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// Express/Node inline-arrow route-handler body attribution. A route whose
// handler is an inline arrow (`app.get('/u', (req,res) => { svc.list() })`)
// has no named function for the call graph to attribute the body's calls to,
// so a trace from the route to the service it calls is lost. This pass
// materialises a synthetic handler node anchored to the route call line and
// attributes the arrow body's *application* calls to it (the framework's
// req/res/next helpers and JS built-ins are filtered out). The contracts
// layer anchors the route Contract's SymbolID to the same node, so the route
// connects through the anonymous handler to the services it invokes.

// expressRouteVerbs are the HTTP verbs (and middleware mounts) a route call
// uses as its method name.
var expressRouteVerbs = map[string]bool{
	"get": true, "post": true, "put": true, "delete": true, "patch": true,
	"options": true, "head": true, "all": true, "use": true,
}

// expressRouteReceivers are the common router/app receiver names.
var expressRouteReceivers = map[string]bool{
	"app": true, "router": true, "fastify": true, "server": true, "api": true, "route": true,
}

// expressInlineCallCap bounds the calls attributed from one handler body.
const expressInlineCallCap = 64

// ExpressInlineHandlerNodeID is the deterministic id of the synthetic
// handler node a route's inline arrow is attributed to. The contracts layer
// reconstructs it from the route call line to anchor the route Contract.
func ExpressInlineHandlerNodeID(filePath string, line int) string {
	return filePath + "::express-handler@" + strconv.Itoa(line)
}

// captureExpressInlineHandlers attributes inline route-handler body calls.
// Runs at the tail of Extract.
func captureExpressInlineHandlers(result *parser.ExtractionResult, root *sitter.Node, filePath string, src []byte) {
	if root == nil || result == nil {
		return
	}
	expressWalk(root, func(call *sitter.Node) {
		if call.Type() != "call_expression" {
			return
		}
		fn := call.ChildByFieldName("function")
		if fn == nil || fn.Type() != "member_expression" {
			return
		}
		prop := fn.ChildByFieldName("property")
		recv := fn.ChildByFieldName("object")
		if prop == nil || recv == nil {
			return
		}
		if !expressRouteVerbs[prop.Content(src)] {
			return
		}
		if !expressIsRouteReceiver(recv, src) {
			return
		}
		args := call.ChildByFieldName("arguments")
		if args == nil || !expressFirstArgIsString(args) {
			return
		}
		// Handler-position args: every arg after the path string — inline
		// arrows, named middleware idents, and XController.method handlers.
		handlerArgs := expressHandlerPositionArgs(args)
		if len(handlerArgs) == 0 {
			return
		}
		line := int(call.StartPoint().Row) + 1
		handlerID := ExpressInlineHandlerNodeID(filePath, line)
		// Materialise the handler node so the route Contract can anchor to it
		// (even when the body calls nothing the call graph tracks) and the
		// body-call / named-handler edges have a real From.
		result.Nodes = append(result.Nodes, &graph.Node{
			ID: handlerID, Kind: graph.KindFunction, Name: "route handler",
			FilePath: filePath, StartLine: line, EndLine: int(call.EndPoint().Row) + 1,
			Language: "javascript",
			Meta:     map[string]any{"express_handler": true, "signature": "(req, res) => {…}"},
		})
		for _, a := range handlerArgs {
			switch a.Type() {
			case "arrow_function", "function_expression", "function":
				params := expressHandlerParamNames(a, src)
				expressEmitHandlerCalls(result, a, src, handlerID, filePath, params)
			case "identifier":
				expressEmitHandlerRef(result, handlerID, filePath, line, "", a.Content(src))
			case "member_expression":
				cls := expressRootIdent(a.ChildByFieldName("object"), src)
				if m := a.ChildByFieldName("property"); m != nil && cls != "" {
					expressEmitHandlerRef(result, handlerID, filePath, line, cls, m.Content(src))
				}
			}
		}
	})
}

// expressEmitHandlerCalls attributes the application calls in a handler body
// to handlerID, returning the count emitted.
func expressEmitHandlerCalls(result *parser.ExtractionResult, handler *sitter.Node, src []byte, handlerID, filePath string, params map[string]bool) int {
	body := handler.ChildByFieldName("body")
	if body == nil {
		return 0
	}
	seen := map[string]bool{}
	emitted := 0
	expressWalk(body, func(c *sitter.Node) {
		if emitted >= expressInlineCallCap || c.Type() != "call_expression" {
			return
		}
		callee, ok := expressCalleeName(c.ChildByFieldName("function"), src, params)
		if !ok || callee == "" || seen[callee] {
			return
		}
		seen[callee] = true
		emitted++
		result.Edges = append(result.Edges, &graph.Edge{
			From: handlerID, To: "unresolved::" + callee, Kind: graph.EdgeCalls,
			FilePath: filePath, Line: int(c.StartPoint().Row) + 1,
			Meta: map[string]any{"express_inline_handler": true},
		})
	})
	return emitted
}

// expressCalleeName returns the application callee name of a call, or
// ok=false when the call is a reserved framework/runtime helper or a call on
// the handler's own (req/res/next) parameters.
func expressCalleeName(fn *sitter.Node, src []byte, params map[string]bool) (string, bool) {
	if fn == nil {
		return "", false
	}
	switch fn.Type() {
	case "identifier":
		name := fn.Content(src)
		if params[name] || jsIsReservedBareCall(name) {
			return "", false
		}
		return name, true
	case "member_expression":
		root := expressRootIdent(fn.ChildByFieldName("object"), src)
		if params[root] || jsIsReservedReceiver(root) {
			return "", false
		}
		if prop := fn.ChildByFieldName("property"); prop != nil {
			return prop.Content(src), true
		}
	}
	return "", false
}

// expressRootIdent returns the leftmost identifier of a member/call chain
// (`req` for `req.body.parse`).
func expressRootIdent(n *sitter.Node, src []byte) string {
	for n != nil {
		switch n.Type() {
		case "identifier":
			return n.Content(src)
		case "member_expression":
			n = n.ChildByFieldName("object")
		case "call_expression":
			n = n.ChildByFieldName("function")
		case "subscript_expression":
			n = n.ChildByFieldName("object")
		default:
			return strings.TrimSpace(n.Content(src))
		}
	}
	return ""
}

// expressIsRouteReceiver reports whether a route call's receiver looks like an
// app/router instance.
func expressIsRouteReceiver(recv *sitter.Node, src []byte) bool {
	name := expressRootIdent(recv, src)
	if name == "" {
		return false
	}
	low := strings.ToLower(name)
	return expressRouteReceivers[low] || strings.Contains(low, "router") || strings.Contains(low, "app")
}

// expressFirstArgIsString reports whether the first call argument is a string
// literal (the route path).
func expressFirstArgIsString(args *sitter.Node) bool {
	for i, _nc := 0, int(args.NamedChildCount()); i < _nc; i++ {
		if c := args.NamedChild(i); c != nil {
			return c.Type() == "string"
		}
	}
	return false
}

// expressHandlerPositionArgs returns every argument after the route path
// string — the handler/middleware position.
func expressHandlerPositionArgs(args *sitter.Node) []*sitter.Node {
	var out []*sitter.Node
	sawPath := false
	for i, _nc := 0, int(args.NamedChildCount()); i < _nc; i++ {
		c := args.NamedChild(i)
		if c == nil {
			continue
		}
		if !sawPath {
			if c.Type() == "string" {
				sawPath = true
			}
			continue
		}
		out = append(out, c)
	}
	return out
}

// expressEmitHandlerRef stamps a placeholder ref from the route handler
// anchor to a named middleware/handler (cls == "") or a XController.method
// handler (cls != ""), for the express resolver to bind by convention.
func expressEmitHandlerRef(result *parser.ExtractionResult, handlerID, filePath string, line int, cls, member string) {
	if member == "" {
		return
	}
	meta := map[string]any{"express_handler_ref": true}
	if cls != "" {
		meta["express_ref_class"] = cls
		meta["express_ref_method"] = member
	} else {
		meta["express_ref_name"] = member
	}
	result.Edges = append(result.Edges, &graph.Edge{
		From: handlerID, To: "unresolved::" + member, Kind: graph.EdgeCalls,
		FilePath: filePath, Line: line, Meta: meta,
	})
}

// expressHandlerParamNames returns the parameter names of a handler function
// (`req`, `res`, `next`).
func expressHandlerParamNames(handler *sitter.Node, src []byte) map[string]bool {
	out := map[string]bool{}
	params := handler.ChildByFieldName("parameters")
	if params == nil {
		// Single-param arrow without parens: `req => ...`.
		if p := handler.ChildByFieldName("parameter"); p != nil {
			out[p.Content(src)] = true
		}
		return out
	}
	for i, _nc := 0, int(params.NamedChildCount()); i < _nc; i++ {
		p := params.NamedChild(i)
		if p == nil {
			continue
		}
		if p.Type() == "identifier" {
			out[p.Content(src)] = true
		} else if id := expressRootIdent(p, src); id != "" {
			out[id] = true
		}
	}
	return out
}

// expressWalk visits n and all its named descendants.
func expressWalk(n *sitter.Node, fn func(*sitter.Node)) {
	if n == nil {
		return
	}
	fn(n)
	for i, _nc := 0, int(n.NamedChildCount()); i < _nc; i++ {
		expressWalk(n.NamedChild(i), fn)
	}
}
