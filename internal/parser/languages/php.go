package languages

import (
	"fmt"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/php"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// PHPExtractor extracts PHP source files.
type PHPExtractor struct {
	lang *sitter.Language
}

func NewPHPExtractor() *PHPExtractor {
	return &PHPExtractor{lang: php.GetLanguage()}
}

func (e *PHPExtractor) Language() string     { return "php" }
func (e *PHPExtractor) Extensions() []string { return []string{".php"} }

func (e *PHPExtractor) Extract(filePath string, src []byte) (*parser.ExtractionResult, error) {
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
		Language: "php",
	}
	result.Nodes = append(result.Nodes, fileNode)

	seen := make(map[string]bool)

	// Walk the AST manually since PHP tree-sitter queries can be tricky.
	e.walkNode(root, src, filePath, fileNode, result, seen, "")

	return result, nil
}

func (e *PHPExtractor) walkNode(
	node *sitter.Node, src []byte,
	filePath string, fileNode *graph.Node,
	result *parser.ExtractionResult, seen map[string]bool,
	currentClass string,
) {
	nodeType := node.Type()

	switch nodeType {
	case "namespace_definition":
		e.extractNamespace(node, src, filePath, fileNode, result, seen)

	case "class_declaration":
		e.extractClass(node, src, filePath, fileNode, result, seen)

	case "interface_declaration":
		e.extractInterface(node, src, filePath, fileNode, result, seen)

	case "function_definition":
		e.extractFunction(node, src, filePath, fileNode, result, seen)

	case "namespace_use_declaration":
		e.extractUseImport(node, src, filePath, fileNode, result)

	case "expression_statement":
		// Check for require/include calls.
		e.extractRequireInclude(node, src, filePath, fileNode, result)
		// Also walk children for call expressions.
		e.walkChildren(node, src, filePath, fileNode, result, seen, currentClass)
		return

	default:
		// For class/interface bodies, walk into children with class context.
		e.walkChildren(node, src, filePath, fileNode, result, seen, currentClass)
		return
	}
}

func (e *PHPExtractor) walkChildren(
	node *sitter.Node, src []byte,
	filePath string, fileNode *graph.Node,
	result *parser.ExtractionResult, seen map[string]bool,
	currentClass string,
) {
	for i := 0; i < int(node.NamedChildCount()); i++ {
		child := node.NamedChild(i)
		e.walkNode(child, src, filePath, fileNode, result, seen, currentClass)
	}
}

func (e *PHPExtractor) extractNamespace(
	node *sitter.Node, src []byte,
	filePath string, fileNode *graph.Node,
	result *parser.ExtractionResult, seen map[string]bool,
) {
	name := e.findChildByType(node, "namespace_name")
	if name == nil {
		return
	}
	nsName := name.Content(src)
	id := filePath + "::" + nsName
	if seen[id] {
		return
	}
	seen[id] = true
	startLine := int(node.StartPoint().Row) + 1
	endLine := int(node.EndPoint().Row) + 1
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindPackage, Name: nsName,
		FilePath: filePath, StartLine: startLine, EndLine: endLine,
		Language: "php",
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileNode.ID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: startLine,
	})

	// Walk children of namespace body.
	body := e.findChildByType(node, "compound_statement")
	if body != nil {
		for i := 0; i < int(body.NamedChildCount()); i++ {
			child := body.NamedChild(i)
			e.walkNode(child, src, filePath, fileNode, result, seen, "")
		}
	}
	// Some namespaces don't use braces; walk remaining children.
	for i := 0; i < int(node.NamedChildCount()); i++ {
		child := node.NamedChild(i)
		if child.Type() != "namespace_name" && child.Type() != "compound_statement" {
			e.walkNode(child, src, filePath, fileNode, result, seen, "")
		}
	}
}

