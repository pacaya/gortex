package languages

import (
	"fmt"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
	"github.com/zzet/gortex/internal/parser/tsitter/scala"
)

// ScalaExtractor extracts Scala source files.
type ScalaExtractor struct {
	lang *sitter.Language
}

func NewScalaExtractor() *ScalaExtractor {
	return &ScalaExtractor{lang: scala.GetLanguage()}
}

func (e *ScalaExtractor) Language() string     { return "scala" }
func (e *ScalaExtractor) Extensions() []string { return []string{".scala", ".sc"} }

func (e *ScalaExtractor) Extract(filePath string, src []byte) (*parser.ExtractionResult, error) {
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
		Language: "scala",
	}
	result.Nodes = append(result.Nodes, fileNode)

	seen := make(map[string]bool)
	annotationSeen := make(map[string]bool)

	// Walk the AST manually to extract all constructs.
	e.extractAll(root, src, filePath, fileNode, result, seen, annotationSeen)

	MaybeEnrichDatabricks(filePath, fileNode.ID, src, result)

	return result, nil
}

func (e *ScalaExtractor) extractAll(
	root *sitter.Node, src []byte, filePath string, fileNode *graph.Node,
	result *parser.ExtractionResult, seen, annotationSeen map[string]bool,
) {
	walkNodes(root, func(node *sitter.Node) {
		switch node.Type() {
		case "trait_definition":
			e.extractTrait(node, src, filePath, fileNode, result, seen, annotationSeen)
		case "class_definition":
			e.extractClass(node, src, filePath, fileNode, result, seen, annotationSeen)
		case "object_definition":
			e.extractObject(node, src, filePath, fileNode, result, seen, annotationSeen)
		case "enum_definition":
			e.extractEnum(node, src, filePath, fileNode, result, seen)
		case "import_declaration":
			e.extractImport(node, src, filePath, fileNode, result)
		case "function_definition", "function_declaration":
			// Only extract top-level functions (direct children of compilation_unit).
			if node.Parent() != nil && node.Parent().Type() == "compilation_unit" {
				e.extractTopLevelFunction(node, src, filePath, fileNode, result, seen, annotationSeen)
			}
		case "call_expression":
			e.extractCall(node, src, filePath, result)
		}
	})
}

// extractTrait extracts a trait as KindInterface with Meta["methods"].
func (e *ScalaExtractor) extractTrait(
	node *sitter.Node, src []byte, filePath string, fileNode *graph.Node,
	result *parser.ExtractionResult, seen, annotationSeen map[string]bool,
) {
	name := scalaFindChildIdentifier(node, src)
	if name == "" {
		return
	}
	id := filePath + "::" + name
	if seen[id] {
		return
	}
	seen[id] = true

	startLine := int(node.StartPoint().Row) + 1
	endLine := int(node.EndPoint().Row) + 1

	// Collect method names from the template_body.
	var methodNames []string
	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(i)
		if child.Type() == "template_body" {
			for j := 0; j < int(child.ChildCount()); j++ {
				member := child.Child(j)
				if member.Type() == "function_declaration" || member.Type() == "function_definition" {
					mName := scalaFindChildIdentifier(member, src)
					if mName != "" {
						methodNames = append(methodNames, mName)

						// Also emit method nodes and edges for methods inside the trait.
						mID := filePath + "::" + name + "." + mName
						mStartLine := int(member.StartPoint().Row) + 1
						mEndLine := int(member.EndPoint().Row) + 1
						if !seen[mID] {
							seen[mID] = true
							seen[filePath+"::_method_L"+fmt.Sprint(mStartLine)] = true
							result.Nodes = append(result.Nodes, &graph.Node{
								ID: mID, Kind: graph.KindMethod, Name: mName,
								FilePath: filePath, StartLine: mStartLine, EndLine: mEndLine,
								Language: "scala",
								Meta:     map[string]any{"receiver": name},
							})
							result.Edges = append(result.Edges, &graph.Edge{
								From: fileNode.ID, To: mID, Kind: graph.EdgeDefines,
								FilePath: filePath, Line: mStartLine,
							})
							result.Edges = append(result.Edges, &graph.Edge{
								From: mID, To: id, Kind: graph.EdgeMemberOf,
								FilePath: filePath, Line: mStartLine,
							})
							emitScalaAnnotationEdges(member, mID, filePath, src, result, annotationSeen)
						}
					}
				}
			}
		}
	}

	traitNode := &graph.Node{
		ID: id, Kind: graph.KindInterface, Name: name,
		FilePath: filePath, StartLine: startLine, EndLine: endLine,
		Language: "scala",
	}
	if len(methodNames) > 0 {
		traitNode.Meta = map[string]any{"methods": methodNames}
	}
	result.Nodes = append(result.Nodes, traitNode)
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileNode.ID, To: id, Kind: graph.EdgeDefines,
		FilePath: filePath, Line: startLine,
	})
	emitScalaAnnotationEdges(node, id, filePath, src, result, annotationSeen)
}

