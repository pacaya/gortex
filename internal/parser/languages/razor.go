package languages

import (
	"regexp"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

var (
	razorCodeBlockRe = regexp.MustCompile(`@(?:code|functions)\s*\{`)
	razorModelRe     = regexp.MustCompile(`(?m)^\s*@(?:model|inherits)\s+([A-Za-z_][\w.]*(?:<[^>]*>)?)`)
	razorInjectRe    = regexp.MustCompile(`(?m)^\s*@inject\s+([A-Za-z_][\w.]*(?:<[^>]*>)?)\s+\w+`)
)

// RazorExtractor extracts Razor / Blazor files (.razor, .cshtml). It carves
// every @code{...} / @functions{...} block and delegates it to the C# extractor
// (rebased into host-file coordinates), and emits type references for the
// @model / @inherits / @inject directives.
type RazorExtractor struct {
	cs *CSharpExtractor
}

// NewRazorExtractor constructs a Razor extractor.
func NewRazorExtractor() *RazorExtractor {
	return &RazorExtractor{cs: NewCSharpExtractor()}
}

func (e *RazorExtractor) Language() string     { return "razor" }
func (e *RazorExtractor) Extensions() []string { return []string{".razor", ".cshtml"} }

func (e *RazorExtractor) Extract(filePath string, src []byte) (*parser.ExtractionResult, error) {
	result := &parser.ExtractionResult{}
	lineCount := 1 + strings.Count(string(src), "\n")
	fileNode := &graph.Node{
		ID: filePath, Kind: graph.KindFile, Name: filePath,
		FilePath: filePath, StartLine: 1, EndLine: lineCount, Language: "razor",
	}
	result.Nodes = append(result.Nodes, fileNode)

	// @code{...} / @functions{...} blocks hold C# class members. They are wrapped
	// in a synthetic class so tree-sitter parses the members, then delegated;
	// delegateRazorCode strips the wrapper and rebases into host coordinates.
	for _, span := range razorCodeSpans(src) {
		lineOffset := strings.Count(string(src[:span.start]), "\n")
		e.delegateRazorCode(src[span.start:span.end], lineOffset, filePath, fileNode.ID, result)
	}

	// Directive type references: @model / @inherits name the view-model or base
	// type; @inject names the injected service type.
	for _, m := range razorModelRe.FindAllSubmatch(src, -1) {
		emitRazorTypeRef(result, fileNode.ID, filePath, string(m[1]))
	}
	for _, m := range razorInjectRe.FindAllSubmatch(src, -1) {
		emitRazorTypeRef(result, fileNode.ID, filePath, string(m[1]))
	}
	return result, nil
}

// razorCodeWrapPrefix is a single line so the wrap shifts content by exactly one
// line (compensated when rebasing).
const razorCodeWrapPrefix = "class __RazorCode {"

// delegateRazorCode wraps a @code block body in a synthetic class, runs the C#
// extractor over it, and merges the result rebased into host coordinates,
// dropping the synthetic file and wrapper-class nodes.
func (e *RazorExtractor) delegateRazorCode(content []byte, lineOffset int, filePath, fileID string, result *parser.ExtractionResult) {
	if strings.TrimSpace(string(content)) == "" {
		return
	}
	virtual := filePath + "#code"
	wrapped := []byte(razorCodeWrapPrefix + "\n" + string(content) + "\n}")
	sub, err := e.cs.Extract(virtual, wrapped)
	if err != nil || sub == nil {
		return
	}
	// The wrapper prefix occupies one line, so a wrapped line W is content line
	// W-1; combined with the block's host offset that is lineOffset-1.
	shift := lineOffset - 1
	wrapperID := ""
	for _, n := range sub.Nodes {
		if n == nil || n.ID == virtual {
			continue
		}
		if n.Kind == graph.KindType && n.Name == "__RazorCode" {
			wrapperID = n.ID
			continue
		}
		n.FilePath = filePath
		n.Language = "razor"
		if n.StartLine > 0 {
			n.StartLine += shift
		}
		if n.EndLine > 0 {
			n.EndLine += shift
		}
		if n.Meta == nil {
			n.Meta = map[string]any{}
		}
		n.Meta["inline_script"] = true
		result.Nodes = append(result.Nodes, n)
	}
	for _, ed := range sub.Edges {
		if ed == nil || ed.From == wrapperID || ed.To == wrapperID {
			continue // drop edges to/from the synthetic wrapper class
		}
		if ed.From == virtual {
			ed.From = fileID
		}
		ed.FilePath = filePath
		if ed.Line > 0 {
			ed.Line += shift
		}
		result.Edges = append(result.Edges, ed)
	}
}

type razorSpan struct{ start, end int }

// razorCodeSpans returns the inner content spans of every @code{...} /
// @functions{...} block, matching braces so nested C# braces do not end it early.
func razorCodeSpans(src []byte) []razorSpan {
	var spans []razorSpan
	for _, loc := range razorCodeBlockRe.FindAllIndex(src, -1) {
		open := loc[1] - 1 // position of the opening '{'
		depth := 0
		for i := open; i < len(src); i++ {
			switch src[i] {
			case '{':
				depth++
			case '}':
				depth--
				if depth == 0 {
					spans = append(spans, razorSpan{open + 1, i})
					i = len(src) // stop scanning this block
				}
			}
		}
	}
	return spans
}

func emitRazorTypeRef(result *parser.ExtractionResult, fromID, filePath, typeName string) {
	typeName = strings.TrimSpace(typeName)
	if i := strings.IndexByte(typeName, '<'); i >= 0 {
		typeName = typeName[:i]
	}
	if i := strings.LastIndexByte(typeName, '.'); i >= 0 {
		typeName = typeName[i+1:]
	}
	if typeName == "" {
		return
	}
	result.Edges = append(result.Edges, &graph.Edge{
		From: fromID, To: "unresolved::" + typeName, Kind: graph.EdgeReferences, FilePath: filePath,
	})
}
