package languages

import (
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// captureReactContextRefs emits a placeholder reference edge from a component
// to the context object it reads via `useContext(SomeContext)`. The TS/JS
// extractor records the `useContext` callee but drops the context-object
// argument, so without this the React resolver has nothing to bind a
// `useContext(AuthContext)` site to its /context/ definition. The edge is
// tagged `via=react_context` so the resolver treats it as a context reference
// regardless of the identifier's suffix. Runs at the tail of Extract.
func captureReactContextRefs(result *parser.ExtractionResult, root *sitter.Node, filePath string, src []byte) {
	if root == nil || result == nil {
		return
	}
	seen := map[string]bool{}
	expressWalk(root, func(c *sitter.Node) {
		if c.Type() != "call_expression" {
			return
		}
		fn := c.ChildByFieldName("function")
		if fn == nil || fn.Type() != "identifier" || fn.Content(src) != "useContext" {
			return
		}
		args := c.ChildByFieldName("arguments")
		if args == nil {
			return
		}
		ctx := reactFirstIdentArg(args, src)
		if ctx == "" {
			return
		}
		line := int(c.StartPoint().Row) + 1
		from := reactEnclosingFuncID(result.Nodes, line)
		if from == "" {
			return
		}
		key := from + "\x00" + ctx
		if seen[key] {
			return
		}
		seen[key] = true
		result.Edges = append(result.Edges, &graph.Edge{
			From: from, To: "unresolved::" + ctx, Kind: graph.EdgeReferences,
			FilePath: filePath, Line: line, Origin: graph.OriginASTInferred,
			Meta: map[string]any{"via": "react_context", "context_name": ctx},
		})
	})
}

// reactFirstIdentArg returns the first call argument when it is a bare
// identifier (the context object passed to useContext), else "".
func reactFirstIdentArg(args *sitter.Node, src []byte) string {
	for i, _nc := 0, int(args.NamedChildCount()); i < _nc; i++ {
		a := args.NamedChild(i)
		if a == nil {
			continue
		}
		if a.Type() == "identifier" {
			if name := a.Content(src); isSimpleJSIdent(name) {
				return name
			}
		}
		return "" // first arg isn't a plain identifier (e.g. a member expression)
	}
	return ""
}

// reactEnclosingFuncID returns the ID of the innermost function/method node
// whose line range contains line, or "" when none does.
func reactEnclosingFuncID(nodes []*graph.Node, line int) string {
	best := ""
	bestSpan := 1 << 30
	for _, n := range nodes {
		if n == nil {
			continue
		}
		if n.Kind != graph.KindFunction && n.Kind != graph.KindMethod {
			continue
		}
		if n.StartLine <= line && line <= n.EndLine {
			if span := n.EndLine - n.StartLine; span < bestSpan {
				bestSpan = span
				best = n.ID
			}
		}
	}
	return best
}
