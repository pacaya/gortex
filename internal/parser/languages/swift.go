package languages

import (
	"regexp"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
	"github.com/zzet/gortex/internal/parser/tsitter/swift"
)

// qSwiftAll is a single tree-sitter query alternating over every
// pattern the Swift extractor needs. One tree walk per file replaces
// the 9 `parser.RunQuery` calls (plus the duplicated triple-query pass
// the legacy collectTypeBodyRanges performed). Capture names are
// disjoint across patterns so the dispatch in Extract can branch on
// which name is set. Method-vs-function classification is performed
// inline by tracking class/struct/enum line ranges as their match
// arrives — types come before their members in document order, so the
// range table is always complete by the time a function_declaration is
// dispatched.
const qSwiftAll = `
[
  (class_declaration
    name: (type_identifier) @class.name) @class.def

  (class_declaration
    name: (type_identifier) @enum.name
    body: (enum_class_body) @enum.body) @enum.def

  (protocol_declaration
    name: (type_identifier) @proto.name) @proto.def

  (protocol_function_declaration
    name: (simple_identifier) @protomethod.name) @protomethod.def

  (function_declaration
    name: (simple_identifier) @func.name) @func.def

  (import_declaration) @import.def

  (call_expression
    (simple_identifier) @call.name) @call.expr
]
`

// SwiftExtractor extracts Swift source files into graph nodes and edges.
type SwiftExtractor struct {
	lang *sitter.Language
	qAll *parser.PreparedQuery
}

func NewSwiftExtractor() *SwiftExtractor {
	lang := swift.GetLanguage()
	return &SwiftExtractor{
		lang: lang,
		qAll: parser.MustPreparedQuery(qSwiftAll, lang),
	}
}

func (e *SwiftExtractor) Language() string     { return "swift" }
func (e *SwiftExtractor) Extensions() []string { return []string{".swift"} }

// --- Deferred match buffers ----------------------------------------

type swiftDeferredCall struct {
	name string
	line int
}

type swiftTypeRange struct {
	name      string
	startLine int // 0-based
	endLine   int // 0-based
}

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
	fileID := fileNode.ID
	result.Nodes = append(result.Nodes, fileNode)

	seen := make(map[string]bool)
	annotationSeen := make(map[string]bool)
	protoMethods := make(map[string][]string) // protocol name → declared method names
	var typeRanges []swiftTypeRange
	// Extensions aren't captured by qSwiftAll (their name is a user_type, not a
	// type_identifier), so seed their ranges first; members inside an
	// `extension Foo { ... }` then attribute to Foo like any other type member.
	typeRanges = append(typeRanges, swiftExtensionRanges(src)...)
	var calls []swiftDeferredCall

	parser.EachMatch(e.qAll, root, src, func(m parser.QueryResult) {
		switch {

		case m.Captures["class.def"] != nil:
			e.emitTypeContainer(m, "class", filePath, fileID, src, result, seen, annotationSeen, &typeRanges, nil)

		case m.Captures["enum.def"] != nil:
			// May fire on the same class_declaration as the prior
			// class.def pattern; emitTypeContainer handles the seen
			// dedupe and stamps Meta["kind"]="enum" on the existing
			// node when it does. Walks the captured enum_class_body
			// for case entries.
			body := m.Captures["enum.body"]
			var bodyNode *sitter.Node
			if body != nil {
				bodyNode = body.Node
			}
			e.emitTypeContainer(m, "enum", filePath, fileID, src, result, seen, annotationSeen, &typeRanges, bodyNode)

		case m.Captures["proto.def"] != nil:
			e.emitProtocol(m, filePath, fileID, src, result, seen, annotationSeen)

		case m.Captures["protomethod.def"] != nil:
			e.recordProtocolMethod(m, src, protoMethods)

		case m.Captures["func.def"] != nil:
			e.emitFunction(m, filePath, fileID, src, result, seen, annotationSeen, typeRanges)

		case m.Captures["import.def"] != nil:
			e.emitImport(m, filePath, fileID, result)

		case m.Captures["call.expr"] != nil:
			expr := m.Captures["call.expr"]
			calls = append(calls, swiftDeferredCall{
				name: m.Captures["call.name"].Text,
				line: expr.StartLine + 1,
			})
		}
	})

	// Stamp protocol method names onto protocol nodes' Meta["methods"].
	for _, n := range result.Nodes {
		if n.Kind != graph.KindInterface {
			continue
		}
		if methods, ok := protoMethods[n.Name]; ok {
			if n.Meta == nil {
				n.Meta = make(map[string]any)
			}
			n.Meta["methods"] = methods
		}
	}

	// Resolve calls against funcRanges.
	funcRanges := buildFuncRanges(result)
	for _, c := range calls {
		callerID := findEnclosingFunc(funcRanges, c.line)
		if callerID == "" {
			continue
		}
		result.Edges = append(result.Edges, &graph.Edge{
			From: callerID, To: "unresolved::" + c.name,
			Kind: graph.EdgeCalls, FilePath: filePath, Line: c.line,
		})
	}

	// Expo Modules native DSL (Name/Function/AsyncFunction) → synthetic
	// JS-callable method nodes for the Expo bridge synthesizer.
	emitExpoModuleNodes(src, filePath, "swift", fileID, result, seen)

	return result, nil
}

