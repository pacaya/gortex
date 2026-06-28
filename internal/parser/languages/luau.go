package languages

import (
	"strings"

	"github.com/alexaandru/go-sitter-forest/luau"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// LuauExtractor extracts Roblox's typed Lua dialect (.luau). It reuses
// the structure of LuaExtractor (function/method/local/call walk) and
// adds Luau-specific depth: `type` alias declarations (KindType, with
// Meta["exported"] for `export type`), generic parameters on functions
// and type aliases (KindGenericParam), and best-effort type-annotation
// reference edges from typed parameters / return types to their named
// types.
//
// The grammar is alexaandru/go-sitter-forest/luau — the same tree-sitter
// family the plain lua extractor uses, so `function_declaration`,
// `variable_declaration`, `assignment_statement`, and `function_call`
// carry the identical shape; the only new node kinds are `type_definition`
// (alias), `generic_type` (parametrised name), `parameter`, and the
// `builtin_type` / `object_type` annotations.
type LuauExtractor struct {
	lang *sitter.Language
}

func NewLuauExtractor() *LuauExtractor {
	return &LuauExtractor{lang: sitter.NewLanguage(luau.GetLanguage())}
}

func (e *LuauExtractor) Language() string     { return "luau" }
func (e *LuauExtractor) Extensions() []string { return []string{".luau"} }

func (e *LuauExtractor) Extract(filePath string, src []byte) (*parser.ExtractionResult, error) {
	tree, err := parser.ParseFile(src, e.lang)
	if err != nil {
		return nil, err
	}
	defer tree.Close()

	root := tree.RootNode()
	result := &parser.ExtractionResult{}

	fileNode := &graph.Node{
		ID: filePath, Kind: graph.KindFile, Name: filePath,
		FilePath: filePath, StartLine: 1, EndLine: int(root.EndPoint().Row) + 1,
		Language: "luau",
	}
	result.Nodes = append(result.Nodes, fileNode)

	seen := make(map[string]bool)

	// Top-level children mirror the lua grammar: `function foo()` and
	// `local function foo()` both arrive as `function_declaration`
	// (the `local` prefix is consumed as a sibling keyword), `local x`
	// as `variable_declaration`, `M.foo = function()` as
	// `assignment_statement`. New to Luau: `type_definition` for type
	// aliases.
	for i, _nc := 0, int(root.ChildCount()); i < _nc; i++ {
		child := root.Child(i)
		if child == nil {
			continue
		}

		switch child.Type() {
		case "function_declaration":
			e.extractFunction(child, src, filePath, fileNode, result, seen)

		case "variable_declaration":
			e.extractVariable(child, src, filePath, fileNode, result, seen)

		case "assignment_statement":
			// `M.foo = function() ... end` — emit methods too.
			e.extractAssignmentFunc(child, src, filePath, fileNode, result, seen)

		case "type_definition":
			e.extractType(child, src, filePath, fileNode, result, seen)
		}
	}

	// require() imports — classic string and Roblox instance-path forms,
	// in any position (shared with the Lua extractor).
	extractLuaRequires(root, src, filePath, fileNode.ID, result)

	// Call sites inside functions.
	funcRanges := buildFuncRanges(result)
	e.extractCalls(root, src, filePath, result, funcRanges)

	// Lua functions are first-class values; capture bare-name and
	// table-member callbacks passed as args / assigned but not called.
	captureFnValueCandidates(result, root, filePath, src)

	return result, nil
}

// extractFunction handles `function name(...)` / `function M.name(...)`
// / `function M:name(...)` / `local function name(...)`, plus Luau's
// generic parameters (`function f<T>(...)`) and typed params / return
// type annotations.
func (e *LuauExtractor) extractFunction(
	node *sitter.Node, src []byte, filePath string, fileNode *graph.Node,
	result *parser.ExtractionResult, seen map[string]bool,
) {
	name := ""
	receiver := ""

	nameNode := node.ChildByFieldName("name")
	if nameNode != nil {
		switch nameNode.Type() {
		case "identifier":
			name = nameNode.Content(src)
		case "dot_index_expression", "method_index_expression":
			full := nameNode.Content(src)
			sep := "."
			if nameNode.Type() == "method_index_expression" {
				sep = ":"
			}
			if idx := strings.Index(full, sep); idx > 0 {
				receiver = strings.TrimSpace(full[:idx])
				name = strings.TrimSpace(full[idx+len(sep):])
			} else {
				name = full
			}
		default:
			name = nameNode.Content(src)
		}
	}

	if name == "" {
		return
	}

	kind := graph.KindFunction
	var id string
	if receiver != "" {
		kind = graph.KindMethod
		id = filePath + "::" + receiver + "." + name
	} else {
		id = filePath + "::" + name
	}

	if seen[id] {
		return
	}
	seen[id] = true

	startLine := int(node.StartPoint().Row) + 1
	endLine := int(node.EndPoint().Row) + 1

	n := &graph.Node{
		ID: id, Kind: kind, Name: name,
		FilePath: filePath, StartLine: startLine, EndLine: endLine,
		Language: "luau",
	}
	if receiver != "" {
		n.Meta = map[string]any{"receiver": receiver}
	}

	result.Nodes = append(result.Nodes, n)
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileNode.ID, To: id, Kind: graph.EdgeDefines,
		FilePath: filePath, Line: startLine,
	})

	if receiver != "" {
		typeID := filePath + "::" + receiver
		result.Edges = append(result.Edges, &graph.Edge{
			From: id, To: typeID, Kind: graph.EdgeMemberOf,
			FilePath: filePath, Line: startLine,
		})
	}

	// Generic parameters: `function f<T, U>(...)` — the grammar attaches
	// a `generic_type` node as a sibling of the name field.
	e.emitGenericParams(node, id, src, filePath, result, startLine)

	// Typed parameters + return type → best-effort reference edges to
	// the named types they mention.
	e.emitTypeAnnotations(node, id, src, filePath, result, startLine)

	// Extract nested function definitions inside the body.
	walkNodes(node, func(inner *sitter.Node) {
		if inner.Equal(node) || inner.Type() != "function_declaration" {
			return
		}
		e.extractFunction(inner, src, filePath, fileNode, result, seen)
	})
}

