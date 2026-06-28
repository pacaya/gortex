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
func emitGoFunctionShape(ownerID string, defNode *sitter.Node, paramsCap, resultCap *parser.CapturedNode, src []byte, filePath string, declLine int, imports map[string]string, result *parser.ExtractionResult) {
	if defNode == nil {
		return
	}
	emitGoParamNodes(ownerID, paramsCap, src, filePath, declLine, result)
	emitGoReturnEdges(ownerID, resultCap, src, filePath, declLine, result)
	emitGoGenericParamNodes(ownerID, defNode, src, filePath, declLine, result)
	if body := goFuncBody(defNode); body != nil {
		emitGoClosureNodes(ownerID, declLine, body, src, filePath, result)
		emitGoChannelOps(ownerID, body, src, filePath, result)
		// CPG-lite intra-procedural dataflow: emits EdgeValueFlow,
		// EdgeArgOf, and EdgeReturnsTo placeholders. Inter-procedural
		// targets are lifted by the indexer's
		// MaterializeDataflowParams pass once the call resolver
		// has landed every callee. declLine anchors local-binding
		// IDs as offsets so edits above the function don't churn
		// every binding inside. imports are the file's package
		// aliases so selector-expression cases inside the walker
		// can rewrite `pkg.Method` calls to the proper
		// `unresolved::extern::<importPath>::<Method>` shape
		// instead of dropping the qualifier.
		paramsByName := goParamNamesFromCapture(paramsCap, src)
		emitGoDataflow(ownerID, declLine, body, paramsByName, imports, src, filePath, result)
	}
}

