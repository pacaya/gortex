package languages

import (
	"regexp"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

var (
	dfmObjectRe = regexp.MustCompile(`(?i)^\s*(?:object|inherited|inline)\s+(\w+)\s*:\s*(\w+)`)
	dfmEventRe  = regexp.MustCompile(`(?i)^\s*(On\w+)\s*=\s*(\w+)\s*$`)
	dfmEndRe    = regexp.MustCompile(`(?i)^\s*end\b`)
)

// DFMExtractor extracts Delphi / FireMonkey form-definition files (.dfm, .fmx).
// Each `object Name: TType` declaration becomes a node — the top-level form is a
// type, nested controls are fields of it — with a reference to the component
// class TType, and each `OnEvent = Handler` assignment references the Pascal
// event-handler method, so a form is linked to the component types and unit
// methods it uses. The Language() key is distinct so it cannot clobber the
// Pascal extractor; the emitted nodes are labelled `pascal` to group with Delphi.
type DFMExtractor struct{}

// NewDFMExtractor constructs a Delphi form-definition extractor.
func NewDFMExtractor() *DFMExtractor { return &DFMExtractor{} }

func (e *DFMExtractor) Language() string     { return "dfm" }
func (e *DFMExtractor) Extensions() []string { return []string{".dfm", ".fmx"} }

func (e *DFMExtractor) Extract(filePath string, src []byte) (*parser.ExtractionResult, error) {
	result := &parser.ExtractionResult{}
	lines := strings.Split(string(src), "\n")
	fileNode := &graph.Node{
		ID: filePath, Kind: graph.KindFile, Name: filePath,
		FilePath: filePath, StartLine: 1, EndLine: len(lines), Language: "pascal",
	}
	result.Nodes = append(result.Nodes, fileNode)

	depth := 0
	formID := ""
	seen := map[string]bool{}
	for i, line := range lines {
		switch {
		case dfmObjectRe.MatchString(line):
			m := dfmObjectRe.FindStringSubmatch(line)
			name, typ := m[1], m[2]
			id := filePath + "::" + name
			if !seen[id] {
				seen[id] = true
				kind := graph.KindField
				if depth == 0 {
					kind = graph.KindType
					formID = id
				}
				result.Nodes = append(result.Nodes, &graph.Node{
					ID: id, Kind: kind, Name: name,
					FilePath: filePath, StartLine: i + 1, EndLine: i + 1, Language: "pascal",
					Meta: map[string]any{"dfm_type": typ},
				})
				result.Edges = append(result.Edges, &graph.Edge{
					From: fileNode.ID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: i + 1,
				})
				if depth > 0 && formID != "" {
					result.Edges = append(result.Edges, &graph.Edge{
						From: id, To: formID, Kind: graph.EdgeMemberOf, FilePath: filePath, Line: i + 1,
					})
				}
				result.Edges = append(result.Edges, &graph.Edge{
					From: id, To: "unresolved::" + typ, Kind: graph.EdgeReferences, FilePath: filePath, Line: i + 1,
				})
			}
			depth++
		case dfmEventRe.MatchString(line):
			m := dfmEventRe.FindStringSubmatch(line)
			result.Edges = append(result.Edges, &graph.Edge{
				From: fileNode.ID, To: "unresolved::" + m[2], Kind: graph.EdgeReferences, FilePath: filePath, Line: i + 1,
			})
		case dfmEndRe.MatchString(line):
			if depth > 0 {
				depth--
			}
		}
	}
	return result, nil
}
