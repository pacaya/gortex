package languages

import (
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// NgRx effects dispatch detection. An effect declares the action(s) it reacts
// to via `createEffect(() => this.actions$.pipe(ofType(SomeAction), ...))`. The
// effect is registered with the EffectsModule, not called, so the static call
// graph never links it to the action it handles. This pass tags the effect node
// with Meta["ngrx_effect"] and stamps a placeholder EdgeCalls per ofType action,
// which the resolver's ResolveNgRxEffects binds to the action node. Shared by
// the JavaScript and TypeScript extractors.

// ngrxEffectViaTag must match resolver.ngrxEffectVia -- the languages package
// does not import resolver, so the two agree on the via tag by value.
const ngrxEffectViaTag = "ngrx-effect"

// ngrxEffectActionCap bounds the ofType actions mined from one effect.
const ngrxEffectActionCap = 16

// captureNgRxEffects finds `name$ = createEffect(() => actions$.pipe(
// ofType(Action), ...))` class properties (and `const name$ = createEffect(...)`
// functional effects), tags the effect node with Meta["ngrx_effect"], and stamps
// a placeholder EdgeCalls from it to each ofType action. Runs at the tail of
// Extract, after all nodes exist, so it can find and tag the effect node.
func captureNgRxEffects(result *parser.ExtractionResult, root *sitter.Node, filePath string, src []byte) {
	if root == nil || result == nil {
		return
	}
	ngrxEffectWalk(root, func(n *sitter.Node) {
		var nameNode, init *sitter.Node
		switch n.Type() {
		case "public_field_definition", "field_definition", "variable_declarator":
			nameNode = n.ChildByFieldName("name")
			init = n.ChildByFieldName("value")
		default:
			return
		}
		if nameNode == nil || init == nil || init.Type() != "call_expression" {
			return
		}
		if jsCalleeLastName(init.ChildByFieldName("function"), src) != "createEffect" {
			return
		}
		name := nameNode.Content(src)
		if name == "" {
			return
		}
		var actions []string
		seen := map[string]bool{}
		ngrxEffectWalk(init, func(c *sitter.Node) {
			if len(actions) >= ngrxEffectActionCap || c.Type() != "call_expression" {
				return
			}
			if jsCalleeLastName(c.ChildByFieldName("function"), src) != "ofType" {
				return
			}
			args := c.ChildByFieldName("arguments")
			if args == nil {
				return
			}
			for i, _nc := 0, int(args.NamedChildCount()); i < _nc; i++ {
				act := ngrxActionName(args.NamedChild(i), src)
				if act == "" || seen[act] {
					continue
				}
				seen[act] = true
				actions = append(actions, act)
			}
		})
		if len(actions) == 0 {
			return
		}
		effectID := ngrxEffectTag(result, filePath, name)
		line := int(n.StartPoint().Row) + 1
		for _, act := range actions {
			result.Edges = append(result.Edges, &graph.Edge{
				From:     effectID,
				To:       "unresolved::*." + act,
				Kind:     graph.EdgeCalls,
				FilePath: filePath,
				Line:     line,
				Meta: map[string]any{
					"via":         ngrxEffectViaTag,
					"ngrx_action": act,
				},
			})
		}
	})
}

// ngrxActionName returns the action name an ofType argument references: a bare
// identifier (`LoadUsers`) or the trailing property of a member access
// (`UserActions.load` -> "load").
func ngrxActionName(n *sitter.Node, src []byte) string {
	if n == nil {
		return ""
	}
	switch n.Type() {
	case "identifier":
		return n.Content(src)
	case "member_expression":
		if p := n.ChildByFieldName("property"); p != nil {
			return p.Content(src)
		}
	}
	return ""
}

// ngrxEffectTag stamps Meta["ngrx_effect"] on the effect's node and returns its
// ID. It prefers the exact-ID node (a functional effect `const name$ = ...`),
// then a same-named class property / variable, and mints a node as a last resort
// so the placeholder edge's From is always real.
func ngrxEffectTag(result *parser.ExtractionResult, filePath, name string) string {
	want := filePath + "::" + name
	var fallback *graph.Node
	for _, nd := range result.Nodes {
		if nd == nil {
			continue
		}
		if nd.ID == want {
			ngrxStampEffect(nd, name)
			return nd.ID
		}
		if fallback == nil && nd.Name == name &&
			(nd.Kind == graph.KindVariable || nd.Kind == graph.KindConstant || nd.Kind == graph.KindField || nd.Kind == graph.KindFunction) {
			fallback = nd
		}
	}
	if fallback != nil {
		ngrxStampEffect(fallback, name)
		return fallback.ID
	}
	node := &graph.Node{ID: want, Kind: graph.KindVariable, Name: name, FilePath: filePath, Meta: map[string]any{"ngrx_effect": name}}
	result.Nodes = append(result.Nodes, node)
	return want
}

func ngrxStampEffect(n *graph.Node, name string) {
	if n.Meta == nil {
		n.Meta = map[string]any{}
	}
	n.Meta["ngrx_effect"] = name
}

// ngrxEffectWalk visits n and all its named descendants.
func ngrxEffectWalk(n *sitter.Node, fn func(*sitter.Node)) {
	if n == nil {
		return
	}
	fn(n)
	for i, _nc := 0, int(n.NamedChildCount()); i < _nc; i++ {
		ngrxEffectWalk(n.NamedChild(i), fn)
	}
}
