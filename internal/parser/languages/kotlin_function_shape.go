package languages

import (
	"strconv"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// emitKotlinAsyncSpawns walks a Kotlin function body for coroutine
// builders (`launch`, `async`, `runBlocking`, `withContext`) and for
// `.await()` postfix calls. Mode is "coroutine" for builders,
// "async" for await.
func emitKotlinAsyncSpawns(ownerID string, body *sitter.Node, src []byte, filePath string, result *parser.ExtractionResult) {
	if body == nil {
		return
	}
	seen := map[string]bool{}
	emit := func(target, mode string, line int) {
		if target == "" {
			return
		}
		key := mode + "\x00" + target
		if seen[key] {
			return
		}
		seen[key] = true
		result.Edges = append(result.Edges, &graph.Edge{
			From:     ownerID,
			To:       "unresolved::" + target,
			Kind:     graph.EdgeSpawns,
			FilePath: filePath,
			Line:     line,
			Origin:   graph.OriginASTInferred,
			Meta: map[string]any{
				"mode": mode,
			},
		})
	}
	walkKotlinNodes(body, func(n *sitter.Node) bool {
		switch n.Type() {
		case "function_declaration", "anonymous_function":
			// Don't descend into nested function bodies. Lambda
			// literals are intentionally walked: Kotlin
			// extractors don't materialise lambdas as graph
			// nodes, so calls inside `launch { compute() }`
			// belong to the enclosing function.
			return false
		case "call_expression":
			fn := n.ChildByFieldName("function")
			if fn == nil {
				// Some grammar shapes set the callee as the first
				// named child without the field name.
				if n.NamedChildCount() > 0 {
					fn = n.NamedChild(0)
				}
			}
			if fn == nil {
				return true
			}
			name := ""
			switch fn.Type() {
			case "simple_identifier":
				name = fn.Content(src)
			case "navigation_expression":
				// `obj.method` — pick the suffix selector.
				for i := int(fn.NamedChildCount()) - 1; i >= 0; i-- {
					c := fn.NamedChild(i)
					if c == nil {
						continue
					}
					if c.Type() == "navigation_suffix" {
						for j, _nc := 0, int(c.NamedChildCount()); j < _nc; j++ {
							cc := c.NamedChild(j)
							if cc != nil && cc.Type() == "simple_identifier" {
								name = cc.Content(src)
								break
							}
						}
						break
					}
				}
			}
			if name == "" {
				return true
			}
			line := int(n.StartPoint().Row) + 1
			switch name {
			case "launch", "async", "runBlocking", "withContext", "supervisorScope", "coroutineScope":
				emit(name, "coroutine", line)
			case "await":
				// `someDeferred.await()` — emit a generic await
				// edge; the target is just `await` since we have
				// no deeper resolution without type info.
				emit("await", "async", line)
			}
		}
		return true
	})
}

func walkKotlinNodes(n *sitter.Node, visit func(*sitter.Node) bool) {
	if n == nil {
		return
	}
	if !visit(n) {
		return
	}
	for i, _nc := 0, int(n.NamedChildCount()); i < _nc; i++ {
		walkKotlinNodes(n.NamedChild(i), visit)
	}
}

// emitKotlinGenericParamNodes emits KindGenericParam nodes plus
// EdgeMemberOf edges for the type_parameters of a Kotlin class /
// interface / function declaration.
func emitKotlinGenericParamNodes(ownerID string, decl *sitter.Node, src []byte, filePath string, line int, result *parser.ExtractionResult) {
	if decl == nil {
		return
	}
	tparams := decl.ChildByFieldName("type_parameters")
	if tparams == nil {
		for i, _nc := 0, int(decl.NamedChildCount()); i < _nc; i++ {
			c := decl.NamedChild(i)
			if c != nil && c.Type() == "type_parameters" {
				tparams = c
				break
			}
		}
	}
	if tparams == nil {
		return
	}
	for i, _nc := 0, int(tparams.NamedChildCount()); i < _nc; i++ {
		tp := tparams.NamedChild(i)
		if tp == nil || tp.Type() != "type_parameter" {
			continue
		}
		var name string
		for j, _nc := 0, int(tp.NamedChildCount()); j < _nc; j++ {
			c := tp.NamedChild(j)
			if c != nil && (c.Type() == "type_identifier" || c.Type() == "simple_identifier") && name == "" {
				name = c.Content(src)
				break
			}
		}
		if name == "" {
			continue
		}
		gpID := ownerID + "#tparam:" + name
		result.Nodes = append(result.Nodes, &graph.Node{
			ID:        gpID,
			Kind:      graph.KindGenericParam,
			Name:      name,
			FilePath:  filePath,
			StartLine: line,
			EndLine:   line,
			Language:  "kotlin",
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

// kotlinFunctionBody returns the body block of a Kotlin function
// declaration, or nil for abstract / expression-bodied functions
// without explicit braces (those don't have spawn-style calls in
// practice).
func kotlinFunctionBody(funcNode *sitter.Node) *sitter.Node {
	if funcNode == nil {
		return nil
	}
	if b := funcNode.ChildByFieldName("body"); b != nil {
		return b
	}
	for i, _nc := 0, int(funcNode.NamedChildCount()); i < _nc; i++ {
		c := funcNode.NamedChild(i)
		if c != nil && (c.Type() == "function_body" || c.Type() == "block") {
			return c
		}
	}
	return nil
}

// emitKotlinTypeUseEdges emits an EdgeTypedAs from ownerID to the bare
// named type that typeText references, so a tree-sitter-only build still
// links a Kotlin symbol to the type it is annotated with (parameter,
// return, local `val`/`var`, or property). typeText is the verbatim
// annotation source; it is normalized through normalizeKotlinTypeName,
// which strips the nullable `?` suffix and generic `<…>` arguments and
// drops Kotlin primitives (Int, String, Boolean, …) and lowercase names.
// No edge is emitted for an empty or primitive type. The target is
// `unresolved::<Type>`; the resolver lands it on a KindType / KindInterface
// of the same name. Mirrors emitTSTypeUseEdges / emitPyTypeUseEdges.
func emitKotlinTypeUseEdges(ownerID, typeText, filePath string, line int, result *parser.ExtractionResult) {
	if ownerID == "" {
		return
	}
	t := normalizeKotlinTypeName(typeText)
	if t == "" {
		return
	}
	result.Edges = append(result.Edges, &graph.Edge{
		From:     ownerID,
		To:       "unresolved::" + t,
		Kind:     graph.EdgeTypedAs,
		FilePath: filePath,
		Line:     line,
		Origin:   graph.OriginASTInferred,
	})
}

// kotlinParamsList returns the function_value_parameters child of a
// function_declaration, or nil when the function takes no parameters.
func kotlinParamsList(funcNode *sitter.Node) *sitter.Node {
	if funcNode == nil {
		return nil
	}
	for i, _nc := 0, int(funcNode.NamedChildCount()); i < _nc; i++ {
		c := funcNode.NamedChild(i)
		if c != nil && c.Type() == "function_value_parameters" {
			return c
		}
	}
	return nil
}

// emitKotlinParamShape materialises one KindParam node per typed/untyped
// parameter of a Kotlin function, plus an EdgeParamOf back to the owner
// and (when the parameter carries a type annotation) an EdgeTypedAs to
// the referenced type. Kotlin parameters are always `name: Type`, so the
// name is the parameter's simple_identifier and the type is its
// user_type / nullable_type child. Mirrors emitTSParamNodes /
// emitPyParamNodes.
func emitKotlinParamShape(ownerID string, funcNode *sitter.Node, src []byte, filePath string, declLine int, result *parser.ExtractionResult) {
	params := kotlinParamsList(funcNode)
	if params == nil {
		return
	}
	pos := 0
	for i, _nc := 0, int(params.NamedChildCount()); i < _nc; i++ {
		p := params.NamedChild(i)
		if p == nil || p.Type() != "parameter" {
			continue
		}
		var name, typeText string
		for j, _nc := 0, int(p.NamedChildCount()); j < _nc; j++ {
			c := p.NamedChild(j)
			if c == nil {
				continue
			}
			switch c.Type() {
			case "simple_identifier":
				if name == "" {
					name = c.Content(src)
				}
			case "user_type", "nullable_type", "function_type":
				if typeText == "" {
					typeText = c.Content(src)
				}
			}
		}
		if name == "" || name == "_" {
			continue
		}
		paramID := ownerID + "#param:" + name + "@" + strconv.Itoa(pos)
		meta := map[string]any{"position": pos}
		if typeText != "" {
			meta["type"] = typeText
		}
		startLine := int(p.StartPoint().Row) + 1
		if startLine == 0 {
			startLine = declLine
		}
		result.Nodes = append(result.Nodes, &graph.Node{
			ID:        paramID,
			Kind:      graph.KindParam,
			Name:      name,
			FilePath:  filePath,
			StartLine: startLine,
			EndLine:   int(p.EndPoint().Row) + 1,
			Language:  "kotlin",
			Meta:      meta,
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From:     paramID,
			To:       ownerID,
			Kind:     graph.EdgeParamOf,
			FilePath: filePath,
			Line:     startLine,
			Origin:   graph.OriginASTResolved,
		})
		emitKotlinTypeUseEdges(paramID, typeText, filePath, startLine, result)
		pos++
	}
}

// emitKotlinReturnEdges emits an EdgeReturns from ownerID to the function's
// declared return type, when present and non-primitive. The return type is
// the user_type / nullable_type that follows the function_value_parameters
// (and precedes the function_body). Mirrors emitTSReturnEdges /
// emitPyReturnEdges.
func emitKotlinReturnEdges(ownerID string, funcNode *sitter.Node, src []byte, filePath string, line int, result *parser.ExtractionResult) {
	if ownerID == "" || funcNode == nil {
		return
	}
	pastParams := false
	for i, _nc := 0, int(funcNode.ChildCount()); i < _nc; i++ {
		child := funcNode.Child(i)
		if child == nil {
			continue
		}
		switch child.Type() {
		case "function_value_parameters":
			pastParams = true
		case "user_type", "nullable_type":
			if !pastParams {
				continue
			}
			t := normalizeKotlinTypeName(child.Content(src))
			if t == "" {
				return
			}
			result.Edges = append(result.Edges, &graph.Edge{
				From:     ownerID,
				To:       "unresolved::" + t,
				Kind:     graph.EdgeReturns,
				FilePath: filePath,
				Line:     line,
				Origin:   graph.OriginASTInferred,
				Meta:     map[string]any{"position": 0},
			})
			return
		case "function_body":
			// Body reached before a return-type annotation → no return type.
			return
		}
	}
}

// emitKotlinFunctionShape wires the parameter-type and return-type edges
// for one Kotlin function/method declaration onto ownerID. Variable
// annotations are handled separately in the Extract post-pass (they need
// the enclosing-function lookup over all declarations).
func emitKotlinFunctionShape(ownerID string, funcNode *sitter.Node, src []byte, filePath string, declLine int, result *parser.ExtractionResult) {
	if funcNode == nil {
		return
	}
	emitKotlinParamShape(ownerID, funcNode, src, filePath, declLine, result)
	emitKotlinReturnEdges(ownerID, funcNode, src, filePath, declLine, result)
}