// extractVariable handles `local name = value`, represented as
// `variable_declaration → assignment_statement → variable_list`.
func (e *LuauExtractor) extractVariable(
	node *sitter.Node, src []byte, filePath string, fileNode *graph.Node,
	result *parser.ExtractionResult, seen map[string]bool,
) {
	var assign *sitter.Node
	for i, _nc := 0, int(node.NamedChildCount()); i < _nc; i++ {
		c := node.NamedChild(i)
		if c == nil {
			continue
		}
		if c.Type() == "assignment_statement" {
			assign = c
			break
		}
	}
	if assign == nil {
		return
	}

	var varList *sitter.Node
	for i, _nc := 0, int(assign.NamedChildCount()); i < _nc; i++ {
		c := assign.NamedChild(i)
		if c != nil && c.Type() == "variable_list" {
			varList = c
			break
		}
	}
	if varList == nil {
		return
	}

	for i, _nc := 0, int(varList.NamedChildCount()); i < _nc; i++ {
		ident := varList.NamedChild(i)
		if ident == nil || ident.Type() != "identifier" {
			continue
		}
		name := ident.Content(src)
		if name == "" {
			continue
		}
		id := filePath + "::" + name
		if seen[id] {
			continue
		}
		seen[id] = true

		startLine := int(node.StartPoint().Row) + 1
		result.Nodes = append(result.Nodes, &graph.Node{
			ID: id, Kind: graph.KindVariable, Name: name,
			FilePath: filePath, StartLine: startLine, EndLine: int(node.EndPoint().Row) + 1,
			Language: "luau",
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileNode.ID, To: id, Kind: graph.EdgeDefines,
			FilePath: filePath, Line: startLine,
		})
	}
}

