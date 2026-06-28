package languages

import (
	"strconv"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// emitPyFunctionShape emits KindParam / EdgeParamOf / EdgeTypedAs /
// EdgeReturns / KindGenericParam for a Python function_definition.
//
// ownerID is the qualified ID under which the params/returns/generics
// should be attributed. Skipping `self` and `cls` keeps method
// metadata consistent with how the rest of the graph treats receivers
// (KindMethod nodes already carry meta.receiver). The blank
// identifier `_` is also skipped.
func emitPyFunctionShape(ownerID string, funcNode *sitter.Node, src []byte, filePath string, declLine int, result *parser.ExtractionResult) {
	if funcNode == nil {
		return
	}
	if params := funcNode.ChildByFieldName("parameters"); params != nil {
		emitPyParamNodes(ownerID, params, src, filePath, declLine, result)
	}
	if rt := pyReturnTypeRaw(funcNode, src); rt != "" {
		emitPyReturnEdges(ownerID, rt, filePath, declLine, result)
	}
	emitPyGenericParamNodes(ownerID, funcNode, src, filePath, declLine, result)
	if body := funcNode.ChildByFieldName("body"); body != nil {
		emitPyAsyncSpawns(ownerID, body, src, filePath, result)
	}
}

// emitPyAsyncSpawns walks a Python function body for await
// expressions and asyncio.{gather, create_task, ensure_future, run}
// calls, emitting EdgeSpawns from the enclosing function to the
// awaited target.
func emitPyAsyncSpawns(ownerID string, body *sitter.Node, src []byte, filePath string, result *parser.ExtractionResult) {
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
	walkPyNodes(body, func(n *sitter.Node) bool {
		switch n.Type() {
		case "function_definition", "lambda":
			// Don't descend into nested function bodies.
			return false
		case "await":
			// `await foo()` parses as (await (call …)). Walk for
			// the call node directly.
			for i, _nc := 0, int(n.NamedChildCount()); i < _nc; i++ {
				c := n.NamedChild(i)
				if c == nil || c.Type() != "call" {
					continue
				}
				if name := pyCallTargetName(c, src); name != "" {
					emit(name, "async", int(n.StartPoint().Row)+1)
				}
			}
		case "call":
			fn := n.ChildByFieldName("function")
			if fn == nil {
				return true
			}
			if fn.Type() == "attribute" {
				obj := fn.ChildByFieldName("object")
				attr := fn.ChildByFieldName("attribute")
				if obj != nil && attr != nil && obj.Content(src) == "asyncio" {
					attrName := attr.Content(src)
					switch attrName {
					case "gather", "create_task", "ensure_future", "run", "wait", "wait_for", "shield":
						emit("asyncio."+attrName, "async", int(n.StartPoint().Row)+1)
					}
				}
			}
		}
		return true
	})
}

// walkPyNodes is a Python-grammar pre-order walker that mirrors the
// Go and TS variants. Returning false from visit skips the subtree.
func walkPyNodes(n *sitter.Node, visit func(*sitter.Node) bool) {
	if n == nil {
		return
	}
	if !visit(n) {
		return
	}
	for i, _nc := 0, int(n.NamedChildCount()); i < _nc; i++ {
		walkPyNodes(n.NamedChild(i), visit)
	}
}

// pyCallTargetName extracts the textual function name of a Python
// call node. attribute (`a.b()`) returns the attribute name; bare
// identifier returns its text. Higher-order or complex callees return
// "".
func pyCallTargetName(call *sitter.Node, src []byte) string {
	fn := call.ChildByFieldName("function")
	if fn == nil {
		return ""
	}
	switch fn.Type() {
	case "identifier":
		return fn.Content(src)
	case "attribute":
		if a := fn.ChildByFieldName("attribute"); a != nil {
			return a.Content(src)
		}
	}
	return ""
}

func emitPyParamNodes(ownerID string, params *sitter.Node, src []byte, filePath string, declLine int, result *parser.ExtractionResult) {
	pos := 0
	for i, _nc := 0, int(params.NamedChildCount()); i < _nc; i++ {
		decl := params.NamedChild(i)
		if decl == nil {
			continue
		}
		name, typeName, variadic := pyParamShape(decl, src)
		if name == "" || name == "_" || name == "self" || name == "cls" {
			continue
		}
		paramID := ownerID + "#param:" + name + "@" + strconv.Itoa(pos)
		meta := map[string]any{"position": pos}
		if variadic {
			meta["variadic"] = true
		}
		if typeName != "" {
			meta["type"] = typeName
		}
		startLine := int(decl.StartPoint().Row) + 1
		if startLine == 0 {
			startLine = declLine
		}
		result.Nodes = append(result.Nodes, &graph.Node{
			ID:        paramID,
			Kind:      graph.KindParam,
			Name:      name,
			FilePath:  filePath,
			StartLine: startLine,
			EndLine:   int(decl.EndPoint().Row) + 1,
			Language:  "python",
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
		if canon := canonicalizePyTypeRef(typeName); canon != "" && !isPyPrimitive(canon) {
			result.Edges = append(result.Edges, &graph.Edge{
				From:     paramID,
				To:       "unresolved::" + canon,
				Kind:     graph.EdgeTypedAs,
				FilePath: filePath,
				Line:     startLine,
				Origin:   graph.OriginASTInferred,
			})
		}
		pos++
	}
}

// pyParamShape pulls (name, type, isVariadic) out of one parameter
// node, navigating the four shapes the Python grammar uses.
func pyParamShape(decl *sitter.Node, src []byte) (string, string, bool) {
	switch decl.Type() {
	case "identifier":
		return decl.Content(src), "", false
	case "typed_parameter":
		// (typed_parameter name: (identifier) type: (type))
		var name, typ string
		variadic := false
		for i, _nc := 0, int(decl.NamedChildCount()); i < _nc; i++ {
			c := decl.NamedChild(i)
			if c == nil {
				continue
			}
			switch c.Type() {
			case "identifier":
				if name == "" {
					name = c.Content(src)
				}
			case "type":
				typ = strings.TrimSpace(c.Content(src))
			case "list_splat_pattern":
				variadic = true
				for j, _nc := 0, int(c.NamedChildCount()); j < _nc; j++ {
					cc := c.NamedChild(j)
					if cc != nil && cc.Type() == "identifier" {
						name = cc.Content(src)
					}
				}
			case "dictionary_splat_pattern":
				variadic = true
				for j, _nc := 0, int(c.NamedChildCount()); j < _nc; j++ {
					cc := c.NamedChild(j)
					if cc != nil && cc.Type() == "identifier" {
						name = cc.Content(src)
					}
				}
			}
		}
		return name, typ, variadic
	case "default_parameter":
		// (default_parameter name: (identifier) value: (...))
		if n := decl.ChildByFieldName("name"); n != nil && n.Type() == "identifier" {
			return n.Content(src), "", false
		}
	case "typed_default_parameter":
		// (typed_default_parameter name: (identifier) type: (type) value: (...))
		var name, typ string
		if n := decl.ChildByFieldName("name"); n != nil && n.Type() == "identifier" {
			name = n.Content(src)
		}
		if t := decl.ChildByFieldName("type"); t != nil {
			typ = strings.TrimSpace(t.Content(src))
		}
		return name, typ, false
	case "list_splat_pattern":
		// *args bare (no annotation)
		for i, _nc := 0, int(decl.NamedChildCount()); i < _nc; i++ {
			c := decl.NamedChild(i)
			if c != nil && c.Type() == "identifier" {
				return c.Content(src), "", true
			}
		}
	case "dictionary_splat_pattern":
		// **kwargs bare
		for i, _nc := 0, int(decl.NamedChildCount()); i < _nc; i++ {
			c := decl.NamedChild(i)
			if c != nil && c.Type() == "identifier" {
				return c.Content(src), "", true
			}
		}
	}
	return "", "", false
}

func pyReturnTypeRaw(funcNode *sitter.Node, src []byte) string {
	for i, _nc := 0, int(funcNode.NamedChildCount()); i < _nc; i++ {
		c := funcNode.NamedChild(i)
		if c != nil && c.Type() == "type" {
			return strings.TrimSpace(c.Content(src))
		}
	}
	return ""
}

func emitPyReturnEdges(ownerID, returnText, filePath string, line int, result *parser.ExtractionResult) {
	if returnText == "" {
		return
	}
	for i, raw := range splitPyUnionType(returnText) {
		t := canonicalizePyTypeRef(raw)
		if t == "" || isPyPrimitive(t) {
			continue
		}
		result.Edges = append(result.Edges, &graph.Edge{
			From:     ownerID,
			To:       "unresolved::" + t,
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

// emitPyTypeUseEdges parses a variable annotation type and emits one
// EdgeTypedAs per top-level named type to unresolved::<type>, so a type
// used only in `x: T` annotation position is a first-class cross-file
// reference the name-based resolver can land without an LSP. Mirrors
// emitPyReturnEdges; primitives are skipped.
func emitPyTypeUseEdges(ownerID, typeText, filePath string, line int, result *parser.ExtractionResult) {
	if typeText == "" {
		return
	}
	for _, raw := range splitPyUnionType(typeText) {
		t := canonicalizePyTypeRef(raw)
		if t == "" || isPyPrimitive(t) {
			continue
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
}

func emitPyGenericParamNodes(ownerID string, funcNode *sitter.Node, src []byte, filePath string, line int, result *parser.ExtractionResult) {
	tparams := funcNode.ChildByFieldName("type_parameters")
	if tparams == nil {
		return
	}
	for i, _nc := 0, int(tparams.NamedChildCount()); i < _nc; i++ {
		tp := tparams.NamedChild(i)
		if tp == nil || tp.Type() != "type_parameter" {
			continue
		}
		var name, bound string
		if n := tp.ChildByFieldName("name"); n != nil {
			name = n.Content(src)
		}
		if name == "" {
			// Fallback: first identifier child.
			for j, _nc := 0, int(tp.NamedChildCount()); j < _nc; j++ {
				c := tp.NamedChild(j)
				if c != nil && c.Type() == "identifier" {
					name = c.Content(src)
					break
				}
			}
		}
		if name == "" {
			continue
		}
		if b := tp.ChildByFieldName("bound"); b != nil {
			bound = strings.TrimSpace(b.Content(src))
		}
		gpID := ownerID + "#tparam:" + name
		meta := map[string]any{}
		if bound != "" {
			meta["bound"] = bound
		}
		result.Nodes = append(result.Nodes, &graph.Node{
			ID:        gpID,
			Kind:      graph.KindGenericParam,
			Name:      name,
			FilePath:  filePath,
			StartLine: line,
			EndLine:   line,
			Language:  "python",
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

// canonicalizePyTypeRef strips wrappers Python idiomatic typing has
// (Optional[X] / List[X] / Tuple[X, ...] / Sequence[X]) so the
// resolver can land the type on the actual class node.
func canonicalizePyTypeRef(t string) string {
	t = strings.TrimSpace(t)
	if t == "" {
		return ""
	}
	// Strip "X | None" → X (PEP-604 union with None).
	if parts := splitPyUnionType(t); len(parts) > 1 {
		// If only one non-None branch, pick it.
		nonNone := []string{}
		for _, p := range parts {
			if p != "None" && p != "" {
				nonNone = append(nonNone, p)
			}
		}
		if len(nonNone) == 1 {
			t = nonNone[0]
		}
	}
	for _, wrapper := range []string{"Optional", "List", "list", "Sequence", "Iterable", "Iterator", "Awaitable", "Coroutine", "Tuple", "tuple", "Set", "set", "FrozenSet"} {
		prefix := wrapper + "["
		if strings.HasPrefix(t, prefix) && strings.HasSuffix(t, "]") {
			inner := t[len(prefix) : len(t)-1]
			// Tuple[X, Y, ...] — pick first element only when the
			// wrapper is a homogeneous container; for Tuple keep it
			// raw when comma is present so the resolver doesn't
			// accidentally land us on the wrong type.
			if wrapper == "Tuple" || wrapper == "tuple" {
				if idx := strings.Index(inner, ","); idx > 0 {
					return canonicalizePyTypeRef(strings.TrimSpace(inner[:idx]))
				}
			}
			return canonicalizePyTypeRef(inner)
		}
	}
	// Generic[T1, T2] — take the head.
	if idx := strings.Index(t, "["); idx > 0 {
		t = t[:idx]
	}
	// strip surrounding parens
	for strings.HasPrefix(t, "(") && strings.HasSuffix(t, ")") {
		t = strings.TrimSpace(t[1 : len(t)-1])
	}
	// Strip module-qualified prefix: "pkg.mod.Foo" → "Foo".
	if idx := strings.LastIndex(t, "."); idx >= 0 {
		t = t[idx+1:]
	}
	return strings.TrimSpace(t)
}

func splitPyUnionType(t string) []string {
	t = strings.TrimSpace(t)
	if t == "" {
		return nil
	}
	depth := 0
	parts := []string{}
	cur := strings.Builder{}
	for i := 0; i < len(t); i++ {
		c := t[i]
		switch c {
		case '[', '(', '{':
			depth++
		case ']', ')', '}':
			depth--
		case '|':
			if depth == 0 {
				parts = append(parts, strings.TrimSpace(cur.String()))
				cur.Reset()
				continue
			}
		}
		cur.WriteByte(c)
	}
	if cur.Len() > 0 {
		parts = append(parts, strings.TrimSpace(cur.String()))
	}
	return parts
}

func isPyPrimitive(t string) bool {
	switch t {
	case "", "None", "Any", "Never", "object",
		"str", "int", "float", "bool", "bytes", "bytearray",
		"complex":
		return true
	}
	return false
}
