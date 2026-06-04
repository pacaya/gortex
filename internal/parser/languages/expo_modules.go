package languages

import (
	"regexp"
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// Expo Modules cross-language support. An Expo native module is declared
// in Swift or Kotlin with a DSL inside `definition() -> ModuleDefinition`:
// `Name("Foo")` sets the JS module name and `Function("bar") { ... }` /
// `AsyncFunction("baz") { ... }` declare the JS-callable methods. On the
// JS side they are consumed via `requireNativeModule('Foo').bar(...)` —
// which the JS/TS extractor already emits as an rn-native placeholder, so
// the Expo bridge synthesizer can land it on these synthetic nodes.

var (
	expoNameRe     = regexp.MustCompile(`\bName\s*\(\s*"([^"]+)"`)
	expoFunctionRe = regexp.MustCompile(`\b(Async)?Function\s*\(\s*"([^"]+)"`)
)

// expoExport is one Expo DSL method declaration.
type expoExport struct {
	module string
	method string
	async  bool
	off    int
}

// extractExpoModules scans Swift/Kotlin source for the Expo module DSL
// and returns each Function/AsyncFunction declaration attributed to the
// most recent preceding Name("...") (its module). Returns nil when the
// file is not an Expo module (no ModuleDefinition marker).
func extractExpoModules(src []byte) []expoExport {
	s := string(src)
	if !strings.Contains(s, "ModuleDefinition") {
		return nil
	}
	type marker struct {
		off    int
		isName bool
		name   string
		async  bool
	}
	var markers []marker
	for _, m := range expoNameRe.FindAllStringSubmatchIndex(s, -1) {
		markers = append(markers, marker{off: m[0], isName: true, name: s[m[2]:m[3]]})
	}
	for _, m := range expoFunctionRe.FindAllStringSubmatchIndex(s, -1) {
		markers = append(markers, marker{off: m[0], name: s[m[4]:m[5]], async: m[2] >= 0})
	}
	sort.Slice(markers, func(i, j int) bool { return markers[i].off < markers[j].off })

	curModule := ""
	var out []expoExport
	for _, mk := range markers {
		if mk.isName {
			curModule = mk.name
			continue
		}
		if curModule == "" {
			continue
		}
		out = append(out, expoExport{module: curModule, method: mk.name, async: mk.async, off: mk.off})
	}
	return out
}

// emitExpoModuleNodes materialises a synthetic JS-callable method node per
// Expo Function/AsyncFunction declaration, carrying expo_module +
// expo_method so the Expo bridge synthesizer can pair a JS
// requireNativeModule('<module>').<method>() call to it.
func emitExpoModuleNodes(src []byte, filePath, language, fileID string, result *parser.ExtractionResult, seen map[string]bool) {
	for _, ex := range extractExpoModules(src) {
		id := filePath + "::expo:" + ex.module + ":" + ex.method
		if seen[id] {
			continue
		}
		seen[id] = true
		line := lineAt(src, ex.off)
		result.Nodes = append(result.Nodes, &graph.Node{
			ID: id, Kind: graph.KindMethod, Name: ex.method,
			FilePath: filePath, StartLine: line, EndLine: line,
			Language: language,
			Meta:     map[string]any{"expo_module": ex.module, "expo_method": ex.method, "expo_async": ex.async},
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: line,
		})
	}
}
