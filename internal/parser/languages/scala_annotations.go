package languages

import (
	"strings"

	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// emitScalaAnnotationEdges scans the direct children of a class /
// object / trait / function / var-definition for `annotation` nodes
// and emits one EdgeAnnotated per annotation onto the synthetic
// `annotation::scala::<name>` node. Examples:
//
//	@deprecated("use newF", "1.5")  → annotation::scala::deprecated
//	@inline                          → annotation::scala::inline
//	@volatile                        → annotation::scala::volatile
//	@tailrec                         → annotation::scala::tailrec
//	@main                            → annotation::scala::main
//
// The Scala tree-sitter grammar places annotations as direct children
// of the annotated definition node, so we only scan the immediate
// child set — recursive walks would attribute method annotations to
// the enclosing class, which is wrong.
//
// `seen` deduplicates synthetic annotation nodes within the extraction
// pass; the graph layer also dedupes by ID so a miss here is wasted
// work, not a correctness bug.
func emitScalaAnnotationEdges(
	defNode *sitter.Node, fromID, filePath string, src []byte,
	result *parser.ExtractionResult, seen map[string]bool,
) {
	if defNode == nil || fromID == "" {
		return
	}
	for i, _nc := 0, int(defNode.NamedChildCount()); i < _nc; i++ {
		c := defNode.NamedChild(i)
		if c == nil || c.Type() != "annotation" {
			continue
		}
		name, args := scalaAnnotationNameAndArgs(c, src)
		if name == "" {
			continue
		}
		line := int(c.StartPoint().Row) + 1
		EmitAnnotationEdge(fromID, "scala", name, args, filePath, line, result, seen)
	}
}

// scalaAnnotationNameAndArgs reads an `annotation` node and returns
// (name, args). Name comes from the `type_identifier` child; args is
// the raw inner-parentheses text of an `arguments` child (or "" when
// the annotation has no argument list). Both return values are
// trimmed of surrounding whitespace.
//
// Multi-segment names like `@a.b.C` (qualified annotation references)
// are returned as the trailing segment ("C") so `find_usages` on the
// synthetic annotation node still groups every site that uses the
// same logical annotation regardless of import alias.
func scalaAnnotationNameAndArgs(annot *sitter.Node, src []byte) (string, string) {
	if annot == nil {
		return "", ""
	}
	var name, args string
	for i, _nc := 0, int(annot.NamedChildCount()); i < _nc; i++ {
		c := annot.NamedChild(i)
		if c == nil {
			continue
		}
		switch c.Type() {
		case "type_identifier", "identifier":
			if name == "" {
				name = strings.TrimSpace(c.Content(src))
			}
		case "stable_type_identifier", "projected_type":
			// Qualified annotation reference like `@scala.deprecated` —
			// keep only the trailing leaf so the synthetic node
			// matches every alias.
			text := strings.TrimSpace(c.Content(src))
			if idx := strings.LastIndex(text, "."); idx >= 0 {
				text = text[idx+1:]
			}
			if name == "" {
				name = text
			}
		case "arguments":
			text := strings.TrimSpace(c.Content(src))
			if len(text) >= 2 && text[0] == '(' && text[len(text)-1] == ')' {
				text = text[1 : len(text)-1]
			}
			args = strings.TrimSpace(text)
		}
	}
	return name, args
}
