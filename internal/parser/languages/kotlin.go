package languages

import (
	"fmt"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/kotlin"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

const (
	kotlinQClass = `(class_declaration
		(type_identifier) @class.name) @class.def`

	kotlinQObject = `(object_declaration
		(type_identifier) @obj.name) @obj.def`

	kotlinQInterface = `(class_declaration
		(type_identifier) @iface.name) @iface.def`

	kotlinQFunction = `(function_declaration
		(simple_identifier) @func.name) @func.def`

	kotlinQClassMethod = `(class_declaration
		(type_identifier) @class.name
		(class_body
			(function_declaration
				(simple_identifier) @method.name) @method.def))`

	kotlinQObjectMethod = `(object_declaration
		(type_identifier) @obj.name
		(class_body
			(function_declaration
				(simple_identifier) @method.name) @method.def))`

	kotlinQImport = `(import_header
		(identifier) @import.path) @import.def`

	kotlinQCall = `(call_expression
		(simple_identifier) @call.name) @call.expr`

	kotlinQProperty = `(property_declaration
		(variable_declaration
			(simple_identifier) @prop.name)) @prop.def`
)

// KotlinExtractor extracts Kotlin source files.
type KotlinExtractor struct {
	lang *sitter.Language
}

func NewKotlinExtractor() *KotlinExtractor {
	return &KotlinExtractor{lang: kotlin.GetLanguage()}
}

func (e *KotlinExtractor) Language() string     { return "kotlin" }
func (e *KotlinExtractor) Extensions() []string { return []string{".kt", ".kts"} }

func (e *KotlinExtractor) Extract(filePath string, src []byte) (*parser.ExtractionResult, error) {
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
		Language: "kotlin",
	}
	result.Nodes = append(result.Nodes, fileNode)

	seen := make(map[string]bool)

	// Classes (class, data class).
	// We need to distinguish classes from interfaces. In the Kotlin tree-sitter grammar,
	// both use class_declaration. Interfaces have "interface" as a keyword child.
	// We'll use a manual walk approach for this distinction.
	e.extractClassesAndInterfaces(root, src, filePath, fileNode, result, seen)

	// Object declarations.
	matches, _ := parser.RunQuery(kotlinQObject, e.lang, root, src)
	for _, m := range matches {
		name := m.Captures["obj.name"].Text
		def := m.Captures["obj.def"]
		id := filePath + "::" + name
		if seen[id] {
			continue
		}
		seen[id] = true
		result.Nodes = append(result.Nodes, &graph.Node{
			ID: id, Kind: graph.KindType, Name: name,
			FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
			Language: "kotlin",
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileNode.ID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
		})
	}

	// Methods inside object declarations.
	matches, _ = parser.RunQuery(kotlinQObjectMethod, e.lang, root, src)
	for _, m := range matches {
		objName := m.Captures["obj.name"].Text
		name := m.Captures["method.name"].Text
		def := m.Captures["method.def"]
		id := filePath + "::" + objName + "." + name
		if seen[id] {
			id = filePath + "::" + objName + "." + name + "_L" + fmt.Sprint(def.StartLine+1)
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		seen[filePath+"::_method_L"+fmt.Sprint(def.StartLine+1)] = true
		result.Nodes = append(result.Nodes, &graph.Node{
			ID: id, Kind: graph.KindMethod, Name: name,
			FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
			Language: "kotlin",
			Meta:     map[string]any{"receiver": objName},
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileNode.ID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
		})
		objID := filePath + "::" + objName
		result.Edges = append(result.Edges, &graph.Edge{
			From: id, To: objID, Kind: graph.EdgeMemberOf, FilePath: filePath, Line: def.StartLine + 1,
		})
	}

	// Methods inside class declarations (already extracted via extractClassesAndInterfaces helper for class membership).
	matches, _ = parser.RunQuery(kotlinQClassMethod, e.lang, root, src)
	for _, m := range matches {
		className := m.Captures["class.name"].Text
		name := m.Captures["method.name"].Text
		def := m.Captures["method.def"]
		id := filePath + "::" + className + "." + name
		if seen[id] {
			id = filePath + "::" + className + "." + name + "_L" + fmt.Sprint(def.StartLine+1)
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		seen[filePath+"::_method_L"+fmt.Sprint(def.StartLine+1)] = true
		result.Nodes = append(result.Nodes, &graph.Node{
			ID: id, Kind: graph.KindMethod, Name: name,
			FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
			Language: "kotlin",
			Meta:     map[string]any{"receiver": className},
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileNode.ID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
		})
		classID := filePath + "::" + className
		result.Edges = append(result.Edges, &graph.Edge{
			From: id, To: classID, Kind: graph.EdgeMemberOf, FilePath: filePath, Line: def.StartLine + 1,
		})
	}

	// Top-level functions (fallback: skip those already found in class/object bodies).
	matches, _ = parser.RunQuery(kotlinQFunction, e.lang, root, src)
	for _, m := range matches {
		name := m.Captures["func.name"].Text
		def := m.Captures["func.def"]
		lineKey := filePath + "::_method_L" + fmt.Sprint(def.StartLine+1)
		if seen[lineKey] {
			continue
		}
		id := filePath + "::" + name
		if seen[id] {
			id = filePath + "::" + name + "_L" + fmt.Sprint(def.StartLine+1)
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		result.Nodes = append(result.Nodes, &graph.Node{
			ID: id, Kind: graph.KindFunction, Name: name,
			FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
			Language: "kotlin",
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileNode.ID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
		})
	}

	// Top-level properties (val/var not inside a class).
	matches, _ = parser.RunQuery(kotlinQProperty, e.lang, root, src)
	for _, m := range matches {
		name := m.Captures["prop.name"].Text
		def := m.Captures["prop.def"]
		// Only include top-level properties (direct children of source_file).
		if def.Node.Parent() != nil && def.Node.Parent().Type() == "source_file" {
			id := filePath + "::" + name
			if seen[id] {
				continue
			}
			seen[id] = true
			result.Nodes = append(result.Nodes, &graph.Node{
				ID: id, Kind: graph.KindVariable, Name: name,
				FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
				Language: "kotlin",
			})
			result.Edges = append(result.Edges, &graph.Edge{
				From: fileNode.ID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
			})
		}
	}

	// Imports.
	matches, _ = parser.RunQuery(kotlinQImport, e.lang, root, src)
	for _, m := range matches {
		path := m.Captures["import.path"]
		importPath := strings.ReplaceAll(path.Text, ".", "/")
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileNode.ID, To: "unresolved::import::" + importPath,
			Kind: graph.EdgeImports, FilePath: filePath, Line: path.StartLine + 1,
		})
	}

	// Call sites.
	funcRanges := buildFuncRanges(result)
	matches, _ = parser.RunQuery(kotlinQCall, e.lang, root, src)
	for _, m := range matches {
		name := m.Captures["call.name"].Text
		expr := m.Captures["call.expr"]
		callerID := findEnclosingFunc(funcRanges, expr.StartLine+1)
		if callerID == "" {
			continue
		}
		result.Edges = append(result.Edges, &graph.Edge{
			From: callerID, To: "unresolved::*." + name,
			Kind: graph.EdgeCalls, FilePath: filePath, Line: expr.StartLine + 1,
		})
	}

	return result, nil
}

// extractClassesAndInterfaces walks the root to distinguish class_declaration
// nodes that are interfaces vs classes. In the Kotlin tree-sitter grammar,
// both classes and interfaces use class_declaration, but interfaces have
// the "interface" keyword as the first child token.
func (e *KotlinExtractor) extractClassesAndInterfaces(
	root *sitter.Node, src []byte, filePath string, fileNode *graph.Node,
	result *parser.ExtractionResult, seen map[string]bool,
) {
	walkNodes(root, func(node *sitter.Node) {
		if node.Type() != "class_declaration" {
			return
		}

		// Find the type_identifier child for the name.
		var name string
		for i := 0; i < int(node.ChildCount()); i++ {
			child := node.Child(i)
			if child.Type() == "type_identifier" {
				name = child.Content(src)
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

		// Determine if this is an interface by checking for "interface" keyword.
		isInterface := false
		for i := 0; i < int(node.ChildCount()); i++ {
			child := node.Child(i)
			if child.Type() == "interface" {
				isInterface = true
				break
			}
		}

		kind := graph.KindType
		if isInterface {
			kind = graph.KindInterface
		}

		seen[id] = true
		startLine := int(node.StartPoint().Row) + 1
		endLine := int(node.EndPoint().Row) + 1
		result.Nodes = append(result.Nodes, &graph.Node{
			ID: id, Kind: kind, Name: name,
			FilePath: filePath, StartLine: startLine, EndLine: endLine,
			Language: "kotlin",
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileNode.ID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: startLine,
		})
	})
}

// walkNodes does a depth-first walk of the tree-sitter node tree.
func walkNodes(node *sitter.Node, fn func(*sitter.Node)) {
	fn(node)
	for i := 0; i < int(node.ChildCount()); i++ {
		walkNodes(node.Child(i), fn)
	}
}
