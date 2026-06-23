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
	// Nuxt framework components — auto-registered, no component file.
	"NuxtPage": true, "NuxtLayout": true, "NuxtLink": true, "NuxtLoadingIndicator": true,
	"NuxtErrorBoundary": true, "NuxtWelcome": true, "NuxtClientFallback": true,
	"NuxtRouteAnnouncer": true, "NuxtImg": true, "NuxtPicture": true, "NuxtIsland": true,
	"ClientOnly": true, "DevOnly": true,
}

// stripNuxtLazyPrefix removes the Nuxt `Lazy` auto-import prefix so a
// `<LazyBaseButton>` lazy-hydrated usage references the same `BaseButton`
// component as `<BaseButton>`. Only strips when `Lazy` is followed by an
// uppercase letter (a PascalCase component head), so a component genuinely
// named `Lazy` or `Lazyload` is left intact.
func stripNuxtLazyPrefix(name string) string {
	const pfx = "Lazy"
	if len(name) > len(pfx) && strings.HasPrefix(name, pfx) && unicode.IsUpper(rune(name[len(pfx)])) {
		return name[len(pfx):]
	}
	return name
}

// mineTemplateComponentUsages scans the template region (everything outside
// <script>/<style>) and emits a reference edge from componentID to each distinct
// component tag it uses — the cross-file "this component renders that one" link
// that makes a child component a resolved dependent. Kebab-case custom elements
// are normalized to PascalCase (my-button -> MyButton); plain HTML elements and
// framework special elements (svelte:, astro:) are skipped.
func mineTemplateComponentUsages(src []byte, filePath, componentID string, result *parser.ExtractionResult) {
	tmpl := templateBlockRe.ReplaceAllFunc(src, blankPreservingNewlines)
	for _, idx := range templateTagRe.FindAllSubmatchIndex(tmpl, -1) {
		// idx[0:2] spans the whole `<Tag` match; idx[2:4] is the captured name.
		raw := string(tmpl[idx[2]:idx[3]])
		if !isComponentTagName(raw) {
			continue
		}
		name := stripNuxtLazyPrefix(componentRefName(raw))
		if name == "" || templateBuiltins[name] {
			continue
		}
		// One positioned edge per render site — NOT deduplicated by name. The
		// tag's line comes from its byte offset (blanked <script>/<style>
		// blocks preserve newlines, so offsets still map to source lines), and
		// each edge carries Origin=OriginASTResolved plus Meta[template]=true so
		// find_usages reports every render location with a line number, an AST
		// provenance tier, and a template-vs-code role — where a name-deduped
		// single reference would collapse repeated renders into one position.
		line := 1 + strings.Count(string(tmpl[:idx[0]]), "\n")
		result.Edges = append(result.Edges, &graph.Edge{
			From: componentID, To: "unresolved::" + name,
			Kind: graph.EdgeReferences, FilePath: filePath,
			Line: line, Origin: graph.OriginASTResolved,
			Meta: map[string]any{"template": true},
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
