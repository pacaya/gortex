package languages

import (
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/lua"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// LuaExtractor extracts Lua source files.
type LuaExtractor struct {
	lang *sitter.Language
}

func NewLuaExtractor() *LuaExtractor {
	return &LuaExtractor{lang: lua.GetLanguage()}
}

func (e *LuaExtractor) Language() string     { return "lua" }
func (e *LuaExtractor) Extensions() []string { return []string{".lua"} }

func (e *LuaExtractor) Extract(filePath string, src []byte) (*parser.ExtractionResult, error) {
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
		Language: "lua",
	}
	result.Nodes = append(result.Nodes, fileNode)

	seen := make(map[string]bool)

	// Walk top-level children.
	for i := 0; i < int(root.ChildCount()); i++ {
		child := root.Child(i)
		if child == nil {
			continue
		}

		switch child.Type() {
		case "function_statement":
			e.extractFunction(child, src, filePath, fileNode, result, seen)

		case "variable_declaration":
			e.extractVariable(child, src, filePath, fileNode, result, seen)

		case "function_call":
			// Top-level calls (require, etc.)
			e.extractTopLevelCall(child, src, filePath, fileNode, result)
		}
	}

	// Call sites inside functions.
	funcRanges := buildFuncRanges(result)
	e.extractCalls(root, src, filePath, result, funcRanges)

	return result, nil
}

// extractFunction handles `function name(...)` and `local function name(...)`
// AST: function_statement → function_name → identifier(s), parameter_list, function_body
func (e *LuaExtractor) extractFunction(
	node *sitter.Node, src []byte, filePath string, fileNode *graph.Node,
	result *parser.ExtractionResult, seen map[string]bool,
) {
	isLocal := false
	name := ""
	receiver := ""

	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(i)
		if child == nil {
			continue
		}
		switch child.Type() {
		case "local":
			isLocal = true
		case "function_name":
			// function_name may contain dots: M.greet → identifier "M", table_dot ".", identifier "greet"
			name = child.Content(src)
			// Check if it's a method-style: M.func or M:method
			if strings.Contains(name, ".") {
				parts := strings.SplitN(name, ".", 2)
				receiver = parts[0]
				name = parts[1]
			} else if strings.Contains(name, ":") {
				parts := strings.SplitN(name, ":", 2)
				receiver = parts[0]
				name = parts[1]
			}
		case "identifier":
			// local function name — identifier is a direct child
			if isLocal && name == "" {
				name = child.Content(src)
			}
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
		Language: "lua",
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

	// Extract nested function definitions inside the body.
	walkNodes(node, func(inner *sitter.Node) {
		if inner == node || inner.Type() != "function_statement" {
			return
		}
		e.extractFunction(inner, src, filePath, fileNode, result, seen)
	})
}

// extractVariable handles `local name = value`
func (e *LuaExtractor) extractVariable(
	node *sitter.Node, src []byte, filePath string, fileNode *graph.Node,
	result *parser.ExtractionResult, seen map[string]bool,
) {
	name := ""
	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(i)
		if child == nil {
			continue
		}
		if child.Type() == "variable_declarator" {
			// variable_declarator → identifier
			for j := 0; j < int(child.ChildCount()); j++ {
				part := child.Child(j)
				if part != nil && part.Type() == "identifier" {
					name = part.Content(src)
					break
				}
			}
			break
		}
	}

	if name == "" {
		return
	}

	id := filePath + "::" + name
	if seen[id] {
		return
	}
	seen[id] = true

	startLine := int(node.StartPoint().Row) + 1

	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindVariable, Name: name,
		FilePath: filePath, StartLine: startLine, EndLine: int(node.EndPoint().Row) + 1,
		Language: "lua",
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileNode.ID, To: id, Kind: graph.EdgeDefines,
		FilePath: filePath, Line: startLine,
	})
}

// extractTopLevelCall handles top-level require() calls as imports.
func (e *LuaExtractor) extractTopLevelCall(
	node *sitter.Node, src []byte, filePath string, fileNode *graph.Node,
	result *parser.ExtractionResult,
) {
	// Check if it's require("module")
	funcName := ""
	arg := ""
	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(i)
		if child == nil {
			continue
		}
		if child.Type() == "identifier" && funcName == "" {
			funcName = strings.TrimSpace(child.Content(src))
		}
		if child.Type() == "function_arguments" {
			for j := 0; j < int(child.NamedChildCount()); j++ {
				argNode := child.NamedChild(j)
				if argNode != nil && argNode.Type() == "string" {
					arg = strings.Trim(argNode.Content(src), `"'`)
					break
				}
			}
		}
	}

	if funcName == "require" && arg != "" {
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileNode.ID, To: "unresolved::import::" + arg,
			Kind: graph.EdgeImports, FilePath: filePath, Line: int(node.StartPoint().Row) + 1,
		})
	}
}

// extractCalls walks the AST for function_call nodes inside functions.
func (e *LuaExtractor) extractCalls(
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

		// Extract function name from children.
		// Simple call: identifier "func" → func()
		// Method call: identifier "obj", identifier "method" → obj.method() or obj:method()
		var names []string
		for i := 0; i < int(node.ChildCount()); i++ {
			child := node.Child(i)
			if child != nil && child.Type() == "identifier" {
				names = append(names, child.Content(src))
			}
		}

		if len(names) == 0 {
			return
		}

		if len(names) == 1 {
			name := names[0]
			if name == "require" {
				return // handled separately as import
			}
			result.Edges = append(result.Edges, &graph.Edge{
				From: callerID, To: "unresolved::" + name,
				Kind: graph.EdgeCalls, FilePath: filePath, Line: line,
			})
		} else {
			// Method call: obj.method or obj:method
			methodName := names[len(names)-1]
			result.Edges = append(result.Edges, &graph.Edge{
				From: callerID, To: "unresolved::*." + methodName,
				Kind: graph.EdgeCalls, FilePath: filePath, Line: line,
			})
		}
	})
}
