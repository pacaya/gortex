package languages

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// RTK Query createApi endpoint + generated-hook detection. Redux Toolkit
// Query declares data endpoints inside a `createApi({ endpoints: (builder)
// => ({ getUser: builder.query(...) }) })` call and auto-generates a React
// hook per endpoint (`useGetUserQuery`) that has no source node. This pass
// mints a node per endpoint and per generated hook, and stamps a
// placeholder EdgeCalls hook → endpoint, which the resolver's
// ResolveRTKQueryCalls binds. A component's `useGetUserQuery()` call then
// reaches the endpoint's query body. Shared by the JS and TS extractors.

// rtkQueryViaTag must match resolver.rtkQueryVia — the languages package
// does not import resolver, so the two agree by value.
const rtkQueryViaTag = "rtk-query"

// captureRTKQueryEndpoints finds createApi endpoint definitions, mints
// endpoint + generated-hook nodes, and stamps the hook → endpoint
// placeholder. Runs at the tail of Extract so a hand-written hook of the
// same name (already in result.Nodes) suppresses the synthetic one.
func captureRTKQueryEndpoints(result *parser.ExtractionResult, root *sitter.Node, filePath, language string, src []byte) {
	if root == nil || result == nil {
		return
	}
	// Names already defined in source — a hand-written useXQuery look-alike
	// must not be shadowed by a synthetic hook node.
	sourceNames := map[string]bool{}
	for _, n := range result.Nodes {
		if n != nil && n.Name != "" {
			sourceNames[n.Name] = true
		}
	}

	rtkWalk(root, func(call *sitter.Node) {
		if call.Type() != "call_expression" {
			return
		}
		if jsCalleeLastName(call.ChildByFieldName("function"), src) != "createApi" {
			return
		}
		arrow := rtkEndpointsArrow(call, src)
		if arrow == nil {
			return
		}
		obj := rtkReturnedObject(arrow)
		if obj == nil {
			return
		}
		for i, _nc := 0, int(obj.NamedChildCount()); i < _nc; i++ {
			pair := obj.NamedChild(i)
			if pair == nil || pair.Type() != "pair" {
				continue
			}
			endpoint := rtkPairKey(pair, src)
			val := pair.ChildByFieldName("value")
			if endpoint == "" || val == nil || val.Type() != "call_expression" {
				continue
			}
			kind := jsCalleeLastName(val.ChildByFieldName("function"), src)
			if kind != "query" && kind != "mutation" {
				continue
			}
			rtkEmitEndpoint(result, filePath, language, endpoint, kind, val, sourceNames)
		}
	})
}

// rtkEmitEndpoint mints the endpoint node, the generated-hook node (unless
// a source hook of that name exists), and the hook → endpoint placeholder.
func rtkEmitEndpoint(result *parser.ExtractionResult, filePath, language, endpoint, kind string, def *sitter.Node, sourceNames map[string]bool) {
	endpointID := filePath + "::api." + endpoint
	if sourceNames["api."+endpoint] {
		return // already materialised (re-run idempotency)
	}
	startLn := int(def.StartPoint().Row) + 1
	endLn := int(def.EndPoint().Row) + 1
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: endpointID, Kind: graph.KindFunction, Name: "api." + endpoint,
		FilePath: filePath, StartLine: startLn, EndLine: endLn, Language: language,
		Meta: map[string]any{"rtk_endpoint": endpoint, "rtk_kind": kind, "signature": "api." + endpoint + "()"},
	})
	sourceNames["api."+endpoint] = true

	suffix := "Query"
	if kind == "mutation" {
		suffix = "Mutation"
	}
	hookName := "use" + rtkPascal(endpoint) + suffix
	// A hand-written hook of this name in source wins — skip synthesis.
	if sourceNames[hookName] {
		return
	}
	hookID := filePath + "::" + hookName
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: hookID, Kind: graph.KindFunction, Name: hookName,
		FilePath: filePath, StartLine: startLn, EndLine: endLn, Language: language,
		Meta: map[string]any{"rtk_generated_hook": true, "generated": true, "codegen_tool": "rtk-query", "signature": hookName + "()"},
	})
	sourceNames[hookName] = true
	result.Edges = append(result.Edges, &graph.Edge{
		From: hookID, To: "unresolved::*." + endpoint, Kind: graph.EdgeCalls,
		FilePath: filePath, Line: startLn,
		Meta: map[string]any{"via": rtkQueryViaTag, "rtk_endpoint": endpoint},
	})
}

// rtkEndpointsArrow returns the `endpoints: (builder) => ({...})` arrow (or
// function) value from a createApi config object.
func rtkEndpointsArrow(call *sitter.Node, src []byte) *sitter.Node {
	args := call.ChildByFieldName("arguments")
	if args == nil {
		return nil
	}
	for i, _nc := 0, int(args.NamedChildCount()); i < _nc; i++ {
		obj := args.NamedChild(i)
		if obj == nil || obj.Type() != "object" {
			continue
		}
		for j, _nc := 0, int(obj.NamedChildCount()); j < _nc; j++ {
			pair := obj.NamedChild(j)
			if pair == nil || pair.Type() != "pair" {
				continue
			}
			if rtkPairKey(pair, src) != "endpoints" {
				continue
			}
			val := pair.ChildByFieldName("value")
			if val == nil {
				return nil
			}
			switch val.Type() {
			case "arrow_function", "function_expression", "function":
				return val
			}
		}
	}
	return nil
}

// rtkReturnedObject returns the object literal a builder arrow/function
// returns — `(b) => ({...})`, `(b) => {...}` (parenthesized), or
// `(b) => { return {...} }`.
func rtkReturnedObject(fn *sitter.Node) *sitter.Node {
	body := fn.ChildByFieldName("body")
	if body == nil {
		return nil
	}
	return rtkUnwrapObject(body)
}

func rtkUnwrapObject(n *sitter.Node) *sitter.Node {
	if n == nil {
		return nil
	}
	switch n.Type() {
	case "object":
		return n
	case "parenthesized_expression":
		for i, _nc := 0, int(n.NamedChildCount()); i < _nc; i++ {
			if o := rtkUnwrapObject(n.NamedChild(i)); o != nil {
				return o
			}
		}
	case "statement_block":
		for i, _nc := 0, int(n.NamedChildCount()); i < _nc; i++ {
			c := n.NamedChild(i)
			if c != nil && c.Type() == "return_statement" {
				for j, _nc := 0, int(c.NamedChildCount()); j < _nc; j++ {
					if o := rtkUnwrapObject(c.NamedChild(j)); o != nil {
						return o
					}
				}
			}
		}
	}
	return nil
}

// rtkPairKey returns the property name of an object pair, for both plain
// (`getUser:`) and string (`'getUser':`) keys.
func rtkPairKey(pair *sitter.Node, src []byte) string {
	key := pair.ChildByFieldName("key")
	if key == nil {
		return ""
	}
	switch key.Type() {
	case "property_identifier", "identifier":
		return key.Content(src)
	case "string":
		return jsStringLiteralContent(key, src)
	}
	return ""
}

// rtkPascal upper-cases the first byte of an identifier (ASCII endpoint
// names): getUser → GetUser.
func rtkPascal(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// rtkWalk visits n and all its named descendants.
func rtkWalk(n *sitter.Node, fn func(*sitter.Node)) {
	if n == nil {
		return
	}
	fn(n)
	for i, _nc := 0, int(n.NamedChildCount()); i < _nc; i++ {
		rtkWalk(n.NamedChild(i), fn)
	}
}
