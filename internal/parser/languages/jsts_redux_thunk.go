package languages

import (
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// Redux Toolkit createAsyncThunk dispatch-chain detection. A thunk's
// payload-creator body dispatches other actions/thunks
// (`dispatch(setLoading())`); those indirect calls are invisible to the
// static call graph because the thunk is registered, not directly called.
// This pass tags the thunk node with Meta["redux_thunk"] and stamps a
// placeholder EdgeCalls per inner dispatch, which the resolver's
// ResolveReduxThunkCalls binds to the action/thunk node. Shared by the
// JavaScript and TypeScript extractors.

// reduxThunkFactories are the createAsyncThunk-family callees whose return
// value is a thunk, matched on the callee's last identifier.
var reduxThunkFactories = map[string]bool{
	"createAsyncThunk": true,
	"createThunk":      true,
}

// reduxThunkViaTag must match resolver.reduxThunkVia — the languages
// package does not import resolver, so the two agree on the via tag by
// value.
const reduxThunkViaTag = "redux-thunk"

// reduxThunkDispatchCap bounds the inner dispatches mined from one thunk
// body so a pathological payload creator does not fan out unbounded.
const reduxThunkDispatchCap = 24

// captureReduxThunkDispatches finds `const X = createAsyncThunk(type,
// (arg, {dispatch}) => { dispatch(Y()); ... })` declarators, tags X's
// node with Meta["redux_thunk"], and stamps a placeholder EdgeCalls from X
// to each dispatched action Y. Runs at the tail of Extract, after all
// nodes exist, so it can find and tag the thunk's own node.
func captureReduxThunkDispatches(result *parser.ExtractionResult, root *sitter.Node, filePath string, src []byte) {
	if root == nil || result == nil {
		return
	}
	reduxThunkWalk(root, func(n *sitter.Node) {
		if n.Type() != "variable_declarator" {
			return
		}
		nameNode := n.ChildByFieldName("name")
		if nameNode == nil || nameNode.Type() != "identifier" {
			return
		}
		init := n.ChildByFieldName("value")
		if init == nil || init.Type() != "call_expression" {
			return
		}
		if !reduxThunkFactories[jsCalleeLastName(init.ChildByFieldName("function"), src)] {
			return
		}
		creator := reduxThunkPayloadCreator(init)
		if creator == nil {
			return
		}
		thunkID := reduxThunkTag(result, filePath, nameNode.Content(src))

		emitted := 0
		reduxThunkWalk(creator, func(c *sitter.Node) {
			if emitted >= reduxThunkDispatchCap || c.Type() != "call_expression" {
				return
			}
			if jsCalleeLastName(c.ChildByFieldName("function"), src) != "dispatch" {
				return
			}
			inner := reduxThunkFirstCallArg(c)
			if inner == nil {
				return
			}
			action := jsCalleeLastName(inner.ChildByFieldName("function"), src)
			if action == "" {
				return
			}
			result.Edges = append(result.Edges, &graph.Edge{
				From:     thunkID,
				To:       "unresolved::*." + action,
				Kind:     graph.EdgeCalls,
				FilePath: filePath,
				Line:     int(c.StartPoint().Row) + 1,
				Meta: map[string]any{
					"via":            reduxThunkViaTag,
					"thunk_dispatch": action,
				},
			})
			emitted++
		})
	})
}

// reduxThunkTag stamps Meta["redux_thunk"] on the thunk const's node and
// returns its ID. It prefers the exact-ID node (filePath::name), falls
// back to a same-named variable/constant/function node when the ID was
// disambiguated, and mints a function node as a last resort so the
// placeholder edge's From is always a real node.
func reduxThunkTag(result *parser.ExtractionResult, filePath, name string) string {
	want := filePath + "::" + name
	var fallback *graph.Node
	for _, nd := range result.Nodes {
		if nd == nil {
			continue
		}
		if nd.ID == want {
			reduxStampThunk(nd, name)
			return nd.ID
		}
		if fallback == nil && nd.Name == name &&
			(nd.Kind == graph.KindVariable || nd.Kind == graph.KindConstant || nd.Kind == graph.KindFunction) {
			fallback = nd
		}
	}
	if fallback != nil {
		reduxStampThunk(fallback, name)
		return fallback.ID
	}
	node := &graph.Node{ID: want, Kind: graph.KindFunction, Name: name, FilePath: filePath, Meta: map[string]any{"redux_thunk": name}}
	result.Nodes = append(result.Nodes, node)
	return want
}

func reduxStampThunk(n *graph.Node, name string) {
	if n.Meta == nil {
		n.Meta = map[string]any{}
	}
	n.Meta["redux_thunk"] = name
}

// reduxThunkPayloadCreator returns the first function argument of a
// createAsyncThunk call — the payload creator that holds the dispatch
// chain (the string type prefix is argument 0).
func reduxThunkPayloadCreator(call *sitter.Node) *sitter.Node {
	args := call.ChildByFieldName("arguments")
	if args == nil {
		return nil
	}
	for i, _nc := 0, int(args.NamedChildCount()); i < _nc; i++ {
		c := args.NamedChild(i)
		if c == nil {
			continue
		}
		switch c.Type() {
		case "arrow_function", "function_expression", "function", "function_declaration":
			return c
		}
	}
	return nil
}

// reduxThunkFirstCallArg returns the first argument of a call when it is
// itself a call expression — the dispatched action creator invocation in
// `dispatch(setLoading())`.
func reduxThunkFirstCallArg(call *sitter.Node) *sitter.Node {
	args := call.ChildByFieldName("arguments")
	if args == nil || args.NamedChildCount() == 0 {
		return nil
	}
	first := args.NamedChild(0)
	if first != nil && first.Type() == "call_expression" {
		return first
	}
	return nil
}

// reduxThunkWalk visits n and all its named descendants.
func reduxThunkWalk(n *sitter.Node, fn func(*sitter.Node)) {
	if n == nil {
		return
	}
	fn(n)
	for i, _nc := 0, int(n.NamedChildCount()); i < _nc; i++ {
		reduxThunkWalk(n.NamedChild(i), fn)
	}
}