// extractAssignmentFunc handles `M.foo = function() ... end` at the top
// level — emits a method bound to M, just like the lua extractor.
func (e *LuauExtractor) extractAssignmentFunc(
	node *sitter.Node, src []byte, filePath string, fileNode *graph.Node,
	result *parser.ExtractionResult, seen map[string]bool,
) {
	var varList, exprList *sitter.Node
	for i, _nc := 0, int(node.NamedChildCount()); i < _nc; i++ {
		c := node.NamedChild(i)
		if c == nil {
			continue
		}
		switch c.Type() {
		case "variable_list":
			varList = c
		case "expression_list":
			exprList = c
		}
	}
	if varList == nil || exprList == nil {
		return
	}
	// Require RHS to be a function_definition (anonymous function literal).
	hasFunc := false
	for i, _nc := 0, int(exprList.NamedChildCount()); i < _nc; i++ {
		c := exprList.NamedChild(i)
		if c != nil && c.Type() == "function_definition" {
			hasFunc = true
			break
		}
	}
	if !hasFunc {
		return
	}
	if varList.NamedChildCount() == 0 {
		return
	}
	lhs := varList.NamedChild(0)
	if lhs == nil {
		return
	}
	var name, receiver string
	switch lhs.Type() {
	case "dot_index_expression", "method_index_expression":
		full := lhs.Content(src)
		sep := "."
		if lhs.Type() == "method_index_expression" {
			sep = ":"
		}
		if idx := strings.Index(full, sep); idx > 0 {
			receiver = strings.TrimSpace(full[:idx])
			name = strings.TrimSpace(full[idx+len(sep):])
		}
	case "identifier":
		name = lhs.Content(src)
	default:
		return
	}
	if name == "" {
		return
	}

	kind := graph.KindFunction
	var id string
	if receiver != "" {
		kind = graph.KindMethod
		id = filePath + "::" + receiver + "." + name
	} else {
		id = filePath + "::" + name
	}
	if seen[id] {
		return
	}
	seen[id] = true

	startLine := int(node.StartPoint().Row) + 1
	n := &graph.Node{
		ID: id, Kind: kind, Name: name,
		FilePath: filePath, StartLine: startLine, EndLine: int(node.EndPoint().Row) + 1,
		Language: "luau",
	}
	if receiver != "" {
		n.Meta = map[string]any{"receiver": receiver}
	}
	result.Nodes = append(result.Nodes, n)
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileNode.ID, To: id, Kind: graph.EdgeDefines,
		FilePath: filePath, Line: startLine,
	})
	if receiver != "" {
		result.Edges = append(result.Edges, &graph.Edge{
			From: id, To: filePath + "::" + receiver, Kind: graph.EdgeMemberOf,
			FilePath: filePath, Line: startLine,
		})
	}
}

// extractType handles Luau type aliases:
//
//	type Id = number
//	export type Account = { balance: number }
//	type Pair<K, V> = { key: K, value: V }
//
// AST: type_definition { (export)? type name: (identifier | generic_type) = <type> }.
// Emits a KindType node, Meta["exported"] reflecting the `export` keyword,
// an EdgeAliases edge to a best-effort underlying named type, and
// KindGenericParam nodes for any `<K, V>` parameters.
func (e *LuauExtractor) extractType(
	node *sitter.Node, src []byte, filePath string, fileNode *graph.Node,
	result *parser.ExtractionResult, seen map[string]bool,
) {
	exported := false
	for i, _nc := 0, int(node.ChildCount()); i < _nc; i++ {
		c := node.Child(i)
		if c != nil && c.Type() == "export" {
			exported = true
			break
		}
	}

	nameNode := node.ChildByFieldName("name")
	if nameNode == nil {
		return
	}

	// The name may be a bare identifier or a `generic_type` wrapping the
	// identifier plus `<...>` parameters.
	var name string
	var genericNode *sitter.Node
	switch nameNode.Type() {
	case "identifier":
		name = nameNode.Content(src)
	case "generic_type":
		genericNode = nameNode
		if id := firstChildOfType(nameNode, "identifier"); id != nil {
			name = id.Content(src)
		}
	default:
		name = strings.TrimSpace(nameNode.Content(src))
	}
	if name == "" {
		return
	}

	// Types live in a distinct ID namespace (`#type:`) so a type alias
	// can coexist with a same-named runtime table/function — idiomatic
	// in Luau, where `local Account = {}` and `export type Account` name
	// the class and its type respectively.
	id := filePath + "::" + name + "#type"
	if seen[id] {
		return
	}
	seen[id] = true

	startLine := int(node.StartPoint().Row) + 1
	endLine := int(node.EndPoint().Row) + 1

	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindType, Name: name,
		FilePath: filePath, StartLine: startLine, EndLine: endLine,
		Language: "luau",
		Meta:     map[string]any{"exported": exported},
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileNode.ID, To: id, Kind: graph.EdgeDefines,
		FilePath: filePath, Line: startLine,
	})

	// Generic parameters declared on the alias name.
	if genericNode != nil {
		e.emitGenericParamsFromNode(genericNode, id, src, filePath, result, startLine)
	}

	// Best-effort alias edge: `type Id = number` → references the
	// underlying named type. We point at the RHS only when it is a
	// single named type (identifier / generic_type), not an inline
	// object or function type literal.
	rhs := typeAliasRHS(node)
	if rhs != nil {
		if target := namedTypeOf(rhs, src); target != "" && !isBuiltinTypeName(target) {
			result.Edges = append(result.Edges, &graph.Edge{
				From: id, To: "unresolved::" + target, Kind: graph.EdgeAliases,
				FilePath: filePath, Line: startLine,
			})
		}
	}
}