// extractClass extracts a class (including case class) as KindType.
func (e *ScalaExtractor) extractClass(
	node *sitter.Node, src []byte, filePath string, fileNode *graph.Node,
	result *parser.ExtractionResult, seen, annotationSeen map[string]bool,
) {
	name := scalaFindChildIdentifier(node, src)
	if name == "" {
		return
	}
	id := filePath + "::" + name
	if seen[id] {
		return
	}
	seen[id] = true

	startLine := int(node.StartPoint().Row) + 1
	endLine := int(node.EndPoint().Row) + 1

	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindType, Name: name,
		FilePath: filePath, StartLine: startLine, EndLine: endLine,
		Language: "scala",
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileNode.ID, To: id, Kind: graph.EdgeDefines,
		FilePath: filePath, Line: startLine,
	})
	emitScalaAnnotationEdges(node, id, filePath, src, result, annotationSeen)

	// Extract methods inside the class template_body.
	e.extractMembersFromBody(node, src, filePath, fileNode, id, name, result, seen, annotationSeen)
}

// extractObject extracts an object as KindType.
func (e *ScalaExtractor) extractObject(
	node *sitter.Node, src []byte, filePath string, fileNode *graph.Node,
	result *parser.ExtractionResult, seen, annotationSeen map[string]bool,
) {
	name := scalaFindChildIdentifier(node, src)
	if name == "" {
		return
	}
	id := filePath + "::" + name
	if seen[id] {
		return
	}
	seen[id] = true

	startLine := int(node.StartPoint().Row) + 1
	endLine := int(node.EndPoint().Row) + 1

	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindType, Name: name,
		FilePath: filePath, StartLine: startLine, EndLine: endLine,
		Language: "scala",
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileNode.ID, To: id, Kind: graph.EdgeDefines,
		FilePath: filePath, Line: startLine,
	})
	emitScalaAnnotationEdges(node, id, filePath, src, result, annotationSeen)

	// Extract methods inside the object template_body.
	e.extractMembersFromBody(node, src, filePath, fileNode, id, name, result, seen, annotationSeen)
}

// extractMembersFromBody extracts function_definition/function_declaration nodes
// from a template_body child as methods with EdgeMemberOf.
func (e *ScalaExtractor) extractMembersFromBody(
	parent *sitter.Node, src []byte, filePath string, fileNode *graph.Node,
	ownerID, ownerName string,
	result *parser.ExtractionResult, seen, annotationSeen map[string]bool,
) {
	for i := 0; i < int(parent.ChildCount()); i++ {
		child := parent.Child(i)
		if child.Type() != "template_body" {
			continue
		}
		for j := 0; j < int(child.ChildCount()); j++ {
			member := child.Child(j)
			switch member.Type() {
			case "val_definition", "var_definition", "val_declaration", "var_declaration":
				e.emitScalaField(member, src, filePath, fileNode, ownerID, ownerName, result, seen)
				continue
			case "function_definition", "function_declaration":
			default:
				continue
			}
			mName := scalaFindChildIdentifier(member, src)
			if mName == "" {
				continue
			}
			mID := filePath + "::" + ownerName + "." + mName
			mStartLine := int(member.StartPoint().Row) + 1
			mEndLine := int(member.EndPoint().Row) + 1
			if seen[mID] {
				mID = filePath + "::" + ownerName + "." + mName + "_L" + fmt.Sprint(mStartLine)
			}
			if seen[mID] {
				continue
			}
			seen[mID] = true
			seen[filePath+"::_method_L"+fmt.Sprint(mStartLine)] = true
			mMeta := map[string]any{"receiver": ownerName}
			if rt := scalaReturnType(member, src); rt != "" {
				mMeta["return_type"] = rt
			}
			result.Nodes = append(result.Nodes, &graph.Node{
				ID: mID, Kind: graph.KindMethod, Name: mName,
				FilePath: filePath, StartLine: mStartLine, EndLine: mEndLine,
				Language: "scala",
				Meta:     mMeta,
			})
			result.Edges = append(result.Edges, &graph.Edge{
				From: fileNode.ID, To: mID, Kind: graph.EdgeDefines,
				FilePath: filePath, Line: mStartLine,
			})
			result.Edges = append(result.Edges, &graph.Edge{
				From: mID, To: ownerID, Kind: graph.EdgeMemberOf,
				FilePath: filePath, Line: mStartLine,
			})
			emitScalaAnnotationEdges(member, mID, filePath, src, result, annotationSeen)
		}
	}
}

