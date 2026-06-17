package languages

import (
	"regexp"
	"strings"
	"unicode"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

var (
	// templateBlockRe matches <script>/<style> blocks, whose bodies are excluded
	// from the template scan (their content is handled by script delegation).
	templateBlockRe = regexp.MustCompile(`(?is)<(script|style)\b[^>]*>.*?</(?:script|style)>`)
	// templateTagRe captures an opening tag name: <Name, <my-name, <svelte:self.
	templateTagRe = regexp.MustCompile(`<([A-Za-z][\w.-]*(?::[\w.-]+)?)`)
)

// templateBuiltins are framework component names that are not real component
// files and must never become cross-file references.
var templateBuiltins = map[string]bool{
	"Teleport": true, "Suspense": true, "KeepAlive": true, "Transition": true,
	"TransitionGroup": true, "Component": true, "Slot": true, "Template": true,
	"Fragment": true, "Code": true, "Debug": true, "Comment": true,
}

// mineTemplateComponentUsages scans the template region (everything outside
// <script>/<style>) and emits a reference edge from componentID to each distinct
// component tag it uses — the cross-file "this component renders that one" link
// that makes a child component a resolved dependent. Kebab-case custom elements
// are normalized to PascalCase (my-button -> MyButton); plain HTML elements and
// framework special elements (svelte:, astro:) are skipped.
func mineTemplateComponentUsages(src []byte, filePath, componentID string, result *parser.ExtractionResult) {
	tmpl := templateBlockRe.ReplaceAllFunc(src, blankPreservingNewlines)
	seen := map[string]bool{}
	for _, m := range templateTagRe.FindAllSubmatch(tmpl, -1) {
		raw := string(m[1])
		if !isComponentTagName(raw) {
			continue
		}
		name := componentRefName(raw)
		if name == "" || templateBuiltins[name] || seen[name] {
			continue
		}
		seen[name] = true
		result.Edges = append(result.Edges, &graph.Edge{
			From: componentID, To: "unresolved::" + name,
			Kind: graph.EdgeReferences, FilePath: filePath,
		})
	}
}

// isComponentTagName reports whether a tag name denotes a component (PascalCase,
// or a hyphenated custom element) rather than a plain HTML element or a framework
// special element.
func isComponentTagName(raw string) bool {
	if raw == "" || strings.HasPrefix(raw, "svelte:") || strings.HasPrefix(raw, "astro:") {
		return false
	}
	if unicode.IsUpper(rune(raw[0])) {
		return true
	}
	return strings.Contains(raw, "-")
}

// componentRefName normalizes a tag name to a component symbol name (kebab-case
// custom elements are PascalCased; PascalCase tags are used verbatim).
func componentRefName(raw string) string {
	if raw == "" {
		return ""
	}
	if unicode.IsUpper(rune(raw[0])) {
		return raw
	}
	var b strings.Builder
	for _, p := range strings.Split(raw, "-") {
		if p == "" {
			continue
		}
		b.WriteString(strings.ToUpper(p[:1]))
		b.WriteString(p[1:])
	}
	return b.String()
}

// blankPreservingNewlines returns a same-length copy of b with every byte except
// newlines replaced by spaces, so a regex replacement keeps line numbers intact.
func blankPreservingNewlines(b []byte) []byte {
	out := make([]byte, len(b))
	for i, c := range b {
		if c == '\n' {
			out[i] = '\n'
		} else {
			out[i] = ' '
		}
	}
	return out
}