// --- Per-match emit helpers -----------------------------------------

// emitTypeContainer emits a class / struct / enum node and records its
// line range so subsequent function_declaration dispatches can classify
// methods by enclosing type. The capture-name prefix selects which
// name/def pair to read. For the "enum" prefix, when the same id is
// already seen (i.e. swQClass already emitted it), stamps
// Meta["kind"]="enum" on the existing node and walks bodyNode for
// case entries instead of emitting a duplicate.
func (e *SwiftExtractor) emitTypeContainer(m parser.QueryResult, prefix, filePath, fileID string, src []byte, result *parser.ExtractionResult, seen, annotationSeen map[string]bool, typeRanges *[]swiftTypeRange, bodyNode *sitter.Node) {
	var nameKey, defKey string
	switch prefix {
	case "enum":
		nameKey, defKey = "enum.name", "enum.def"
	default:
		nameKey, defKey = "class.name", "class.def"
	}
	name := m.Captures[nameKey].Text
	def := m.Captures[defKey]
	id := filePath + "::" + name

	// Always extend the type-range table — this is what method
	// classification consults. Adding the same id twice (once for
	// class.def, once for enum.def on the same enum) is harmless: the
	// findEnclosingType lookup picks the innermost match by size.
	*typeRanges = append(*typeRanges, swiftTypeRange{
		name:      name,
		startLine: def.StartLine,
		endLine:   def.EndLine,
	})

	if !seen[id] {
		seen[id] = true
		meta := map[string]any{"visibility": swiftVisibility(def.Node, src)}
		if prefix == "enum" {
			meta["kind"] = "enum"
		}
		if doc := ExtractDocAbove(src, def.StartLine, DocLangSlashSlash); doc != "" {
			meta["doc"] = doc
		}
		result.Nodes = append(result.Nodes, &graph.Node{
			ID: id, Kind: graph.KindType, Name: name,
			FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
			Language: "swift",
			Meta:     meta,
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
		})
		emitSwiftAnnotationEdges(def.Node, id, filePath, src, result, annotationSeen)
	} else if prefix == "enum" {
		// Backfill enum kind on the existing node.
		for _, n := range result.Nodes {
			if n.ID == id {
				if n.Meta == nil {
					n.Meta = make(map[string]any)
				}
				n.Meta["kind"] = "enum"
				break
			}
		}
	}

	// Enum cases — cases with associated values contain nested
	// simple_identifier labels (`case labeled(x: Int)` has `x` as a
	// simple_identifier), so we take *only the first* simple_identifier
	// child of each enum_entry as the case name.
	if prefix != "enum" || bodyNode == nil {
		return
	}
	for i := 0; i < int(bodyNode.ChildCount()); i++ {
		entry := bodyNode.Child(i)
		if entry == nil || entry.Type() != "enum_entry" {
			continue
		}
		var caseName string
		for j := 0; j < int(entry.ChildCount()); j++ {
			ch := entry.Child(j)
			if ch != nil && ch.Type() == "simple_identifier" {
				caseName = ch.Content(src)
				break
			}
		}
		if caseName == "" {
			continue
		}
		caseID := id + "." + caseName
		result.Nodes = append(result.Nodes, &graph.Node{
			ID: caseID, Kind: graph.KindVariable, Name: caseName,
			FilePath:  filePath,
			StartLine: int(entry.StartPoint().Row) + 1,
			EndLine:   int(entry.EndPoint().Row) + 1,
			Language:  "swift",
			Meta:      map[string]any{"receiver": name, "kind": "enum_case"},
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From: caseID, To: id, Kind: graph.EdgeMemberOf,
			FilePath: filePath, Line: int(entry.StartPoint().Row) + 1,
		})
	}
}

// recordProtocolMethod walks up to the enclosing protocol_declaration
// and appends the method name to its Meta["methods"] entry. Mirrors
// legacy swQProtocolMethod nested capture.
func (e *SwiftExtractor) recordProtocolMethod(m parser.QueryResult, src []byte, protoMethods map[string][]string) {
	def := m.Captures["protomethod.def"]
	if def.Node == nil {
		return
	}
	protoNode := findEnclosingSwiftContainer(def.Node, "protocol_declaration")
	if protoNode == nil {
		return
	}
	nameNode := protoNode.ChildByFieldName("name")
	if nameNode == nil {
		return
	}
	protoMethods[nameNode.Content(src)] = append(protoMethods[nameNode.Content(src)], m.Captures["protomethod.name"].Text)
}