// extractEnum extracts a Scala 3 enum as a type with kind "enum" and emits each
// of its cases (simple or full) as an enum-member of it.
func (e *ScalaExtractor) extractEnum(node *sitter.Node, src []byte, filePath string, fileNode *graph.Node, result *parser.ExtractionResult, seen map[string]bool) {
	name := scalaFindChildIdentifier(node, src)
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
		ID: id, Kind: graph.KindType, Name: name,
		FilePath: filePath, StartLine: startLine, EndLine: int(node.EndPoint().Row) + 1,
		Language: "scala", Meta: map[string]any{"kind": "enum"},
	})
	result.Edges = append(result.Edges, &graph.Edge{From: fileNode.ID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: startLine})

	walkNodes(node, func(n *sitter.Node) {
		if n.Type() != "simple_enum_case" && n.Type() != "full_enum_case" {
			return
		}
		caseName := scalaFindChildIdentifier(n, src)
		if caseName == "" {
			return
		}
		caseID := id + "." + caseName
		if seen[caseID] {
			return
		}
		seen[caseID] = true
		cl := int(n.StartPoint().Row) + 1
		result.Nodes = append(result.Nodes, &graph.Node{
			ID: caseID, Kind: graph.KindEnumMember, Name: caseName,
			FilePath: filePath, StartLine: cl, EndLine: int(n.EndPoint().Row) + 1,
			Language: "scala", Meta: map[string]any{"receiver": name},
		})
		result.Edges = append(result.Edges, &graph.Edge{From: fileNode.ID, To: caseID, Kind: graph.EdgeDefines, FilePath: filePath, Line: cl})
		result.Edges = append(result.Edges, &graph.Edge{From: caseID, To: id, Kind: graph.EdgeMemberOf, FilePath: filePath, Line: cl})
	})
}

// emitScalaField emits a val/var member as a field of its enclosing type, with a
// typed-as reference to its declared type when annotated.
func (e *ScalaExtractor) emitScalaField(member *sitter.Node, src []byte, filePath string, fileNode *graph.Node, ownerID, ownerName string, result *parser.ExtractionResult, seen map[string]bool) {
	name := scalaFindChildIdentifier(member, src)
	if name == "" {
		return
	}
	id := filePath + "::" + ownerName + "." + name
	if seen[id] {
		return
	}
	seen[id] = true
	line := int(member.StartPoint().Row) + 1
	meta := map[string]any{"receiver": ownerName}
	if t := member.Type(); t == "var_definition" || t == "var_declaration" {
		meta["mutable"] = true
	}
	typ := scalaTypeAnnotation(member, src)
	if typ != "" {
		meta["field_type"] = typ
	}
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindField, Name: name,
		FilePath: filePath, StartLine: line, EndLine: int(member.EndPoint().Row) + 1,
		Language: "scala", Meta: meta,
	})
	result.Edges = append(result.Edges, &graph.Edge{From: fileNode.ID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: line})
	result.Edges = append(result.Edges, &graph.Edge{From: id, To: ownerID, Kind: graph.EdgeMemberOf, FilePath: filePath, Line: line})
	if typ != "" {
		result.Edges = append(result.Edges, &graph.Edge{From: id, To: "unresolved::" + typ, Kind: graph.EdgeTypedAs, FilePath: filePath, Line: line})
	}
}

// scalaTypeAnnotation returns the base type named in a `val/var name: Type = ...`
// declaration (generics and dotted prefix stripped), or "".
func scalaTypeAnnotation(member *sitter.Node, src []byte) string {
	header := scalaDeclHeader(member, src)
	colon := strings.IndexByte(header, ':')
	if colon < 0 {
		return ""
	}
	return scalaBaseType(header[colon+1:])
}