// extractCalls walks the AST for function_call nodes inside functions.
func (e *LuauExtractor) extractCalls(
	root *sitter.Node, src []byte, filePath string,
	result *parser.ExtractionResult, funcRanges []funcRange,
) {
	walkNodes(root, func(node *sitter.Node) {
		if node.Type() != "function_call" {
			return
		}

		line := int(node.StartPoint().Row) + 1
		callerID := findEnclosingFunc(funcRanges, line)
		if callerID == "" {
			return
		}

		fn := node.ChildByFieldName("name")
		if fn == nil {
			return
		}
		switch fn.Type() {
		case "identifier":
			name := fn.Content(src)
			if name == "require" {
				return // handled separately as import
			}
			result.Edges = append(result.Edges, &graph.Edge{
				From: callerID, To: "unresolved::" + name,
				Kind: graph.EdgeCalls, FilePath: filePath, Line: line,
			})
		case "dot_index_expression", "method_index_expression":
			text := fn.Content(src)
			sep := "."
			if fn.Type() == "method_index_expression" {
				sep = ":"
			}
			if idx := strings.LastIndex(text, sep); idx > 0 {
				methodName := text[idx+len(sep):]
				result.Edges = append(result.Edges, &graph.Edge{
					From: callerID, To: "unresolved::*." + methodName,
					Kind: graph.EdgeCalls, FilePath: filePath, Line: line,
				})
			}
		}
	})
}

// emitGenericParams looks for a `generic_type` node attached to a
// function declaration's name (`function f<T>(...)`) and emits a
// KindGenericParam node per type parameter.
func (e *LuauExtractor) emitGenericParams(
	fnNode *sitter.Node, ownerID string, src []byte, filePath string,
	result *parser.ExtractionResult, line int,
) {
	gn := fnNode.ChildByFieldName("name")
	if gn != nil && gn.Type() == "generic_type" {
		e.emitGenericParamsFromNode(gn, ownerID, src, filePath, result, line)
		return
	}
	// Some grammar variants attach a bare generic_type as a sibling.
	if gn := firstChildOfType(fnNode, "generic_type"); gn != nil {
		e.emitGenericParamsFromNode(gn, ownerID, src, filePath, result, line)
	}
}

// emitGenericParamsFromNode walks a `generic_type` node — the first
// identifier is the owner's name, every subsequent identifier is a type
// parameter — and emits KindGenericParam nodes for the parameters.
func (e *LuauExtractor) emitGenericParamsFromNode(
	gn *sitter.Node, ownerID string, src []byte, filePath string,
	result *parser.ExtractionResult, line int,
) {
	first := true
	for i, _nc := 0, int(gn.NamedChildCount()); i < _nc; i++ {
		c := gn.NamedChild(i)
		if c == nil || c.Type() != "identifier" {
			continue
		}
		if first {
			// Skip the owner's own name (e.g. `Pair` in `Pair<K, V>`).
			first = false
			continue
		}
		name := c.Content(src)
		if name == "" {
			continue
		}
		gpID := ownerID + "#tparam:" + name
		result.Nodes = append(result.Nodes, &graph.Node{
			ID: gpID, Kind: graph.KindGenericParam, Name: name,
			FilePath: filePath, StartLine: line, EndLine: line,
			Language: "luau",
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From: gpID, To: ownerID, Kind: graph.EdgeMemberOf,
			FilePath: filePath, Line: line,
		})
	}
}