func (e *SwiftExtractor) emitProtocol(m parser.QueryResult, filePath, fileID string, src []byte, result *parser.ExtractionResult, seen, annotationSeen map[string]bool) {
	name := m.Captures["proto.name"].Text
	def := m.Captures["proto.def"]
	id := filePath + "::" + name
	if seen[id] {
		return
	}
	seen[id] = true
	meta := map[string]any{"visibility": swiftVisibility(def.Node, src)}
	if doc := ExtractDocAbove(src, def.StartLine, DocLangSlashSlash); doc != "" {
		meta["doc"] = doc
	}
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindInterface, Name: name,
		FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
		Language: "swift",
		Meta:     meta,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
	})
	emitSwiftAnnotationEdges(def.Node, id, filePath, src, result, annotationSeen)
}

func (e *SwiftExtractor) emitFunction(m parser.QueryResult, filePath, fileID string, src []byte, result *parser.ExtractionResult, seen, annotationSeen map[string]bool, typeRanges []swiftTypeRange) {
	name := m.Captures["func.name"].Text
	def := m.Captures["func.def"]
	startLine := def.StartLine

	doc := ExtractDocAbove(src, def.StartLine, DocLangSlashSlash)
	visibility := swiftVisibility(def.Node, src)
	sig, returnType, isAsync, isStatic := swiftFunctionDetails(def.Node, src)
	if sig == "" {
		sig = "func " + name + "(...)"
	}

	if typeName, ok := findEnclosingSwiftType(typeRanges, startLine); ok {
		id := filePath + "::" + typeName + "." + name
		if seen[id] {
			return
		}
		seen[id] = true
		meta := map[string]any{
			"receiver":   typeName,
			"signature":  sig,
			"visibility": visibility,
		}
		swiftStampFuncMeta(meta, returnType, isAsync, isStatic)
		if doc != "" {
			meta["doc"] = doc
		}
		if sel := swiftObjCSelector(def.Node, name, src); sel != "" {
			meta["objc_selector"] = sel
		}
		result.Nodes = append(result.Nodes, &graph.Node{
			ID: id, Kind: graph.KindMethod, Name: name,
			FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
			Language: "swift", Meta: meta,
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
		})
		typeID := filePath + "::" + typeName
		result.Edges = append(result.Edges, &graph.Edge{
			From: id, To: typeID, Kind: graph.EdgeMemberOf, FilePath: filePath, Line: def.StartLine + 1,
		})
		emitSwiftAnnotationEdges(def.Node, id, filePath, src, result, annotationSeen)
		return
	}

	id := filePath + "::" + name
	if seen[id] {
		return
	}
	seen[id] = true
	meta := map[string]any{
		"signature":  sig,
		"visibility": visibility,
	}
	swiftStampFuncMeta(meta, returnType, isAsync, isStatic)
	if doc != "" {
		meta["doc"] = doc
	}
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindFunction, Name: name,
		FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
		Language: "swift", Meta: meta,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
	})
	emitSwiftAnnotationEdges(def.Node, id, filePath, src, result, annotationSeen)
}

// swiftFunctionDetails parses a function declaration's header for its real
// signature, return type, and async/static modifier flags. The body is dropped
// at the first brace; modifiers precede `func` and the return type follows `->`.
func swiftFunctionDetails(decl *sitter.Node, src []byte) (signature, returnType string, isAsync, isStatic bool) {
	if decl == nil {
		return "", "", false, false
	}
	header := decl.Content(src)
	if i := strings.IndexByte(header, '{'); i >= 0 {
		header = header[:i]
	}
	header = strings.Join(strings.Fields(header), " ")
	isAsync = swiftHasWord(header, "async")
	fi := strings.Index(header, "func ")
	if fi < 0 {
		return strings.TrimSpace(header), "", isAsync, false
	}
	isStatic = swiftHasWord(header[:fi], "static") || swiftHasWord(header[:fi], "class")
	signature = strings.TrimSpace(header[fi:])
	if ri := strings.Index(signature, "->"); ri >= 0 {
		rt := strings.TrimSpace(signature[ri+2:])
		if wi := strings.Index(rt, " where "); wi >= 0 {
			rt = strings.TrimSpace(rt[:wi])
		}
		rt = strings.TrimPrefix(rt, "some ")
		rt = strings.TrimPrefix(rt, "any ")
		returnType = strings.TrimSpace(rt)
	}
	return signature, returnType, isAsync, isStatic
}

