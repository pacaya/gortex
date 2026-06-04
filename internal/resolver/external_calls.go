package resolver

import (
	"path/filepath"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// External-call placeholder synthesis.
//
// When code calls into an un-indexed third party (an npm / pip / cargo
// package not in the index) or a sibling microservice's client SDK, the
// call target can't be resolved to a real graph node. The main resolver
// lands such a call on a bookkeeping-string terminal — `dep::<path>::<sym>`,
// `stdlib::<path>::<sym>`, or `external::<path>` — that names no node. A
// call-chain walk treats those exactly like an `unresolved::` placeholder:
// graph.Engine.bfs drops any edge whose target node is missing, so
// `get_call_chain` / `get_callers` silently lose the fact that the
// function reaches out to an external system at all.
//
// This pass closes that gap. For each such edge it synthesises a single
// shared graph node — one per (ecosystem, import path) — marked clearly
// as external + synthetic, and retargets the call edge to it. The
// call-chain then terminates on an explicit "external" node instead of
// vanishing.
//
// Gated, and noise-filtered:
//
//   - The whole pass is opt-in (`.gortex.yaml::index::synthesize_external_calls`).
//     Default-off, so behaviour is unchanged unless a project asks for it.
//
//   - Even when on, synthesis is restricted to *genuine* external
//     packages. A call into a language built-in or standard library is
//     noise — every Go file calls `fmt`, every Node file requires `path`
//     — and materialising a node for each would bury the real
//     cross-system edges. isLanguageStdlib drops those. The decision is
//     language-aware: the same un-dotted name (`crypto`) is the Go stdlib
//     but, in a TS file, a real npm package, so the caller's language
//     selects the rule.
//
// The pass is a full recompute and idempotent: a synthetic node has a
// deterministic ID, so re-running rewrites each edge to the same target
// and graph.AddNode dedupes. Runs after the main resolution pass and the
// cross-package guard — by then every edge that was going to land on a
// real node already has, and the cross_pkg_guard has reverted its weak
// name-only guesses back to bare `unresolved::` placeholders. Those bare
// placeholders carry no import path and are deliberately left alone here:
// without import evidence we cannot tell a genuine external from an
// un-indexed in-repo symbol.
//
// Returns the number of call edges retargeted onto a synthetic node.

// externalCallPrefix is the placeholder namespace for a synthesised
// external-call node. Deliberately distinct from the `ext::` namespace
// the goanalysis externals pass uses (those are type-checker-grounded
// symbols with a module attribution) and from `external::` / `dep::` /
// `stdlib::` (bookkeeping strings that name no node) — so the
// `analyze kind=external_calls` surface, which keys on `ext::` + the
// `external` Meta flag, never mistakes a synthetic node for one of its
// own attributed symbols.
const externalCallPrefix = "external-call::"

// SynthesizeExternalCalls materialises a synthetic placeholder node for
// every call edge that lands on an un-indexed external package / sibling
// service and retargets the edge onto it, so call-chain traversals keep
// the external hop visible. Enabled is the opt-in gate
// (`.gortex.yaml::index::synthesize_external_calls`); when false the
// pass is a no-op and the graph is untouched.
func SynthesizeExternalCalls(g graph.Store, enabled bool) int {
	if g == nil || !enabled {
		return 0
	}
	return synthesizeExternalCalls(g, func() []*graph.Edge { return externalCallCandidateEdges(g) })
}

// SynthesizeExternalCallsForFiles is the incremental counterpart of
// SynthesizeExternalCalls: it materialises external-call nodes for only
// the out-edges of the given changed files, so a single-file reindex does
// O(edited-file) work instead of the full-graph recompute. The synthetic
// per-package nodes are shared (deterministic ID), so a file that adds a
// caller for an already-materialised package just dedups onto the existing
// node; graph.EvictFile drops a removed file's synthesised edges before
// reindex, so no orphan-cleanup pass is needed. A no-op when disabled or
// when no files changed.
func SynthesizeExternalCallsForFiles(g graph.Store, enabled bool, files []string) int {
	if g == nil || !enabled || len(files) == 0 {
		return 0
	}
	return synthesizeExternalCalls(g, func() []*graph.Edge { return externalCallCandidateEdgesForFiles(g, files) })
}

// synthesizeExternalCalls is the shared materialisation core. collect runs
// under the resolve lock and returns the candidate call / reference edges
// (external-package terminals plus any already-synthesised external-call::
// edges, so the returned count stays "edges terminating on a synthetic
// node after the pass"). Each genuine external terminal gets a shared
// per-(ecosystem, import path) node and the edge is retargeted onto it.
func synthesizeExternalCalls(g graph.Store, collect func() []*graph.Edge) int {
	// Serialise against the other graph-wide passes that mutate the
	// graph (markTestSymbolsAndEmitEdges, ResolveTemporalCalls,
	// reach.BuildIndex). This pass calls AddNode and ReindexEdge; a
	// concurrent reader walking AllNodes / AllEdges would otherwise
	// trip the runtime's concurrent map access check.
	mu := g.ResolveMutex()
	mu.Lock()
	defer mu.Unlock()

	synthesized := 0
	var reindexBatch []graph.EdgeReindex
	type candidate struct {
		edge                  *graph.Edge
		ecosystem, importPath string
	}
	var candidates []candidate
	fromIDSet := map[string]struct{}{}
	for _, e := range collect() {
		if e == nil {
			continue
		}
		// Already pointing at a synthetic node — a prior run landed it.
		// Count it (the return value is "call edges terminating on a
		// synthetic node after the pass") and leave it untouched, so
		// re-running is a stable no-op.
		if strings.HasPrefix(e.To, externalCallPrefix) {
			synthesized++
			continue
		}
		ecosystem, importPath, ok := parseExternalCallTarget(e.To)
		if !ok {
			continue
		}
		candidates = append(candidates, candidate{edge: e, ecosystem: ecosystem, importPath: importPath})
		if e.From != "" {
			fromIDSet[e.From] = struct{}{}
		}
	}
	fromList := make([]string, 0, len(fromIDSet))
	for id := range fromIDSet {
		fromList = append(fromList, id)
	}
	callerNodes := g.GetNodesByIDs(fromList)

	for _, c := range candidates {
		e := c.edge
		callerLang := ""
		if from := callerNodes[e.From]; from != nil && from.Language != "" {
			callerLang = from.Language
		} else {
			callerLang = langFamilyFromExt(e.FilePath)
		}
		if isLanguageStdlib(callerLang, c.importPath) {
			// Language built-in / standard library — noise. Leave the
			// edge on its bookkeeping-string terminal; a stdlib hop is
			// not a cross-system call worth a call-chain node.
			continue
		}

		nodeID := externalCallNodeID(c.ecosystem, c.importPath)
		if g.GetNode(nodeID) == nil {
			g.AddNode(newExternalCallNode(nodeID, c.ecosystem, c.importPath, callerLang))
		}

		oldTo := e.To
		e.To = nodeID
		// The edge now lands on a real (synthetic) node. It is an
		// inferred, name-only-grade binding — the import path tells us
		// which package, never the specific callee symbol, and the
		// synthetic node is per-package — so it rides at the weakest
		// tier.
		e.Origin = graph.OriginTextMatched
		e.Confidence = 0.5
		e.ConfidenceLabel = graph.ConfidenceLabelFor(e.Kind, e.Confidence)
		if e.Meta == nil {
			e.Meta = map[string]any{}
		}
		e.Meta["external_call"] = true
		reindexBatch = append(reindexBatch, graph.EdgeReindex{Edge: e, OldTo: oldTo})
		synthesized++
	}
	if len(reindexBatch) > 0 {
		g.ReindexEdges(reindexBatch)
	}
	return synthesized
}

// externalCallCandidateEdges returns the call / reference edges whose
// target is an un-indexed external-package terminal (dep:: / stdlib:: /
// external::, including the per-repo-prefixed stdlib form) or an
// already-synthesised external-call:: node. It uses the
// ExternalCallCandidates pushdown capability when the backend implements
// it — the disk backend then selects exactly these rows instead of
// marshaling every call edge in the graph and filtering Go-side — and
// falls back to the EdgesByKinds scan + prefix filter otherwise.
func externalCallCandidateEdges(g graph.Store) []*graph.Edge {
	if cap, ok := g.(graph.ExternalCallCandidates); ok {
		return cap.ExternalCallCandidateEdges()
	}
	var out []*graph.Edge
	for e := range edgesByKinds(g, []graph.EdgeKind{graph.EdgeCalls, graph.EdgeReferences}) {
		if e != nil && isExternalCandidateTarget(e.To) {
			out = append(out, e)
		}
	}
	return out
}

// externalCallCandidateEdgesForFiles returns the external-terminal call /
// reference out-edges originating in the given files only — the O(edited
// files) input for incremental synthesis. Edges are gathered from the
// out-edges of every symbol the files define.
func externalCallCandidateEdgesForFiles(g graph.Store, files []string) []*graph.Edge {
	idSet := make(map[string]struct{})
	var ids []string
	for _, f := range files {
		for _, n := range g.GetFileNodes(f) {
			if n == nil {
				continue
			}
			if _, seen := idSet[n.ID]; seen {
				continue
			}
			idSet[n.ID] = struct{}{}
			ids = append(ids, n.ID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	var out []*graph.Edge
	for _, edges := range g.GetOutEdgesByNodeIDs(ids) {
		for _, e := range edges {
			if e == nil {
				continue
			}
			if e.Kind != graph.EdgeCalls && e.Kind != graph.EdgeReferences {
				continue
			}
			if isExternalCandidateTarget(e.To) {
				out = append(out, e)
			}
		}
	}
	return out
}

// isExternalCandidateTarget reports whether a target string is one that
// synthesizeExternalCalls considers: an external-package terminal or an
// already-materialised external-call:: node (kept so the pass's return
// count stays stable across re-runs).
func isExternalCandidateTarget(to string) bool {
	if strings.HasPrefix(to, externalCallPrefix) {
		return true
	}
	_, _, ok := parseExternalCallTarget(to)
	return ok
}

// parseExternalCallTarget recognises the three bookkeeping-string
// terminals the main resolver lands an un-indexed external call on and
// extracts (ecosystem, importPath) from each. Returns ok=false for
// anything else — a real node ID, a bare `unresolved::` placeholder, a
// `builtin::` terminal, or an already-synthesised `external-call::`
// node.
//
//	dep::<importPath>::<symbol>       — resolveExtern, dotted import path
//	stdlib::<importPath>::<symbol>    — resolveExtern, stdlib-shaped path
//	external::<importPath>            — resolveImport (no symbol component)
//
// The `dep::` / `stdlib::` forms carry a trailing `::<symbol>`; it is
// dropped — the synthetic node is per-package, so the specific callee
// symbol is not retained. `dep` / `stdlib` here are the resolver's
// Go-centric labels; the real stdlib-vs-third-party decision is re-made
// language-aware by the caller via isLanguageStdlib, so both prefixes
// feed the same path.
func parseExternalCallTarget(target string) (ecosystem, importPath string, ok bool) {
	switch {
	case strings.HasPrefix(target, "dep::"):
		path := importPathOfExtern(strings.TrimPrefix(target, "dep::"))
		if path == "" {
			return "", "", false
		}
		return "dep", path, true
	case graph.IsStdlibStub(target):
		// Handles both legacy `stdlib::<path>::<sym>` and the
		// per-repo-prefixed `<repo>::stdlib::<path>::<sym>` shape
		// (see internal/graph/stub.go).
		path := importPathOfExtern(graph.StubRest(target))
		if path == "" {
			return "", "", false
		}
		return "stdlib", path, true
	case strings.HasPrefix(target, "external::"):
		path := strings.TrimPrefix(target, "external::")
		if path == "" {
			return "", "", false
		}
		return "external", path, true
	}
	return "", "", false
}

// importPathOfExtern strips the trailing `::<symbol>` from a
// `<importPath>::<symbol>` resolver terminal, returning just the import
// path. Splitting at the final `::` keeps the path intact even in the
// pathological case of a path that itself contains `::`. Returns "" when
// the string carries no `::` separator at all.
func importPathOfExtern(s string) string {
	i := strings.LastIndex(s, "::")
	if i < 0 {
		return ""
	}
	return s[:i]
}

// externalCallNodeID is the deterministic ID of the synthetic node for
// one (ecosystem, importPath) pair. Deterministic so a re-run of the
// pass retargets onto the same node and graph.AddNode dedupes — the
// node is shared by every call into that package.
func externalCallNodeID(ecosystem, importPath string) string {
	return externalCallPrefix + ecosystem + "::" + importPath
}

// newExternalCallNode builds the synthetic placeholder node for an
// un-indexed external package. It is marked unmistakably as both
// synthetic and external so analyzers can filter it: `synthetic: true`
// keeps it out of dead-code / hotspot / coverage rollups that only mean
// to score real source symbols, and `external_call: true` lets a query
// pick out exactly the cross-system terminals this pass created.
func newExternalCallNode(nodeID, ecosystem, importPath, callerLang string) *graph.Node {
	return &graph.Node{
		ID:       nodeID,
		Kind:     graph.KindModule,
		Name:     importPath,
		QualName: importPath,
		// A synthetic FilePath that can never collide with a real
		// source file, mirroring the goanalysis externals pass's
		// `external::go:<path>` convention. Keeps byFile buckets clean.
		FilePath: externalCallPrefix + ecosystem + ":" + importPath,
		Language: callerLang,
		Meta: map[string]any{
			"synthetic":     true,
			"external_call": true,
			"import_path":   importPath,
			"ecosystem":     ecosystem,
		},
	}
}

// langFamilyFromExt maps a file extension to the coarse language label
// stored on graph nodes. Distinct from builtins.go::langFromFilePath,
// which collapses ts→ts/js→js for the built-in method tables; here we
// want the node-level Language value ("typescript", "go", …) so the
// stdlib rule below can be keyed the same way for caller-node hits and
// file-extension fallbacks.
func langFamilyFromExt(p string) string {
	switch filepath.Ext(p) {
	case ".go":
		return "go"
	case ".js", ".jsx", ".mjs", ".cjs":
		return "javascript"
	case ".ts", ".tsx", ".mts", ".cts":
		return "typescript"
	case ".py":
		return "python"
	case ".rs":
		return "rust"
	}
	return ""
}

// isLanguageStdlib reports whether importPath addresses the language's
// built-in standard library rather than a genuine third-party package.
// This is the noise filter: a stdlib hop (`fmt`, `os`, `node:path`) is
// not a cross-system call and gets no synthetic node.
//
// The decision is language-specific because the same path shape means
// different things per ecosystem — an un-dotted single segment is the
// Go stdlib but, for npm / pip, an ordinary package name. When the
// caller's language is unknown the import path is treated as external
// (return false): a missed-filter false positive is one extra node,
// while a wrong-filter false negative would drop a real external edge.
func isLanguageStdlib(lang, importPath string) bool {
	if importPath == "" {
		return false
	}
	switch lang {
	case "go":
		// Go stdlib import paths have no dot in their first segment
		// (`fmt`, `net/http`, `encoding/json`); third-party modules
		// always lead with a domain (`github.com/...`). Same heuristic
		// the resolver's stdlib/dep split already uses.
		return isStdlibLike(importPath)
	case "python":
		return isPythonStdlib(pyTopLevelModule(importPath))
	case "javascript", "typescript":
		return isNodeCoreModule(importPath)
	case "rust":
		// The Rust standard distribution: std / core / alloc / proc_macro.
		// `test` is also distribution-shipped. Everything else is a crate.
		root := importPath
		if i := strings.IndexAny(root, ":/"); i >= 0 {
			root = root[:i]
		}
		switch root {
		case "std", "core", "alloc", "proc_macro", "test":
			return true
		}
		return false
	case "java", "kotlin", "scala":
		// JVM platform packages: the JDK (java.* / javax.*), the Jakarta
		// EE successor (jakarta.*), and the JDK-internal trees (jdk.* /
		// sun.* / com.sun.*). Everything else — including Kotlin/Scala
		// stdlibs, which ship as ordinary Maven artifacts — is treated as
		// a genuine dependency.
		return hasDottedPrefix(importPath, "java", "javax", "jakarta", "jdk", "sun") ||
			strings.HasPrefix(importPath, "com.sun.")
	case "csharp", "fsharp":
		// The .NET base class library: System.* and Microsoft.* (the
		// framework-shipped namespaces) plus the legacy mscorlib. Third
		// party NuGet packages live under their own vendor namespaces.
		return hasDottedPrefix(importPath, "System", "Microsoft", "mscorlib", "netstandard")
	}
	return false
}

// hasDottedPrefix reports whether importPath equals one of roots or has
// it as a dotted-namespace prefix (`java` matches `java` and `java.util`
// but not `javafx`). Used by the JVM / .NET stdlib filters where the
// platform namespace is the first dotted component.
func hasDottedPrefix(importPath string, roots ...string) bool {
	for _, r := range roots {
		if importPath == r || strings.HasPrefix(importPath, r+".") {
			return true
		}
	}
	return false
}

// pyTopLevelModule returns the first dotted component of a Python import
// path — `os.path` → `os`, `xml.etree.ElementTree` → `xml`. The stdlib
// membership test keys on the top-level package.
func pyTopLevelModule(importPath string) string {
	if i := strings.IndexByte(importPath, '.'); i >= 0 {
		return importPath[:i]
	}
	return importPath
}

// isNodeCoreModule reports whether spec names a Node.js built-in module.
// Accepts both the bare form (`fs`) and the `node:` protocol form
// (`node:fs`) — modern Node code uses the prefixed spelling. A subpath
// like `stream/promises` is matched on its first segment.
func isNodeCoreModule(spec string) bool {
	s := strings.TrimPrefix(spec, "node:")
	if i := strings.IndexByte(s, '/'); i >= 0 {
		s = s[:i]
	}
	_, ok := nodeCoreModules[s]
	return ok
}

// nodeCoreModules is the set of Node.js standard-library module names.
// Calls into these are runtime built-ins, not third-party dependencies,
// so they are filtered out of external-call synthesis.
var nodeCoreModules = map[string]struct{}{
	"assert": {}, "async_hooks": {}, "buffer": {}, "child_process": {},
	"cluster": {}, "console": {}, "constants": {}, "crypto": {},
	"dgram": {}, "diagnostics_channel": {}, "dns": {}, "domain": {},
	"events": {}, "fs": {}, "http": {}, "http2": {}, "https": {},
	"inspector": {}, "module": {}, "net": {}, "os": {}, "path": {},
	"perf_hooks": {}, "process": {}, "punycode": {}, "querystring": {},
	"readline": {}, "repl": {}, "stream": {}, "string_decoder": {},
	"sys": {}, "timers": {}, "tls": {}, "trace_events": {}, "tty": {},
	"url": {}, "util": {}, "v8": {}, "vm": {}, "wasi": {},
	"worker_threads": {}, "zlib": {},
}

// isPythonStdlib reports whether a top-level module name belongs to the
// Python standard library. The set covers the modules that realistically
// surface in extracted call edges; an unlisted stdlib module is treated
// as external (one extra synthetic node) rather than risk filtering a
// real package.
func isPythonStdlib(top string) bool {
	_, ok := pythonStdlibModules[top]
	return ok
}

// pythonStdlibModules is the set of Python standard-library top-level
// package names. Calls into these are interpreter built-ins, not pip
// dependencies, and are filtered out of external-call synthesis.
var pythonStdlibModules = map[string]struct{}{
	"abc": {}, "argparse": {}, "array": {}, "ast": {}, "asyncio": {},
	"base64": {}, "bisect": {}, "builtins": {}, "calendar": {},
	"collections": {}, "concurrent": {}, "contextlib": {}, "copy": {},
	"csv": {}, "ctypes": {}, "dataclasses": {}, "datetime": {},
	"decimal": {}, "difflib": {}, "dis": {}, "enum": {}, "errno": {},
	"functools": {}, "gc": {}, "getpass": {}, "glob": {}, "gzip": {},
	"hashlib": {}, "heapq": {}, "hmac": {}, "html": {}, "http": {},
	"importlib": {}, "inspect": {}, "io": {}, "ipaddress": {},
	"itertools": {}, "json": {}, "logging": {}, "math": {}, "mmap": {},
	"multiprocessing": {}, "operator": {}, "os": {}, "pathlib": {},
	"pickle": {}, "platform": {}, "pprint": {}, "queue": {}, "random": {},
	"re": {}, "secrets": {}, "select": {}, "shlex": {}, "shutil": {},
	"signal": {}, "socket": {}, "sqlite3": {}, "ssl": {}, "stat": {},
	"string": {}, "struct": {}, "subprocess": {}, "sys": {},
	"tempfile": {}, "textwrap": {}, "threading": {}, "time": {},
	"timeit": {}, "traceback": {}, "types": {}, "typing": {},
	"unittest": {}, "urllib": {}, "uuid": {}, "warnings": {},
	"weakref": {}, "xml": {}, "zipfile": {}, "zlib": {},
}
