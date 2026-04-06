package languages

import (
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/bash"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

const (
	bashQFunction = `(function_definition
		name: (word) @func.name) @func.def`

	bashQVariable = `(variable_assignment
		name: (variable_name) @var.name) @var.def`

	bashQCommand = `(command
		name: (command_name) @cmd.name) @cmd.expr`
)

// BashExtractor extracts Bash/Shell source files.
type BashExtractor struct {
	lang *sitter.Language
}

func NewBashExtractor() *BashExtractor {
	return &BashExtractor{lang: bash.GetLanguage()}
}

func (e *BashExtractor) Language() string     { return "bash" }
func (e *BashExtractor) Extensions() []string { return []string{".sh", ".bash"} }

func (e *BashExtractor) Extract(filePath string, src []byte) (*parser.ExtractionResult, error) {
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
		Language: "bash",
	}
	result.Nodes = append(result.Nodes, fileNode)

	seen := make(map[string]bool)

	// Functions.
	matches, _ := parser.RunQuery(bashQFunction, e.lang, root, src)
	for _, m := range matches {
		name := m.Captures["func.name"].Text
		def := m.Captures["func.def"]
		id := filePath + "::" + name
		if seen[id] {
			continue
		}
		seen[id] = true

		result.Nodes = append(result.Nodes, &graph.Node{
			ID: id, Kind: graph.KindFunction, Name: name,
			FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
			Language: "bash", Meta: map[string]any{"signature": name + "()"},
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileNode.ID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
		})
	}

	// Top-level variable assignments.
	matches, _ = parser.RunQuery(bashQVariable, e.lang, root, src)
	for _, m := range matches {
		name := m.Captures["var.name"].Text
		def := m.Captures["var.def"]
		// Only top-level: parent is program.
		if def.Node != nil && def.Node.Parent() != nil && def.Node.Parent().Type() == "program" {
			id := filePath + "::" + name
			if seen[id] {
				continue
			}
			seen[id] = true
			result.Nodes = append(result.Nodes, &graph.Node{
				ID: id, Kind: graph.KindVariable, Name: name,
				FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
				Language: "bash",
			})
			result.Edges = append(result.Edges, &graph.Edge{
				From: fileNode.ID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
			})
		}
	}

	// Command calls — extract source/dot imports and general call sites.
	funcRanges := buildFuncRanges(result)

	matches, _ = parser.RunQuery(bashQCommand, e.lang, root, src)
	for _, m := range matches {
		cmdName := m.Captures["cmd.name"].Text
		expr := m.Captures["cmd.expr"]

		// Source/dot imports: `source foo.sh` or `. foo.sh`
		if cmdName == "source" || cmdName == "." {
			// The argument is the second child of the command node.
			cmdNode := expr.Node
			if cmdNode != nil && cmdNode.NamedChildCount() >= 2 {
				arg := cmdNode.NamedChild(1)
				if arg != nil {
					importPath := arg.Content(src)
					// Strip quotes if present.
					importPath = strings.Trim(importPath, "\"'")
					result.Edges = append(result.Edges, &graph.Edge{
						From: fileNode.ID, To: "unresolved::import::" + importPath,
						Kind: graph.EdgeImports, FilePath: filePath, Line: expr.StartLine + 1,
					})
				}
			}
			continue
		}

		// Regular command call.
		callerID := findEnclosingFunc(funcRanges, expr.StartLine+1)
		if callerID == "" {
			continue
		}
		result.Edges = append(result.Edges, &graph.Edge{
			From: callerID, To: "unresolved::" + cmdName,
			Kind: graph.EdgeCalls, FilePath: filePath, Line: expr.StartLine + 1,
		})
	}

	return result, nil
}