// swiftHasWord reports whether word appears as a standalone space-delimited
// token in s.
func swiftHasWord(s, word string) bool {
	for _, f := range strings.Fields(s) {
		if f == word {
			return true
		}
	}
	return false
}

// swiftStampFuncMeta records the return type and async/static flags on a
// function or method node's Meta when present.
func swiftStampFuncMeta(meta map[string]any, returnType string, isAsync, isStatic bool) {
	if returnType != "" {
		meta["return_type"] = returnType
	}
	if isAsync {
		meta["is_async"] = true
	}
	if isStatic {
		meta["is_static"] = true
	}
}

var swiftExtensionRe = regexp.MustCompile(`(?m)^[ \t]*(?:(?:public|private|internal|fileprivate|open|final)[ \t]+)*extension[ \t]+([A-Za-z_][\w.]*)`)

// swiftExtensionRanges returns a type-range per `extension Foo { ... }` block in
// src (Foo collapsed to its last dotted segment), found by a brace-matched text
// scan since the tree-sitter query does not capture extension declarations.
func swiftExtensionRanges(src []byte) []swiftTypeRange {
	s := string(src)
	var ranges []swiftTypeRange
	for _, loc := range swiftExtensionRe.FindAllStringSubmatchIndex(s, -1) {
		typeName := s[loc[2]:loc[3]]
		if i := strings.LastIndexByte(typeName, '.'); i >= 0 {
			typeName = typeName[i+1:]
		}
		rel := strings.IndexByte(s[loc[1]:], '{')
		if rel < 0 {
			continue
		}
		open := loc[1] + rel
		end := swiftMatchBrace(s, open)
		if end < 0 {
			continue
		}
		ranges = append(ranges, swiftTypeRange{
			name:      typeName,
			startLine: strings.Count(s[:open], "\n"),
			endLine:   strings.Count(s[:end], "\n"),
		})
	}
	return ranges
}

// swiftMatchBrace returns the index of the '}' that closes the '{' at open, or
// -1 when unbalanced.
func swiftMatchBrace(s string, open int) int {
	depth := 0
	for i := open; i < len(s); i++ {
		switch s[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// swiftVisibility scans a declaration's leading modifier children for
// an access-level keyword. Swift's default is "internal" when no
// modifier is present. The grammar emits modifiers as plain keyword
// children of the declaration node (visibility_modifier etc.).
func swiftVisibility(decl *sitter.Node, src []byte) string {
	if decl == nil {
		return VisibilityInternal
	}
	for i := 0; i < int(decl.ChildCount()); i++ {
		c := decl.Child(i)
		if c == nil {
			continue
		}
		// Stop scanning once we pass the leading modifier band — once
		// we hit `func` / `class` / `struct` / `protocol` etc. there
		// are no more access modifiers ahead.
		t := c.Type()
		if t == "modifiers" {
			// Some grammar versions wrap modifiers; recurse.
			if v := swiftVisibility(c, src); v != VisibilityInternal {
				return v
			}
			continue
		}
		switch strings.TrimSpace(c.Content(src)) {
		case "public":
			return VisibilityPublic
		case "open":
			return VisibilityPublic
		case "private":
			return VisibilityPrivate
		case "fileprivate":
			return VisibilityPrivate
		case "internal":
			return VisibilityInternal
		}
	}
	return VisibilityInternal
}

func (e *SwiftExtractor) emitImport(m parser.QueryResult, filePath, fileID string, result *parser.ExtractionResult) {
	def := m.Captures["import.def"]
	importText := strings.TrimSpace(def.Text)
	importText = strings.TrimPrefix(importText, "import ")
	importText = strings.TrimSpace(importText)
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: "unresolved::import::" + importText,
		Kind: graph.EdgeImports, FilePath: filePath, Line: def.StartLine + 1,
	})
}

// --- Helpers --------------------------------------------------------

// findEnclosingSwiftType returns the innermost type whose line range
// contains the 0-based line. Mirrors the legacy findEnclosingType
// logic — picks the smallest enclosing range so nested types attribute
// correctly.
func findEnclosingSwiftType(ranges []swiftTypeRange, line int) (string, bool) {
	best := ""
	bestSize := int(^uint(0) >> 1)
	for _, r := range ranges {
		if line >= r.startLine && line <= r.endLine {
			size := r.endLine - r.startLine
			if size < bestSize {
				bestSize = size
				best = r.name
			}
		}
	}
	if best == "" {
		return "", false
	}
	return best, true
}

// findEnclosingSwiftContainer walks the parent chain of n looking for
// the nearest ancestor whose Type() matches t. Returns nil if none.
func findEnclosingSwiftContainer(n *sitter.Node, t string) *sitter.Node {
	if n == nil {
		return nil
	}
	for p := n.Parent(); p != nil; p = p.Parent() {
		if p.Type() == t {
			return p
		}
	}
	return nil
}