// emitTypeAnnotations emits best-effort EdgeTypedAs edges from a function
// to the named types mentioned in its typed parameters, plus an
// EdgeReferences edge to the named return type. Builtin primitives
// (string, number, boolean, ...) are skipped — they are not graph nodes.
func (e *LuauExtractor) emitTypeAnnotations(
	fnNode *sitter.Node, ownerID string, src []byte, filePath string,
	result *parser.ExtractionResult, line int,
) {
	// Typed parameters.
	if params := fnNode.ChildByFieldName("parameters"); params != nil {
		for i, _nc := 0, int(params.NamedChildCount()); i < _nc; i++ {
			p := params.NamedChild(i)
			if p == nil || p.Type() != "parameter" {
				continue
			}
			if tn := paramTypeNode(p); tn != nil {
				if name := namedTypeOf(tn, src); name != "" && !isBuiltinTypeName(name) {
					result.Edges = append(result.Edges, &graph.Edge{
						From: ownerID, To: "unresolved::" + name, Kind: graph.EdgeTypedAs,
						FilePath: filePath, Line: line,
					})
				}
			}
		}
	}

	// Return type: a `:` then a type node placed between `parameters`
	// and `body` at the top level of the declaration.
	if rt := returnTypeNode(fnNode); rt != nil {
		if name := namedTypeOf(rt, src); name != "" && !isBuiltinTypeName(name) {
			result.Edges = append(result.Edges, &graph.Edge{
				From: ownerID, To: "unresolved::" + name, Kind: graph.EdgeReferences,
				FilePath: filePath, Line: line,
			})
		}
	}
}

// --- Luau type helpers ---------------------------------------------

// firstChildOfType returns the first direct child of the given type.
func firstChildOfType(node *sitter.Node, typ string) *sitter.Node {
	for i, _nc := 0, int(node.ChildCount()); i < _nc; i++ {
		c := node.Child(i)
		if c != nil && c.Type() == typ {
			return c
		}
	}
	return nil
}

// paramTypeNode returns the type-annotation node of a `parameter`
// (`name: T`) — the named child after the `:` token, if present.
func paramTypeNode(p *sitter.Node) *sitter.Node {
	seenColon := false
	for i, _nc := 0, int(p.ChildCount()); i < _nc; i++ {
		c := p.Child(i)
		if c == nil {
			continue
		}
		if c.Type() == ":" {
			seenColon = true
			continue
		}
		if seenColon && c.IsNamed() {
			return c
		}
	}
	return nil
}

// returnTypeNode returns the return-type annotation node of a function
// declaration — the named node sitting after the `:` that follows the
// `parameters` field but before the `body`.
func returnTypeNode(fnNode *sitter.Node) *sitter.Node {
	params := fnNode.ChildByFieldName("parameters")
	body := fnNode.ChildByFieldName("body")
	if params == nil {
		return nil
	}
	afterParams := false
	for i, _nc := 0, int(fnNode.ChildCount()); i < _nc; i++ {
		c := fnNode.Child(i)
		if c == nil {
			continue
		}
		if c.Equal(params) {
			afterParams = true
			continue
		}
		if body != nil && c.Equal(body) {
			break
		}
		if afterParams && c.IsNamed() {
			return c
		}
	}
	return nil
}

// typeAliasRHS returns the underlying type node of a `type_definition`
// — the named node after the `=` token.
func typeAliasRHS(node *sitter.Node) *sitter.Node {
	seenEq := false
	for i, _nc := 0, int(node.ChildCount()); i < _nc; i++ {
		c := node.Child(i)
		if c == nil {
			continue
		}
		if c.Type() == "=" {
			seenEq = true
			continue
		}
		if seenEq && c.IsNamed() {
			return c
		}
	}
	return nil
}

// namedTypeOf returns the named-type identifier referenced by a type
// node, or "" if the node is not a plain named reference (e.g. an inline
// object_type or function_type literal). `builtin_type` resolves to its
// primitive name (caller filters those out).
func namedTypeOf(node *sitter.Node, src []byte) string {
	if node == nil {
		return ""
	}
	switch node.Type() {
	case "identifier", "builtin_type":
		return strings.TrimSpace(node.Content(src))
	case "generic_type":
		if id := firstChildOfType(node, "identifier"); id != nil {
			return strings.TrimSpace(id.Content(src))
		}
	case "type":
		// Some grammar revisions wrap the reference in a `type` node.
		if id := firstChildOfType(node, "identifier"); id != nil {
			return strings.TrimSpace(id.Content(src))
		}
	}
	return ""
}

// isBuiltinTypeName reports whether name is a Luau primitive type that
// should not be treated as a referenceable graph symbol.
func isBuiltinTypeName(name string) bool {
	switch name {
	case "string", "number", "boolean", "nil", "any", "unknown",
		"never", "thread", "buffer", "table", "void", "self", "true", "false":
		return true
	}
	return false
}
