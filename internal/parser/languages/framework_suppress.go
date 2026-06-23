package languages

import (
	"regexp"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// Framework-provided-identifier suppression and template-handler mining for the
// SFC frameworks (Vue/Nuxt, Svelte/SvelteKit, Astro). The script blocks are
// delegated to the TS/JS extractor, which emits a bare-call edge for every
// compiler macro / auto-import the framework provides (defineProps, useFetch,
// $state, …). Those have no user definition, so they would linger as unresolved
// edges (noise) or, worse, mis-bind to an unrelated user symbol of the same
// name. Dropping them keeps the component's call graph precise — and a real,
// user-defined same-named symbol still resolves and is left intact.

func frameworkIdentSet(names ...string) map[string]bool {
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}
	return m
}

var frameworkSuppressionSets = map[string]map[string]bool{
	"vue": frameworkIdentSet(
		// Vue compiler macros.
		"defineProps", "defineEmits", "defineExpose", "defineOptions",
		"defineModel", "defineSlots", "withDefaults", "useAttrs", "useSlots",
		// Nuxt auto-imports.
		"useFetch", "useAsyncData", "useLazyFetch", "useState", "useRoute",
		"useRouter", "useRuntimeConfig", "useHead", "useSeoMeta", "useNuxtApp",
		"useCookie", "useRequestHeaders", "navigateTo", "definePageMeta",
		"defineNuxtConfig", "defineEventHandler",
	),
	"svelte": frameworkIdentSet(
		// Svelte 5 runes.
		"$state", "$derived", "$effect", "$props", "$bindable", "$inspect", "$host",
		// Svelte 3/4 lifecycle / store helpers.
		"onMount", "onDestroy", "beforeUpdate", "afterUpdate", "tick",
	),
	"astro": frameworkIdentSet("Astro", "Fragment"),
}

// frameworkImportPrefixes are the virtual module roots a framework injects;
// imports from them are framework-provided, not user files.
var frameworkImportPrefixes = map[string][]string{
	"svelte": {"$app/", "$env/", "$lib/", "svelte", "@sveltejs/"},
	"vue":    {"#imports", "#app", "nuxt/"},
	"astro":  {"astro:", "#imports"},
}

// suppressFrameworkIdents drops the unresolved macro/auto-import edges a
// delegated script produced for lang. Only unresolved targets are touched, so a
// user symbol that genuinely matches the name keeps its resolved edge.
func suppressFrameworkIdents(result *parser.ExtractionResult, lang string) {
	set := frameworkSuppressionSets[lang]
	prefixes := frameworkImportPrefixes[lang]
	if len(set) == 0 && len(prefixes) == 0 {
		return
	}
	kept := result.Edges[:0]
	for _, e := range result.Edges {
		if e == nil {
			continue
		}
		switch e.Kind {
		case graph.EdgeCalls, graph.EdgeReferences, graph.EdgeInstantiates, graph.EdgeReads:
			if graph.IsUnresolvedTarget(e.To) && set[graph.UnresolvedName(e.To)] {
				continue // framework macro — suppress
			}
		case graph.EdgeImports:
			if graph.IsUnresolvedTarget(e.To) {
				name := graph.UnresolvedName(e.To)
				if hasAnyPrefix(name, prefixes) {
					continue // framework virtual module — suppress
				}
			}
		}
		kept = append(kept, e)
	}
	result.Edges = kept
}

func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

var (
	// vueHandlerRe matches a Vue template event binding `@click="onClick"` or
	// `v-on:click="onClick"`. Group 2 is the handler identifier.
	vueHandlerRe = regexp.MustCompile(`(?:@|v-on:)[\w-]+\s*=\s*"(\w+)`)
	// svelteHandlerRe matches a Svelte template binding `on:click={onClick}` or
	// the Svelte 5 `onclick={onClick}` attribute form. Group 1 is the handler.
	svelteHandlerRe = regexp.MustCompile(`on:?[\w-]+\s*=\s*\{\s*(\w+)`)
	// composableDestructureRe matches a script-setup destructure of a Vue/Nuxt
	// composable's return — `const { close, open: o } = useModal()`. Group 1 is
	// the destructure body, group 2 the `use*` composable. Only `use`-prefixed
	// callees qualify (the composable convention), keeping the binding precise.
	composableDestructureRe = regexp.MustCompile(`(?:const|let|var)\s*\{([^}]*)\}\s*=\s*(use[A-Z]\w*)\s*\(`)
)