func (e *PHPExtractor) extractClass(
	node *sitter.Node, src []byte,
	filePath string, fileNode *graph.Node,
	result *parser.ExtractionResult, seen map[string]bool,
) {
	nameNode := e.findChildByFieldName(node, "name")
	if nameNode == nil {
		return
	}
	className := nameNode.Content(src)
	id := filePath + "::" + className
	if seen[id] {
		return
	}
	seen[id] = true
	startLine := int(node.StartPoint().Row) + 1
	endLine := int(node.EndPoint().Row) + 1
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindType, Name: className,
		FilePath: filePath, StartLine: startLine, EndLine: endLine,
		Language: "php",
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileNode.ID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: startLine,
	})

	// Extract methods inside the class body.
	body := e.findChildByType(node, "declaration_list")
	if body == nil {
		return
	}
	for i := 0; i < int(body.NamedChildCount()); i++ {
		child := body.NamedChild(i)
		if child.Type() == "method_declaration" {
			e.extractMethod(child, src, filePath, fileNode, result, seen, className)
		}
	}
}

func (e *PHPExtractor) extractInterface(
	node *sitter.Node, src []byte,
	filePath string, fileNode *graph.Node,
	result *parser.ExtractionResult, seen map[string]bool,
) {
	nameNode := e.findChildByFieldName(node, "name")
	if nameNode == nil {
		return
	}
	ifaceName := nameNode.Content(src)
	id := filePath + "::" + ifaceName
	if seen[id] {
		return
	}
	seen[id] = true
	startLine := int(node.StartPoint().Row) + 1
	endLine := int(node.EndPoint().Row) + 1
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindInterface, Name: ifaceName,
		FilePath: filePath, StartLine: startLine, EndLine: endLine,
		Language: "php",
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileNode.ID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: startLine,
	})

	// Extract method signatures inside the interface body.
	body := e.findChildByType(node, "declaration_list")
	if body == nil {
		return
	}
	for i := 0; i < int(body.NamedChildCount()); i++ {
		child := body.NamedChild(i)
		if child.Type() == "method_declaration" {
			e.extractMethod(child, src, filePath, fileNode, result, seen, ifaceName)
		}
	}
}

func (e *PHPExtractor) extractFunction(
	node *sitter.Node, src []byte,
	filePath string, fileNode *graph.Node,
	result *parser.ExtractionResult, seen map[string]bool,
) {
	nameNode := e.findChildByFieldName(node, "name")
	if nameNode == nil {
		return
	}
	funcName := nameNode.Content(src)
	id := filePath + "::" + funcName
	if seen[id] {
		return
	}
	seen[id] = true
	startLine := int(node.StartPoint().Row) + 1
	endLine := int(node.EndPoint().Row) + 1
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindFunction, Name: funcName,
		FilePath: filePath, StartLine: startLine, EndLine: endLine,
		Language: "php",
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileNode.ID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: startLine,
	})

	// Extract call sites within the function body.
	body := e.findChildByType(node, "compound_statement")
	if body != nil {
		e.extractCallSites(body, src, filePath, id, result)
	}
}

func (e *PHPExtractor) extractMethod(
	node *sitter.Node, src []byte,
	filePath string, fileNode *graph.Node,
	result *parser.ExtractionResult, seen map[string]bool,
	className string,
) {
	nameNode := e.findChildByFieldName(node, "name")
	if nameNode == nil {
		return
	}
	methodName := nameNode.Content(src)
	id := filePath + "::" + className + "." + methodName
	startLine := int(node.StartPoint().Row) + 1
	endLine := int(node.EndPoint().Row) + 1
	if seen[id] {
		id = filePath + "::" + className + "." + methodName + "_L" + fmt.Sprint(startLine)
	}
	if seen[id] {
		return
	}
	seen[id] = true
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindMethod, Name: methodName,
		FilePath: filePath, StartLine: startLine, EndLine: endLine,
		Language: "php",
		Meta:     map[string]any{"receiver": className},
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileNode.ID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: startLine,
	})
	// MemberOf edge to containing class/interface.
	classID := filePath + "::" + className
	result.Edges = append(result.Edges, &graph.Edge{
		From: id, To: classID, Kind: graph.EdgeMemberOf, FilePath: filePath, Line: startLine,
	})

	// Extract call sites within the method body.
	body := e.findChildByType(node, "compound_statement")
	if body != nil {
		e.extractCallSites(body, src, filePath, id, result)
	}
}

