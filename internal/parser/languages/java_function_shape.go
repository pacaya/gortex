package languages

import (
	"strconv"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// emitJavaFunctionShape emits KindParam/EdgeParamOf/EdgeTypedAs/
// EdgeReturns/KindGenericParam for a Java method_declaration.
// Constructors share the same body shape and so reuse this helper.
func emitJavaFunctionShape(ownerID string, methodNode *sitter.Node, src []byte, filePath string, declLine int, result *parser.ExtractionResult) {
	if methodNode == nil {
		return
	}
	if params := javaFormalParameters(methodNode); params != nil {
		emitJavaParamNodes(ownerID, params, src, filePath, declLine, result)
	}
	if rt := javaReturnTypeRaw(methodNode, src); rt != "" {
		emitJavaReturnEdges(ownerID, rt, filePath, declLine, result)
	}
	emitJavaGenericParamNodes(ownerID, methodNode, src, filePath, declLine, result)
}

func javaFormalParameters(methodNode *sitter.Node) *sitter.Node {
	if p := methodNode.ChildByFieldName("parameters"); p != nil {
		return p
	}
	for i, _nc := 0, int(methodNode.NamedChildCount()); i < _nc; i++ {
		c := methodNode.NamedChild(i)
		if c != nil && c.Type() == "formal_parameters" {
			return c
		}
	}
	return nil
}

func emitJavaParamNodes(ownerID string, params *sitter.Node, src []byte, filePath string, declLine int, result *parser.ExtractionResult) {
	pos := 0
	for i, _nc := 0, int(params.NamedChildCount()); i < _nc; i++ {
		decl := params.NamedChild(i)
		if decl == nil {
			continue
		}
		t := decl.Type()
		if t != "formal_parameter" && t != "spread_parameter" {
			continue
		}
		isVariadic := t == "spread_parameter"
		var name, typeRaw string
		if n := decl.ChildByFieldName("name"); n != nil {
			name = n.Content(src)
		}
		if ty := decl.ChildByFieldName("type"); ty != nil {
			typeRaw = strings.TrimSpace(ty.Content(src))
		}
		// Some grammar shapes (spread_parameter) wrap the name in a
		// (variable_declarator name: (identifier)) — fall back.
		if name == "" {
			for j, _nc := 0, int(decl.NamedChildCount()); j < _nc; j++ {
				c := decl.NamedChild(j)
				if c == nil {
					continue
				}
				if c.Type() == "identifier" {
					name = c.Content(src)
					break
				}
				if c.Type() == "variable_declarator" {
					if vn := c.ChildByFieldName("name"); vn != nil {
						name = vn.Content(src)
						break
					}
				}
			}
		}
		if name == "" || name == "_" {
			continue
		}
		paramID := ownerID + "#param:" + name + "@" + strconv.Itoa(pos)
		meta := map[string]any{"position": pos}
		if isVariadic {
			meta["variadic"] = true
		}
		if typeRaw != "" {
			meta["type"] = typeRaw
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
			Language:  "java",
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
		if canon := canonicalizeJavaTypeRef(typeRaw); canon != "" && !isJavaPrimitive(canon) {
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

func javaReturnTypeRaw(methodNode *sitter.Node, src []byte) string {
	if rt := methodNode.ChildByFieldName("type"); rt != nil {
		return strings.TrimSpace(rt.Content(src))
	}
	return ""
}

func emitJavaReturnEdges(ownerID, returnText, filePath string, line int, result *parser.ExtractionResult) {
	if returnText == "" {
		return
	}
	t := canonicalizeJavaTypeRef(returnText)
	if t == "" || isJavaPrimitive(t) {
		return
	}
	result.Edges = append(result.Edges, &graph.Edge{
		From:     ownerID,
		To:       "unresolved::" + t,
		Kind:     graph.EdgeReturns,
		FilePath: filePath,
		Line:     line,
		Origin:   graph.OriginASTInferred,
		Meta: map[string]any{
			"position": 0,
		},
	})
}

// emitJavaTypeUseEdges parses a local-variable / field type annotation
// and emits one EdgeTypedAs to unresolved::<type>, so a type used only
// in declaration position (`HttpResponse resp = client.get();`) is a
// first-class cross-file reference the name-based resolver can land
// without an LSP. Bare-type canonicalization (strip generics <…>, array
// [], varargs …, package prefix) and primitive skipping reuse the same
// helpers as the param / return type edges. Mirrors emitJavaReturnEdges.
func emitJavaTypeUseEdges(ownerID, typeText, filePath string, line int, result *parser.ExtractionResult) {
	t := canonicalizeJavaTypeRef(typeText)
	if t == "" || isJavaPrimitive(t) {
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

func emitJavaGenericParamNodes(ownerID string, methodNode *sitter.Node, src []byte, filePath string, line int, result *parser.ExtractionResult) {
	tparams := methodNode.ChildByFieldName("type_parameters")
	if tparams == nil {
		// Look for unnamed type_parameters child.
		for i, _nc := 0, int(methodNode.NamedChildCount()); i < _nc; i++ {
			c := methodNode.NamedChild(i)
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
		var name, bound string
		for j, _nc := 0, int(tp.NamedChildCount()); j < _nc; j++ {
			c := tp.NamedChild(j)
			if c == nil {
				continue
			}
			if c.Type() == "type_identifier" && name == "" {
				name = c.Content(src)
			}
			if c.Type() == "type_bound" {
				bound = strings.TrimSpace(c.Content(src))
				bound = strings.TrimPrefix(bound, "extends ")
			}
		}
		if name == "" {
			continue
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
			Language:  "java",
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

func canonicalizeJavaTypeRef(t string) string {
	t = strings.TrimSpace(t)
	if t == "" {
		return ""
	}
	// Strip array suffix.
	for strings.HasSuffix(t, "[]") {
		t = strings.TrimSuffix(t, "[]")
		t = strings.TrimSpace(t)
	}
	// Strip varargs marker.
	t = strings.TrimSuffix(t, "...")
	t = strings.TrimSpace(t)
	// Unwrap common containers: List<X>, Optional<X>, Mono<X>, Flux<X>,
	// CompletableFuture<X>.
	for _, wrapper := range []string{"List", "ArrayList", "LinkedList", "Collection",
		"Set", "HashSet", "Optional", "Mono", "Flux", "CompletableFuture",
		"Future", "Iterable", "Iterator", "Stream"} {
		prefix := wrapper + "<"
		if strings.HasPrefix(t, prefix) && strings.HasSuffix(t, ">") {
			inner := t[len(prefix) : len(t)-1]
			return canonicalizeJavaTypeRef(inner)
		}
	}
	// Strip generic <…> tail.
	if idx := strings.Index(t, "<"); idx > 0 {
		t = t[:idx]
	}
	// Strip package prefix: com.example.User → User.
	if idx := strings.LastIndex(t, "."); idx >= 0 {
		t = t[idx+1:]
	}
	return strings.TrimSpace(t)
}

func isJavaPrimitive(t string) bool {
	switch t {
	case "", "void", "boolean", "byte", "short", "int", "long",
		"float", "double", "char",
		"String", "Object", "Number":
		return true
	}
	return false
}