// composableBinding records a template handler that is a composable-destructured
// local: the original member it aliases and the composable it came from.
type composableBinding struct {
	original   string
	composable string
}

// composableHandlerBindings maps each local name destructured from a `use*`
// composable to its original member + composable, so a template handler that
// references the (possibly renamed) local resolves to the composable member
// instead of an unresolved bare name. `const { close: c } = useModal()` maps
// c -> {original: "close", composable: "useModal"}.
func composableHandlerBindings(src []byte) map[string]composableBinding {
	out := map[string]composableBinding{}
	for _, m := range composableDestructureRe.FindAllSubmatch(src, -1) {
		composable := string(m[2])
		for _, part := range strings.Split(string(m[1]), ",") {
			part = strings.TrimSpace(part)
			if part == "" || strings.HasPrefix(part, "...") {
				continue
			}
			// Drop a default value (`open = false`).
			if i := strings.IndexByte(part, '='); i >= 0 {
				part = strings.TrimSpace(part[:i])
			}
			orig, local := part, part
			if i := strings.IndexByte(part, ':'); i >= 0 { // rename `orig: local`
				orig = strings.TrimSpace(part[:i])
				local = strings.TrimSpace(part[i+1:])
			}
			if !isSimpleJSIdent(local) || !isSimpleJSIdent(orig) {
				continue
			}
			if _, ok := out[local]; !ok {
				out[local] = composableBinding{original: orig, composable: composable}
			}
		}
	}
	return out
}

// isSimpleJSIdent reports whether s is a bare JS identifier (letters, digits,
// `_`, `$`; not starting with a digit).
func isSimpleJSIdent(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '_' || c == '$':
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// mineTemplateHandlers emits a callback reference edge from the component to
// each event handler bound in its markup (@click / v-on / on:click), resolved
// to the in-script function so "what handles this event" and find_usages reach
// the handler — the template→code link a script-only scanner misses.
func mineTemplateHandlers(src []byte, filePath, componentID, lang string, result *parser.ExtractionResult) {
	var re *regexp.Regexp
	switch lang {
	case "svelte":
		re = svelteHandlerRe
	case "astro":
		return // Astro markup uses framework client directives, not @click.
	default:
		re = vueHandlerRe
	}

	funcByName := map[string]string{}
	for _, n := range result.Nodes {
		if n.Kind == graph.KindFunction || n.Kind == graph.KindMethod {
			if _, ok := funcByName[n.Name]; !ok {
				funcByName[n.Name] = n.ID
			}
		}
	}

	// Composable-destructured handlers (`const { close: c } = useModal()` +
	// `@click="c"`) bind to the composable member, not a top-level function.
	bindings := composableHandlerBindings(src)

	tmpl := templateBlockRe.ReplaceAllFunc(src, blankPreservingNewlines)
	seen := map[string]bool{}
	for _, m := range re.FindAllSubmatchIndex(tmpl, -1) {
		handler := string(tmpl[m[2]:m[3]])
		if handler == "" || seen[handler] {
			continue
		}
		seen[handler] = true
		to := "unresolved::" + handler
		origin := graph.OriginTextMatched
		meta := map[string]any{"ref_context": graph.RefContextCallback, "via": "template_handler"}
		if id, ok := funcByName[handler]; ok {
			to = id
			origin = graph.OriginASTResolved
		} else if b, ok := bindings[handler]; ok {
			// Reference the composable's original member so the binding is
			// traceable (`@click="c"` where c aliases useModal().close).
			to = "unresolved::" + b.original
			meta["via"] = "composable_handler"
			meta["composable"] = b.composable
		}
		result.Edges = append(result.Edges, &graph.Edge{
			From: componentID, To: to, Kind: graph.EdgeReferences,
			FilePath: filePath, Line: 1 + strings.Count(string(tmpl[:m[0]]), "\n"),
			Origin: origin,
			Meta:   meta,
		})
	}
}

// applyFrameworkTemplatePasses runs the suppression + handler-mining passes
// shared by the Vue/Svelte/Astro extractors after their scripts are delegated.
func applyFrameworkTemplatePasses(src []byte, filePath, componentID, lang string, result *parser.ExtractionResult) {
	suppressFrameworkIdents(result, lang)
	mineTemplateHandlers(src, filePath, componentID, lang, result)
}
