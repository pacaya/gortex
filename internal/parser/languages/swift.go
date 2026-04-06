package languages

import (
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/swift"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

const (
	swQClass = `(class_declaration
		name: (type_identifier) @type.name) @type.def`

	swQStruct = `(struct_declaration
		name: (type_identifier) @type.name) @type.def`

	swQEnum = `(enum_declaration
		name: (type_identifier) @type.name) @type.def`

	swQProtocol = `(protocol_declaration
		name: (type_identifier) @proto.name) @proto.def`

	swQFunction = `(function_declaration
		name: (simple_identifier) @func.name) @func.def`

	swQImport = `(import_declaration) @import.def`

	swQCall = `(call_expression
		(simple_identifier) @call.name) @call.expr`

	swQProtocolMethod = `(protocol_declaration
		name: (type_identifier) @proto.name
		body: (protocol_body
			(protocol_function_declaration
				name: (simple_identifier) @proto.method.name)))`
)

// SwiftExtractor extracts Swift source files.
type SwiftExtractor struct {
	lang *sitter.Language
}

func NewSwiftExtractor() *SwiftExtractor {
	return &SwiftExtractor{lang: swift.GetLanguage()}
}

func (e *SwiftExtractor) Language() string     { return "swift" }
func (e *SwiftExtractor) Extensions() []string { return []string{".swift"} }

func (e *SwiftExtractor) Extract(filePath string, src []byte) (*parser.ExtractionResult, error) {
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
		Language: "swift",
	}
	result.Nodes = append(result.Nodes, fileNode)

	seen := make(map[string]bool)

	// Collect type body ranges for method detection.
	// A function_declaration inside a class/struct/enum body is a method.
	typeBodyRanges := e.collectTypeBodyRanges(root, src)

	// Functions — extract all, then classify as method or function based on enclosing type.
	matches, _ := parser.RunQuery(swQFunction, e.lang, root, src)
	for _, m := range matches {
		name := m.Captures["func.name"].Text
		def := m.Captures["func.def"]
		startLine := def.StartLine

		// Check if this function is inside a type body.
		if typeName, ok := e.findEnclosingType(typeBodyRanges, startLine); ok {
			// It's a method.
			id := filePath + "::" + typeName + "." + name
			if seen[id] {
				continue
			}
			seen[id] = true
			result.Nodes = append(result.Nodes, &graph.Node{
				ID: id, Kind: graph.KindMethod, Name: name,
				FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
				Language: "swift", Meta: map[string]any{
					"receiver":  typeName,
					"signature": "func " + name + "(...)",
				},
			})
			result.Edges = append(result.Edges, &graph.Edge{
				From: fileNode.ID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
			})
			typeID := filePath + "::" + typeName
			result.Edges = append(result.Edges, &graph.Edge{
				From: id, To: typeID, Kind: graph.EdgeMemberOf, FilePath: filePath, Line: def.StartLine + 1,
			})
		} else {
			// It's a free function.
			id := filePath + "::" + name
			if seen[id] {
				continue
			}
			seen[id] = true
			result.Nodes = append(result.Nodes, &graph.Node{
				ID: id, Kind: graph.KindFunction, Name: name,
				FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
				Language: "swift", Meta: map[string]any{"signature": "func " + name + "(...)"},
			})
			result.Edges = append(result.Edges, &graph.Edge{
				From: fileNode.ID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
			})
		}
	}

	// Classes.
	matches, _ = parser.RunQuery(swQClass, e.lang, root, src)
	for _, m := range matches {
		name := m.Captures["type.name"].Text
		def := m.Captures["type.def"]
		id := filePath + "::" + name
		if seen[id] {
			continue
		}
		seen[id] = true
		result.Nodes = append(result.Nodes, &graph.Node{
			ID: id, Kind: graph.KindType, Name: name,
			FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
			Language: "swift",
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileNode.ID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
		})
	}

	// Structs.
	matches, _ = parser.RunQuery(swQStruct, e.lang, root, src)
	for _, m := range matches {
		name := m.Captures["type.name"].Text
		def := m.Captures["type.def"]
		id := filePath + "::" + name
		if seen[id] {
			continue
		}
		seen[id] = true
		result.Nodes = append(result.Nodes, &graph.Node{
			ID: id, Kind: graph.KindType, Name: name,
			FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
			Language: "swift",
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileNode.ID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
		})
	}

	// Enums.
	matches, _ = parser.RunQuery(swQEnum, e.lang, root, src)
	for _, m := range matches {
		name := m.Captures["type.name"].Text
		def := m.Captures["type.def"]
		id := filePath + "::" + name
		if seen[id] {
			continue
		}
		seen[id] = true
		result.Nodes = append(result.Nodes, &graph.Node{
			ID: id, Kind: graph.KindType, Name: name,
			FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
			Language: "swift",
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileNode.ID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
		})
	}

	// Protocol method specs (collect before creating protocol nodes).
	protoMethods := make(map[string][]string)
	matches, _ = parser.RunQuery(swQProtocolMethod, e.lang, root, src)
	for _, m := range matches {
		pName := m.Captures["proto.name"].Text
		mName := m.Captures["proto.method.name"].Text
		protoMethods[pName] = append(protoMethods[pName], mName)
	}

	// Protocols.
	matches, _ = parser.RunQuery(swQProtocol, e.lang, root, src)
	for _, m := range matches {
		name := m.Captures["proto.name"].Text
		def := m.Captures["proto.def"]
		id := filePath + "::" + name
		if seen[id] {
			continue
		}
		seen[id] = true
		meta := map[string]any{}
		if methods, ok := protoMethods[name]; ok {
			meta["methods"] = methods
		}
		result.Nodes = append(result.Nodes, &graph.Node{
			ID: id, Kind: graph.KindInterface, Name: name,
			FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
			Language: "swift", Meta: meta,
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileNode.ID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
		})
	}

	// Imports.
	matches, _ = parser.RunQuery(swQImport, e.lang, root, src)
	for _, m := range matches {
		def := m.Captures["import.def"]
		importText := strings.TrimSpace(def.Text)
		importText = strings.TrimPrefix(importText, "import ")
		importText = strings.TrimSpace(importText)
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileNode.ID, To: "unresolved::import::" + importText,
			Kind: graph.EdgeImports, FilePath: filePath, Line: def.StartLine + 1,
		})
	}

	// Call sites.
	funcRanges := buildFuncRanges(result)
	matches, _ = parser.RunQuery(swQCall, e.lang, root, src)
	for _, m := range matches {
		name := m.Captures["call.name"].Text
		expr := m.Captures["call.expr"]
		callerID := findEnclosingFunc(funcRanges, expr.StartLine+1)
		if callerID == "" {
			continue
		}
		result.Edges = append(result.Edges, &graph.Edge{
			From: callerID, To: "unresolved::" + name,
			Kind: graph.EdgeCalls, FilePath: filePath, Line: expr.StartLine + 1,
		})
	}

	return result, nil
}

type typeBodyRange struct {
	typeName  string
	startLine int // 0-based
	endLine   int // 0-based
}

func (e *SwiftExtractor) collectTypeBodyRanges(root *sitter.Node, src []byte) []typeBodyRange {
	var ranges []typeBodyRange
	for _, q := range []string{swQClass, swQStruct, swQEnum} {
		matches, _ := parser.RunQuery(q, e.lang, root, src)
		for _, m := range matches {
			name := m.Captures["type.name"].Text
			def := m.Captures["type.def"]
			ranges = append(ranges, typeBodyRange{
				typeName:  name,
				startLine: def.StartLine,
				endLine:   def.EndLine,
			})
		}
	}
	return ranges
}

func (e *SwiftExtractor) findEnclosingType(ranges []typeBodyRange, line int) (string, bool) {
	// Find the most specific (innermost) enclosing type.
	best := ""
	bestSize := int(^uint(0) >> 1) // max int
	for _, r := range ranges {
		if line >= r.startLine && line <= r.endLine {
			size := r.endLine - r.startLine
			if size < bestSize {
				bestSize = size
				best = r.typeName
			}
		}
	}
	if best == "" {
		return "", false
	}
	return best, true
}
