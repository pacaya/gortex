package languages

import (
	"strconv"
	"strings"

	sitter "github.com/zzet/gortex/internal/parser/tsitter"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// emitGoFunctionShape emits the per-function structural detail that
// the coverage layer surfaces as queryable graph: parameters, return
// types, type parameters, and inline closures. The function-shape
// domain has a strip pass downstream (Indexer.applyCoverageDomains)
// that drops these when CoverageConfig.FunctionShape is disabled, so
// the extractor always emits.
//
// ownerID is the function/method node ID (e.g. "pkg/foo.go::Run" or
// "pkg/foo.go::Server.Handle"). defNode is the *_declaration AST
// node. paramsCap / resultCap are the named-capture results for
// `func.params`/`method.params` and `func.result`/`method.result`.
// declLine is the 1-based line of the declaration, used as the
// anchor for nodes/edges that don't have a finer-grained AST
// position to reference.
func emitGoFunctionShape(ownerID string, defNode *sitter.Node, paramsCap, resultCap *parser.CapturedNode, src []byte, filePath string, declLine int, result *parser.ExtractionResult) {
	if defNode == nil {
		return
	}
	emitGoParamNodes(ownerID, paramsCap, src, filePath, declLine, result)
	emitGoReturnEdges(ownerID, resultCap, src, filePath, declLine, result)
	emitGoGenericParamNodes(ownerID, defNode, src, filePath, declLine, result)
	if body := goFuncBody(defNode); body != nil {
		emitGoClosureNodes(ownerID, body, src, filePath, result)
		emitGoChannelOps(ownerID, body, src, filePath, result)
	}
}

// emitGoChannelOps walks a function body and emits EdgeSends /
// EdgeRecvs edges from the enclosing function to the channel
// variable for each `ch <- v` send statement and `<-ch` receive
// expression. Channel names resolve through the existing
// unresolved-target convention so the resolver can later patch
// them to the variable's actual node when in-scope.
//
// v1 limitations:
//
//   - Receives inside larger expressions (`x := <-ch` is fine,
//     but `f(<-ch + 1)` only flags the immediate `<-ch` operand).
//   - Range-over-channel (`for v := range ch`) doesn't currently
//     emit a recv edge. The grammar wraps it in for_statement
//     rather than unary_expression.
//   - `select` statement cases are walked normally (their bodies
//     contain send_statement / unary_expression children).
//   - Closure bodies are skipped — closures are walked separately
//     by emitGoClosureNodes; their channel ops attribute to the
//     closure node when re-attribution lands as a follow-up.
//     Today they attribute to the enclosing function, matching
//     the same v1 limitation as call edges in closures.
func emitGoChannelOps(ownerID string, body *sitter.Node, src []byte, filePath string, result *parser.ExtractionResult) {
	if body == nil {
		return
	}
	walkGoNodes(body, func(n *sitter.Node) bool {
		switch n.Type() {
		case "func_literal":
			// Don't recurse into nested closures — handled
			// elsewhere. Same convention as emitGoClosureNodes.
			return false
		case "send_statement":
			channel := n.ChildByFieldName("channel")
			if channel != nil {
				name := strings.TrimSpace(channel.Content(src))
				if name != "" {
					result.Edges = append(result.Edges, &graph.Edge{
						From:     ownerID,
						To:       "unresolved::" + name,
						Kind:     graph.EdgeSends,
						FilePath: filePath,
						Line:     int(n.StartPoint().Row) + 1,
						Origin:   graph.OriginASTInferred,
					})
				}
			}
		case "unary_expression":
			// Receive operations have operator "<-" and an
			// operand pointing at the channel.
			op := n.ChildByFieldName("operator")
			if op == nil || op.Content(src) != "<-" {
				return true
			}
			operand := n.ChildByFieldName("operand")
			if operand != nil {
				name := strings.TrimSpace(operand.Content(src))
				if name != "" {
					result.Edges = append(result.Edges, &graph.Edge{
						From:     ownerID,
						To:       "unresolved::" + name,
						Kind:     graph.EdgeRecvs,
						FilePath: filePath,
						Line:     int(n.StartPoint().Row) + 1,
						Origin:   graph.OriginASTInferred,
					})
				}
			}
			// `for v := range ch` is the third receive shape in
			// Go but distinguishing channel-range from map-range
			// or slice-range needs type info we don't propagate
			// here. Emitting a recv edge for every range target
			// would over-fire on every map/slice iteration; the
			// alternative — name-pattern heuristics — has worse
			// precision than just leaving the gap. Tracked as a
			// v1 limitation; a future pass that threads
			// paramsByFunc into the channel walker can filter
			// range RHSes by chan-typed variables only.
		}
		return true
	})
}

// emitGoParamNodes walks a parameter_list and emits one KindParam
// per identifier. Multi-name parameter declarations like
// `(a, b int)` produce two param nodes that share a typed_as target.
// Variadic parameters carry meta.variadic=true on the param node.
// The blank identifier `_` is skipped. The line argument is the
// declaration's anchor line, kept for parity with the other
// helpers though the param's own start line wins where present.
func emitGoParamNodes(ownerID string, paramsCap *parser.CapturedNode, src []byte, filePath string, _ int, result *parser.ExtractionResult) {
	if paramsCap == nil || paramsCap.Node == nil {
		return
	}
	list := paramsCap.Node
	pos := 0
	for i := 0; i < int(list.NamedChildCount()); i++ {
		decl := list.NamedChild(i)
		if decl == nil {
			continue
		}
		t := decl.Type()
		isVariadic := t == "variadic_parameter_declaration"
		if t != "parameter_declaration" && !isVariadic {
			continue
		}
		typeNode := decl.ChildByFieldName("type")
		typeName := ""
		if typeNode != nil {
			typeName = canonicalizeGoTypeRef(typeNode.Content(src))
		}
		// One declaration may carry multiple identifier names sharing
		// a single type. Walk all identifier children, skipping the
		// type node itself.
		for j := 0; j < int(decl.NamedChildCount()); j++ {
			c := decl.NamedChild(j)
			if c == nil || c == typeNode {
				continue
			}
			if c.Type() != "identifier" {
				continue
			}
			name := c.Content(src)
			if name == "" || name == "_" {
				continue
			}
			paramID := goParamNodeID(ownerID, name, pos)
			pos++
			meta := map[string]any{
				"position": pos - 1,
			}
			if isVariadic {
				meta["variadic"] = true
			}
			if typeName != "" {
				meta["type"] = typeName
			}
			result.Nodes = append(result.Nodes, &graph.Node{
				ID:        paramID,
				Kind:      graph.KindParam,
				Name:      name,
				FilePath:  filePath,
				StartLine: int(c.StartPoint().Row) + 1,
				EndLine:   int(c.EndPoint().Row) + 1,
				Language:  "go",
				Meta:      meta,
			})
			result.Edges = append(result.Edges, &graph.Edge{
				From:     paramID,
				To:       ownerID,
				Kind:     graph.EdgeParamOf,
				FilePath: filePath,
				Line:     int(c.StartPoint().Row) + 1,
				Origin:   graph.OriginASTResolved,
			})
			if typeName != "" {
				result.Edges = append(result.Edges, &graph.Edge{
					From:     paramID,
					To:       "unresolved::" + typeName,
					Kind:     graph.EdgeTypedAs,
					FilePath: filePath,
					Line:     int(c.StartPoint().Row) + 1,
					Origin:   graph.OriginASTInferred,
				})
			}
		}
	}
}

// emitGoReturnEdges emits one EdgeReturns per declared return type.
// Multi-return signatures like `(int, error)` produce two edges,
// preserving order via meta.position. Resolution is left to the
// resolver (target is `unresolved::<typeName>`); the bare `error`
// interface gets the same external::error sentinel that EdgeThrows
// uses so reverse walks share a single landing point.
func emitGoReturnEdges(ownerID string, resultCap *parser.CapturedNode, src []byte, filePath string, line int, result *parser.ExtractionResult) {
	if resultCap == nil || resultCap.Node == nil {
		return
	}
	types := splitGoReturnTypes(resultCap.Node, src)
	for i, t := range types {
		t = canonicalizeGoTypeRef(t)
		if t == "" {
			continue
		}
		target := "unresolved::" + t
		if t == "error" {
			target = "external::error"
		}
		result.Edges = append(result.Edges, &graph.Edge{
			From:     ownerID,
			To:       target,
			Kind:     graph.EdgeReturns,
			FilePath: filePath,
			Line:     line,
			Origin:   graph.OriginASTInferred,
			Meta: map[string]any{
				"position": i,
			},
		})
	}
}

// splitGoReturnTypes returns the declared return types in source
// order. Two AST shapes occur: a `parameter_list` parent (when the
// signature wraps results in parens) holding zero or more
// parameter_declaration children, or a bare type node (single
// unparenthesised result). Anonymous results — common in Go — are
// emitted as their type with no associated parameter name.
func splitGoReturnTypes(node *sitter.Node, src []byte) []string {
	if node == nil {
		return nil
	}
	if node.Type() != "parameter_list" {
		return []string{strings.TrimSpace(node.Content(src))}
	}
	var out []string
	for i := 0; i < int(node.NamedChildCount()); i++ {
		decl := node.NamedChild(i)
		if decl == nil {
			continue
		}
		switch decl.Type() {
		case "parameter_declaration", "variadic_parameter_declaration":
			if tn := decl.ChildByFieldName("type"); tn != nil {
				// Multi-name declarations duplicate the type once per name.
				names := 0
				for j := 0; j < int(decl.NamedChildCount()); j++ {
					c := decl.NamedChild(j)
					if c == nil || c == tn {
						continue
					}
					if c.Type() == "identifier" {
						names++
					}
				}
				if names == 0 {
					names = 1
				}
				typeText := strings.TrimSpace(tn.Content(src))
				for n := 0; n < names; n++ {
					out = append(out, typeText)
				}
			}
		default:
			// Bare type node nested under parameter_list (rare but
			// the grammar permits it for unnamed single results).
			out = append(out, strings.TrimSpace(decl.Content(src)))
		}
	}
	return out
}

// emitGoGenericParamNodes turns a function/method declaration's
// type_parameters into KindGenericParam nodes with EdgeMemberOf
// pointing at the owner. Bound types are stored as meta.bound so
// queries can filter by constraint.
func emitGoGenericParamNodes(ownerID string, defNode *sitter.Node, src []byte, filePath string, line int, result *parser.ExtractionResult) {
	tparams := goTypeParams(defNode, src)
	if len(tparams) == 0 {
		return
	}
	for _, tp := range tparams {
		name := tp["name"]
		if name == "" {
			continue
		}
		gpID := ownerID + "#tparam:" + name
		meta := map[string]any{}
		if b := tp["bound"]; b != "" {
			meta["bound"] = b
		}
		result.Nodes = append(result.Nodes, &graph.Node{
			ID:        gpID,
			Kind:      graph.KindGenericParam,
			Name:      name,
			FilePath:  filePath,
			StartLine: line,
			EndLine:   line,
			Language:  "go",
			Meta:      meta,
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From:     gpID,
			To:       ownerID,
			Kind:     graph.EdgeMemberOf,
			FilePath: filePath,
			Line:     line,
			Origin:   graph.OriginASTResolved,
		})
	}
}

// emitGoClosureNodes walks a function/method body looking for
// func_literal nodes (Go's anonymous-function syntax) and emits a
// KindClosure for each one. EdgeMemberOf links the closure back to
// the enclosing function so blast-radius walks reach it.
//
// v1 limitation: call edges inside a closure still attribute to the
// enclosing function. Re-attributing them would require teaching
// the call-emit walker to recognise closure boundaries — tracked as
// a Phase 1.5 follow-up.
func emitGoClosureNodes(ownerID string, body *sitter.Node, src []byte, filePath string, result *parser.ExtractionResult) {
	if body == nil {
		return
	}
	idx := 0
	walkGoNodes(body, func(n *sitter.Node) bool {
		if n.Type() != "func_literal" {
			return true
		}
		startLine := int(n.StartPoint().Row) + 1
		closureID := ownerID + "#closure@" + strconv.Itoa(startLine)
		// If two anonymous functions start on the same line, append a
		// stable suffix so IDs stay unique. Rare in practice but
		// defensive.
		if idx > 0 {
			closureID += "#" + strconv.Itoa(idx)
		}
		idx++
		result.Nodes = append(result.Nodes, &graph.Node{
			ID:        closureID,
			Kind:      graph.KindClosure,
			Name:      "closure@" + strconv.Itoa(startLine),
			FilePath:  filePath,
			StartLine: startLine,
			EndLine:   int(n.EndPoint().Row) + 1,
			Language:  "go",
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From:     closureID,
			To:       ownerID,
			Kind:     graph.EdgeMemberOf,
			FilePath: filePath,
			Line:     startLine,
			Origin:   graph.OriginASTResolved,
		})
		// `go func() {...}()` — the closure is launched as a
		// goroutine. Emit an EdgeSpawns from the enclosing function
		// to the closure, mirroring how named-call spawns produce
		// EdgeSpawns to the called function. Without this, agents
		// asking "what goroutines does Run launch?" miss the entire
		// inline-closure pattern.
		if isGoroutineSpawnedClosure(n) {
			result.Edges = append(result.Edges, &graph.Edge{
				From:     ownerID,
				To:       closureID,
				Kind:     graph.EdgeSpawns,
				FilePath: filePath,
				Line:     startLine,
				Origin:   graph.OriginASTResolved,
				Meta: map[string]any{
					"mode": "goroutine",
				},
			})
		}
		// Don't recurse into nested func_literals — they belong to
		// the inner closure, not the outer one. The outer walker will
		// pick them up when (if) closures-within-closures are
		// supported. For Phase 1 the flat enumeration is sufficient.
		return false
	})
}

// isGoroutineSpawnedClosure reports whether a func_literal node is
// the operand of an immediately-invoked call inside a go_statement —
// i.e. the `func() {...}` in `go func() {...}()`. The Go grammar
// shape is go_statement → call_expression → func_literal, so two
// Parent() hops are sufficient.
func isGoroutineSpawnedClosure(funcLit *sitter.Node) bool {
	if funcLit == nil {
		return false
	}
	call := funcLit.Parent()
	if call == nil || call.Type() != "call_expression" {
		return false
	}
	stmt := call.Parent()
	if stmt == nil {
		return false
	}
	return stmt.Type() == "go_statement"
}

// walkGoNodes is a small DFS helper that calls visit on each node
// and recurses into named children when visit returns true.
func walkGoNodes(node *sitter.Node, visit func(*sitter.Node) bool) {
	if node == nil {
		return
	}
	if !visit(node) {
		return
	}
	for i := 0; i < int(node.NamedChildCount()); i++ {
		walkGoNodes(node.NamedChild(i), visit)
	}
}

// isGoroutineSpawn reports whether a call_expression node is the
// direct child of a go_statement, meaning the call launches a
// goroutine rather than executing synchronously. The check is a
// single Parent() hop — Go's grammar wraps `go f()` as
// `go_statement -> call_expression`, so deeper walks are unnecessary.
func isGoroutineSpawn(callExpr *sitter.Node) bool {
	if callExpr == nil {
		return false
	}
	parent := callExpr.Parent()
	if parent == nil {
		return false
	}
	return parent.Type() == "go_statement"
}

// emitGoSpawnEdge appends an EdgeSpawns from caller → target when
// the underlying call was launched via `go`. Emitted in addition to
// EdgeCalls so synchronous-reachability queries can scope by edge
// kind (drop spawns) while concurrency analyses can see both. Meta
// records mode=goroutine so downstream consumers can distinguish
// from future async/Promise spawn modes.
func emitGoSpawnEdge(c goDeferredCall, callerID, target, filePath string, result *parser.ExtractionResult) {
	if !c.spawn {
		return
	}
	result.Edges = append(result.Edges, &graph.Edge{
		From:     callerID,
		To:       target,
		Kind:     graph.EdgeSpawns,
		FilePath: filePath,
		Line:     c.line,
		Origin:   graph.OriginASTResolved,
		Meta: map[string]any{
			"mode": "goroutine",
		},
	})
}

// canonicalizeGoTypeRef returns a type-name string suitable for use
// as the target of a typed_as / returns edge. Unlike
// normalizeGoTypeName it preserves primitives — the agent-facing
// query "find me functions taking io.Reader" benefits from having
// the same shape for primitives ("find me functions returning int")
// even though no graph node exists for the primitive itself; the
// string serves as a stable, searchable target.
//
// Strips: leading whitespace, slice/array prefix, pointer prefix,
// generic-instantiation suffix, package qualifier.
// Returns "" for map/chan/func/struct/interface anonymous types and
// for empty input.
func canonicalizeGoTypeRef(t string) string {
	t = strings.TrimSpace(t)
	if t == "" {
		return ""
	}
	t = strings.TrimPrefix(t, "[]")
	if strings.HasPrefix(t, "[") {
		if end := strings.Index(t, "]"); end >= 0 {
			t = t[end+1:]
		}
	}
	if strings.HasPrefix(t, "map[") ||
		strings.HasPrefix(t, "chan ") ||
		strings.HasPrefix(t, "func(") ||
		strings.HasPrefix(t, "struct{") ||
		strings.HasPrefix(t, "interface{") {
		return ""
	}
	t = strings.TrimPrefix(t, "*")
	if i := strings.LastIndex(t, "."); i >= 0 {
		t = t[i+1:]
	}
	if i := strings.Index(t, "["); i >= 0 {
		t = t[:i]
	}
	return strings.TrimSpace(t)
}

// goParamNodeID is the canonical ID convention for a Go parameter
// node: `<owner-id>#param:<name>`. Duplicate parameter names are
// already filtered (we skip `_`), so a position-disambiguating
// suffix isn't needed in the common case. The pos argument is kept
// in the signature for symmetry with future languages where
// duplicate names are legal.
func goParamNodeID(ownerID, name string, _ int) string {
	return ownerID + "#param:" + name
}
