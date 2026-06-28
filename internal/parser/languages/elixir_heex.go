package languages

import (
	"regexp"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// emitElixirHEExEdges walks a defmodule body for `~H"""..."""` sigil
// templates (the Phoenix HEEx markup that LiveView and the function-
// component layer compile down to render trees) and emits one
// EdgeRendersChild per unique component reference.
//
// HEEx component syntax:
//
//   - `<.local_name attr="...">…</.local_name>`     → local component
//     in the same module
//   - `<MyApp.Components.Card />`                   → cross-module
//     reference
//
// Lowercase HTML / SVG primitives (`<div>`, `<span>`) are skipped — the
// rendering edge graph would be pure noise otherwise. Capital-letter
// component names and dot-leading local names both qualify; we match
// the React-side convention so the same `analyze kind=components`
// rollup works for HEEx and JSX side-by-side.
//
// We can't rely on the tree-sitter-elixir grammar to parse the HEEx
// markup — the sigil contents are exposed as a raw string. A scan of
// the sigil body with a tight regex is the pragmatic alternative.
// Bracketed conditionals, comprehensions, and `<%= … %>` interpolation
// blocks aren't false positives because the regex requires a leading
// `<.` or `<UpperCase` token immediately followed by an attribute /
// space / `>` boundary.
func emitElixirHEExEdges(modID string, body *sitter.Node, src []byte, filePath string, result *parser.ExtractionResult) {
	if body == nil || modID == "" {
		return
	}
	seen := map[string]int{}
	walkAST(body, func(n *sitter.Node) bool {
		if n == nil {
			return true
		}
		if !isHEExSigil(n, src) {
			return true
		}
		// Pull the sigil body text — we only care about positions
		// relative to the file, so use the node's overall start
		// row as the line for every ref it contributes (good
		// enough for the line filter the analyzer uses).
		text := n.Content(src)
		startLine := int(n.StartPoint().Row) + 1
		for _, name := range parseHEExComponentRefs(text) {
			if _, dup := seen[name]; !dup {
				seen[name] = startLine
			}
		}
		return false // don't recurse into the sigil body — it's text
	})
	for name, line := range seen {
		result.Edges = append(result.Edges, &graph.Edge{
			From:     modID,
			To:       "unresolved::" + name,
			Kind:     graph.EdgeRendersChild,
			FilePath: filePath,
			Line:     line,
			Origin:   graph.OriginASTInferred,
			Meta: map[string]any{
				"child_name": name,
				"flavor":     "heex",
			},
		})
	}
}

// isHEExSigil reports whether n is a sigil node carrying HEEx markup.
// Phoenix maps `~H"..."` to the HEEx engine; older codebases used
// `~L"..."` for the LEEx engine which we treat the same. Both surface
// as a tree-sitter-elixir `sigil` node whose `name` field is `H` or
// `L`.
func isHEExSigil(n *sitter.Node, src []byte) bool {
	if n == nil || n.Type() != "sigil" {
		return false
	}
	nameNode := n.ChildByFieldName("name")
	if nameNode == nil {
		// Some grammar variants don't expose the name as a field.
		// Fall back to scanning the first few children for a
		// sigil_name node carrying the letter.
		for i, _nc := 0, int(n.NamedChildCount()); i < _nc; i++ {
			c := n.NamedChild(i)
			if c != nil && (c.Type() == "sigil_name" || c.Type() == "sigil_modifiers") {
				nameNode = c
				break
			}
		}
	}
	if nameNode == nil {
		return false
	}
	name := nameNode.Content(src)
	return name == "H" || name == "L"
}

// heexLocalComponent matches `<.local_name` followed by a non-name
// boundary (space / tab / newline / `/` / `>`). Captures the local
// name without the leading dot.
var heexLocalComponent = regexp.MustCompile(`<\.([a-z][a-zA-Z0-9_!?]*)\b`)

// heexRemoteComponent matches `<Module.Path.Name` or `<Component`
// followed by a boundary. Captures the qualified component name.
// The leading character must be uppercase to skip HTML primitives;
// dot-segments after the first uppercase character carry the full
// module path so `<MyApp.UI.Card />` lands as `MyApp.UI.Card`.
var heexRemoteComponent = regexp.MustCompile(`<([A-Z][A-Za-z0-9_]*(?:\.[A-Za-z][A-Za-z0-9_]*)*)\b`)

// parseHEExComponentRefs returns the unique component names (local
// and remote) referenced inside a HEEx sigil body. Local names are
// returned with their leading dot preserved (`.button`) so the
// resolver can distinguish `local_name` from `LocalName` if both
// happen to exist.
func parseHEExComponentRefs(text string) []string {
	out := []string{}
	seen := map[string]struct{}{}
	for _, m := range heexLocalComponent.FindAllStringSubmatch(text, -1) {
		if len(m) < 2 {
			continue
		}
		name := "." + m[1]
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	for _, m := range heexRemoteComponent.FindAllStringSubmatch(text, -1) {
		if len(m) < 2 {
			continue
		}
		name := m[1]
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	return out
}