// scalaReturnType returns the declared return type of a `def name(...): Type`,
// or "". Best-effort: the return colon is taken after the parameter list.
func scalaReturnType(member *sitter.Node, src []byte) string {
	header := scalaDeclHeader(member, src)
	region := header
	if rp := strings.LastIndexByte(header, ')'); rp >= 0 {
		region = header[rp+1:]
	}
	colon := strings.IndexByte(region, ':')
	if colon < 0 {
		return ""
	}
	return scalaBaseType(region[colon+1:])
}

// scalaDeclHeader returns the first line of a declaration up to its initializer.
func scalaDeclHeader(member *sitter.Node, src []byte) string {
	text := member.Content(src)
	if i := strings.IndexByte(text, '\n'); i >= 0 {
		text = text[:i]
	}
	if i := strings.IndexByte(text, '='); i >= 0 {
		text = text[:i]
	}
	return text
}

// scalaBaseType reduces a type expression to its base name (generic args and
// dotted prefix stripped).
func scalaBaseType(s string) string {
	t := strings.TrimSpace(s)
	if i := strings.IndexByte(t, '['); i >= 0 {
		t = t[:i]
	}
	if i := strings.LastIndexByte(t, '.'); i >= 0 {
		t = t[i+1:]
	}
	return strings.TrimSpace(t)
}

// extractImport extracts an import_declaration, building the path from identifier children.
func (e *ScalaExtractor) extractImport(
	node *sitter.Node, src []byte, filePath string, fileNode *graph.Node,
	result *parser.ExtractionResult,
) {
	var parts []string
	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(i)
		if child.Type() == "identifier" {
			parts = append(parts, child.Content(src))
		}
	}
	if len(parts) == 0 {
		return
	}
	importPath := strings.Join(parts, "/")
	startLine := int(node.StartPoint().Row) + 1
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileNode.ID, To: "unresolved::import::" + importPath,
		Kind: graph.EdgeImports, FilePath: filePath, Line: startLine,
	})
}

// extractTopLevelFunction extracts a function defined at the top level (not in a class/object/trait).
func (e *ScalaExtractor) extractTopLevelFunction(
	node *sitter.Node, src []byte, filePath string, fileNode *graph.Node,
	result *parser.ExtractionResult, seen, annotationSeen map[string]bool,
) {
	name := scalaFindChildIdentifier(node, src)
	if name == "" {
		return
	}
	startLine := int(node.StartPoint().Row) + 1
	lineKey := filePath + "::_method_L" + fmt.Sprint(startLine)
	if seen[lineKey] {
		return
	}
	id := filePath + "::" + name
	if seen[id] {
		id = filePath + "::" + name + "_L" + fmt.Sprint(startLine)
	}
	if seen[id] {
		return
	}
	seen[id] = true
	endLine := int(node.EndPoint().Row) + 1
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindFunction, Name: name,
		FilePath: filePath, StartLine: startLine, EndLine: endLine,
		Language: "scala",
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileNode.ID, To: id, Kind: graph.EdgeDefines,
		FilePath: filePath, Line: startLine,
	})
	emitScalaAnnotationEdges(node, id, filePath, src, result, annotationSeen)
}

// extractCall extracts a call_expression.
func (e *ScalaExtractor) extractCall(
	node *sitter.Node, src []byte, filePath string,
	result *parser.ExtractionResult,
) {
	// The callee is the first child — either an identifier or a field_expression.
	if node.ChildCount() == 0 {
		return
	}
	callee := node.Child(0)
	var callName string
	switch callee.Type() {
	case "identifier":
		callName = callee.Content(src)
	case "field_expression":
		// field_expression has children: object, ".", field_name (identifier)
		// The last identifier child is the method name.
		for i := int(callee.ChildCount()) - 1; i >= 0; i-- {
			fc := callee.Child(i)
			if fc.Type() == "identifier" {
				callName = fc.Content(src)
				break
			}
		}
	default:
		return
	}
	if callName == "" {
		return
	}

	startLine := int(node.StartPoint().Row) + 1
	funcRanges := buildFuncRanges(result)
	callerID := findEnclosingFunc(funcRanges, startLine)
	if callerID == "" {
		return
	}
	result.Edges = append(result.Edges, &graph.Edge{
		From: callerID, To: "unresolved::*." + callName,
		Kind: graph.EdgeCalls, FilePath: filePath, Line: startLine,
	})
}

// scalaFindChildIdentifier finds the first direct child of type "identifier"
// and returns its text content.
func scalaFindChildIdentifier(node *sitter.Node, src []byte) string {
	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(i)
		if child.Type() == "identifier" {
			return child.Content(src)
		}
	}
	return ""
}