// goParamNamesFromCapture returns a name → declared-type map for
// the parameters captured by the function/method shape query. The
// type isn't load-bearing for dataflow today (only the name set is)
// but is kept on the map so future improvements can use it for
// argument-binding precision without changing the call signature.
func goParamNamesFromCapture(paramsCap *parser.CapturedNode, src []byte) map[string]string {
	out := map[string]string{}
	if paramsCap == nil || paramsCap.Node == nil {
		return out
	}
	list := paramsCap.Node
	for i, _nc := 0, int(list.NamedChildCount()); i < _nc; i++ {
		decl := list.NamedChild(i)
		if decl == nil {
			continue
		}
		t := decl.Type()
		if t != "parameter_declaration" && t != "variadic_parameter_declaration" {
			continue
		}
		typeNode := decl.ChildByFieldName("type")
		typeText := ""
		if typeNode != nil {
			typeText = strings.TrimSpace(typeNode.Content(src))
		}
		for j, _nc := 0, int(decl.NamedChildCount()); j < _nc; j++ {
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
			out[name] = typeText
		}
	}
	return out
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
	for i, _nc := 0, int(list.NamedChildCount()); i < _nc; i++ {
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
		for j, _nc := 0, int(decl.NamedChildCount()); j < _nc; j++ {
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
	for i, _nc := 0, int(node.NamedChildCount()); i < _nc; i++ {
		decl := node.NamedChild(i)
		if decl == nil {
			continue
		}
		switch decl.Type() {
		case "parameter_declaration", "variadic_parameter_declaration":
			if tn := decl.ChildByFieldName("type"); tn != nil {
				// Multi-name declarations duplicate the type once per name.
				names := 0
				for j, _nc := 0, int(decl.NamedChildCount()); j < _nc; j++ {
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
func emitGoClosureNodes(ownerID string, ownerStartLine int, body *sitter.Node, src []byte, filePath string, result *parser.ExtractionResult) {
	if body == nil {
		return
	}
	idx := 0
	walkGoNodes(body, func(n *sitter.Node) bool {
		if n.Type() != "func_literal" {
			return true
		}
		startLine := int(n.StartPoint().Row) + 1
		// ID anchors on the owner-relative offset (+ prefix) so edits
		// above the enclosing function don't churn the closure's ID.
		// Name keeps the absolute line for human readability in search
		// results / outlines.
		offset := startLine
		if ownerStartLine > 0 {
			offset = startLine - ownerStartLine + 1
		}
		closureID := ownerID + "#closure@+" + strconv.Itoa(offset)
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
		// EdgeCaptures: every bare identifier the closure references
		// without locally declaring/binding is a capture of an outer
		// scope binding. The resolver later lands the unresolved::
		// targets on the actual variable / function node.
		emitGoClosureCaptures(closureID, n, src, filePath, result)
		// Don't recurse into nested func_literals — they belong to
		// the inner closure, not the outer one. The outer walker will
		// pick them up when (if) closures-within-closures are
		// supported. For Phase 1 the flat enumeration is sufficient.
		return false
	})
}

// emitGoClosureCaptures walks a func_literal and emits one
// EdgeCaptures per free variable. "Free" means: the identifier is
// used as a value somewhere in the closure body but isn't bound by
// a parameter, short-var-decl, var-spec, or const-spec inside the
// closure. Locally re-declared shadowing names suppress the capture
// (matches Go scoping rules).
//
// v1 limitations:
//   - We don't recurse into nested closures; a nested closure's
//     captures emit against the nested closure node when its own
//     emitGoClosureNodes pass runs.
//   - Selector RHS (`x.field`) only captures the operand `x`; the
//     `.field` part is a field reference, not a free variable.
//   - Identifiers in type position (e.g. `MyType` in `var x MyType`)
//     count as captures — Go closures do close over file-scope type
//     names, and the resolver can land them on the type node.
func emitGoClosureCaptures(closureID string, funcLit *sitter.Node, src []byte, filePath string, result *parser.ExtractionResult) {
	if funcLit == nil {
		return
	}
	body := funcLit.ChildByFieldName("body")
	if body == nil {
		return
	}
	locals := map[string]bool{}
	collectGoClosureLocals(funcLit, src, locals)

	seen := map[string]bool{}
	walkGoNodes(body, func(n *sitter.Node) bool {
		// Skip nested closures — they own their own captures.
		if n.Type() == "func_literal" {
			return false
		}
		if n.Type() != "identifier" {
			return true
		}
		// Filter out identifiers that aren't actually a value-side
		// reference: the LHS of a short-var-decl, the name on a var
		// or const spec, the parameter name in a function decl, the
		// field portion of a selector — none of these are captures.
		if !isGoClosureValueRef(n) {
			return true
		}
		name := n.Content(src)
		if name == "" || name == "_" || locals[name] || isGoBuiltinOrKeyword(name) {
			return true
		}
		if seen[name] {
			return true
		}
		seen[name] = true
		result.Edges = append(result.Edges, &graph.Edge{
			From:     closureID,
			To:       "unresolved::" + name,
			Kind:     graph.EdgeCaptures,
			FilePath: filePath,
			Line:     int(n.StartPoint().Row) + 1,
			Origin:   graph.OriginASTInferred,
			Meta: map[string]any{
				"name": name,
			},
		})
		return true
	})
}

// collectGoClosureLocals records every name declared inside the
// closure (parameters, return-named results, var/const/short-var
// decls). Mutates locals in place.
func collectGoClosureLocals(funcLit *sitter.Node, src []byte, locals map[string]bool) {
	if funcLit == nil {
		return
	}
	addIdentNames := func(root *sitter.Node) {
		if root == nil {
			return
		}
		walkGoNodes(root, func(n *sitter.Node) bool {
			if n.Type() == "func_literal" {
				return false
			}
			if n.Type() == "identifier" {
				locals[n.Content(src)] = true
			}
			return true
		})
	}
	if params := funcLit.ChildByFieldName("parameters"); params != nil {
		// Parameter names live under parameter_declaration → name.
		walkGoNodes(params, func(n *sitter.Node) bool {
			if n.Type() == "func_literal" {
				return false
			}
			if n.Type() == "parameter_declaration" {
				if name := n.ChildByFieldName("name"); name != nil {
					addIdentNames(name)
				}
			}
			return true
		})
	}
	if res := funcLit.ChildByFieldName("result"); res != nil {
		walkGoNodes(res, func(n *sitter.Node) bool {
			if n.Type() == "func_literal" {
				return false
			}
			if n.Type() == "parameter_declaration" {
				if name := n.ChildByFieldName("name"); name != nil {
					addIdentNames(name)
				}
			}
			return true
		})
	}
	body := funcLit.ChildByFieldName("body")
	if body == nil {
		return
	}
	walkGoNodes(body, func(n *sitter.Node) bool {
		if n.Type() == "func_literal" {
			return false
		}
		switch n.Type() {
		case "short_var_declaration":
			if left := n.ChildByFieldName("left"); left != nil {
				addIdentNames(left)
			}
		case "var_spec", "const_spec":
			if name := n.ChildByFieldName("name"); name != nil {
				addIdentNames(name)
			}
		case "range_clause":
			// `for k, v := range x { … }` — k, v are loop locals.
			if left := n.ChildByFieldName("left"); left != nil {
				addIdentNames(left)
			}
		case "for_statement":
			// Init clause locals (not common in Go but possible).
			if init := n.ChildByFieldName("initializer"); init != nil {
				addIdentNames(init)
			}
		case "type_switch_statement":
			// `switch v := x.(type)` — v is a local.
			walkGoNodes(n, func(c *sitter.Node) bool {
				if c.Type() == "expression_list" {
					addIdentNames(c)
					return false
				}
				return true
			})
		}
		return true
	})
}

// isGoClosureValueRef reports whether an identifier node is used as
// a value-side reference rather than a binding-site declaration.
// Returns false for the LHS of a short-var-decl, parameter names,
// variable/const declaration names, and the .field portion of a
// selector expression.
func isGoClosureValueRef(n *sitter.Node) bool {
	parent := n.Parent()
	if parent == nil {
		return true
	}
	switch parent.Type() {
	case "selector_expression":
		// We capture the operand (LHS), not the field name (RHS).
		if field := parent.ChildByFieldName("field"); field != nil && field.Equal(n) {
			return false
		}
	case "parameter_declaration":
		if name := parent.ChildByFieldName("name"); name != nil {
			declared := false
			walkGoNodes(name, func(c *sitter.Node) bool {
				if c.Equal(n) {
					declared = true
					return false
				}
				return true
			})
			if declared {
				return false
			}
		}
	case "var_spec", "const_spec":
		if name := parent.ChildByFieldName("name"); name != nil {
			declared := false
			walkGoNodes(name, func(c *sitter.Node) bool {
				if c.Equal(n) {
					declared = true
					return false
				}
				return true
			})
			if declared {
				return false
			}
		}
	case "short_var_declaration":
		if left := parent.ChildByFieldName("left"); left != nil {
			declared := false
			walkGoNodes(left, func(c *sitter.Node) bool {
				if c.Equal(n) {
					declared = true
					return false
				}
				return true
			})
			if declared {
				return false
			}
		}
	case "range_clause":
		if left := parent.ChildByFieldName("left"); left != nil {
			declared := false
			walkGoNodes(left, func(c *sitter.Node) bool {
				if c.Equal(n) {
					declared = true
					return false
				}
				return true
			})
			if declared {
				return false
			}
		}
	case "function_declaration", "method_declaration":
		// The function/method's own name isn't a free var inside
		// its closure children — but closures can't be at this
		// scope, so this is mostly defensive.
		return false
	case "field_identifier":
		return false
	}
	return true
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
	for i, _nc := 0, int(node.NamedChildCount()); i < _nc; i++ {
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