func (e *PHPExtractor) extractUseImport(
	node *sitter.Node, src []byte,
	filePath string, fileNode *graph.Node,
	result *parser.ExtractionResult,
) {
	// use_declaration children can be namespace_use_clause or namespace_name.
	for i := 0; i < int(node.NamedChildCount()); i++ {
		child := node.NamedChild(i)
		var importPath string
		switch child.Type() {
		case "namespace_use_clause":
			nameNode := e.findChildByType(child, "qualified_name")
			if nameNode == nil {
				nameNode = e.findChildByType(child, "namespace_name")
			}
			if nameNode != nil {
				importPath = nameNode.Content(src)
			} else {
				importPath = child.Content(src)
			}
		case "qualified_name", "namespace_name":
			importPath = child.Content(src)
		default:
			continue
		}
		if importPath == "" {
			continue
		}
		importPath = strings.TrimLeft(importPath, "\\")
		importPath = strings.ReplaceAll(importPath, "\\", "/")
		line := int(child.StartPoint().Row) + 1
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileNode.ID, To: "unresolved::import::" + importPath,
			Kind: graph.EdgeImports, FilePath: filePath, Line: line,
		})
	}
}

func (e *PHPExtractor) extractRequireInclude(
	node *sitter.Node, src []byte,
	filePath string, fileNode *graph.Node,
	result *parser.ExtractionResult,
) {
	// Look for require, require_once, include, include_once expressions.
	for i := 0; i < int(node.NamedChildCount()); i++ {
		child := node.NamedChild(i)
		ct := child.Type()
		if ct == "require_expression" || ct == "require_once_expression" ||
			ct == "include_expression" || ct == "include_once_expression" {
			// The path is a string/encapsed_string containing a string_content child.
			path := e.extractStringContent(child, src)
			if path != "" {
				line := int(child.StartPoint().Row) + 1
				result.Edges = append(result.Edges, &graph.Edge{
					From: fileNode.ID, To: "unresolved::import::" + path,
					Kind: graph.EdgeImports, FilePath: filePath, Line: line,
				})
			}
		}
	}
}

func (e *PHPExtractor) extractCallSites(
	node *sitter.Node, src []byte,
	filePath string, callerID string,
	result *parser.ExtractionResult,
) {
	nodeType := node.Type()
	if nodeType == "function_call_expression" {
		funcNode := node.ChildByFieldName("function")
		if funcNode != nil {
			name := funcNode.Content(src)
			// Strip namespace prefix, keep just the function name.
			if idx := strings.LastIndex(name, "\\"); idx >= 0 {
				name = name[idx+1:]
			}
			line := int(node.StartPoint().Row) + 1
			result.Edges = append(result.Edges, &graph.Edge{
				From: callerID, To: "unresolved::*." + name,
				Kind: graph.EdgeCalls, FilePath: filePath, Line: line,
			})
		}
	} else if nodeType == "member_call_expression" || nodeType == "scoped_call_expression" {
		nameNode := node.ChildByFieldName("name")
		if nameNode != nil {
			name := nameNode.Content(src)
			line := int(node.StartPoint().Row) + 1
			result.Edges = append(result.Edges, &graph.Edge{
				From: callerID, To: "unresolved::*." + name,
				Kind: graph.EdgeCalls, FilePath: filePath, Line: line,
			})
		}
	}

	// Recurse into children.
	for i := 0; i < int(node.NamedChildCount()); i++ {
		child := node.NamedChild(i)
		e.extractCallSites(child, src, filePath, callerID, result)
	}
}

// findChildByType finds the first named child with the given type.
func (e *PHPExtractor) findChildByType(node *sitter.Node, typeName string) *sitter.Node {
	for i := 0; i < int(node.NamedChildCount()); i++ {
		child := node.NamedChild(i)
		if child.Type() == typeName {
			return child
		}
	}
	return nil
}

// findChildByFieldName finds a child by its field name.
func (e *PHPExtractor) findChildByFieldName(node *sitter.Node, fieldName string) *sitter.Node {
	return node.ChildByFieldName(fieldName)
}

// extractStringContent recursively finds the first string_content node and returns its text.
func (e *PHPExtractor) extractStringContent(node *sitter.Node, src []byte) string {
	if node.Type() == "string_content" {
		return node.Content(src)
	}
	for i := 0; i < int(node.NamedChildCount()); i++ {
		if result := e.extractStringContent(node.NamedChild(i), src); result != "" {
			return result
		}
	}
	return ""
}
