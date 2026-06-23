package resolver

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// resolveRelativeImports rewrites Python and Dart relative-import edges
// onto the internal `KindFile` node they actually reference. The Go
// resolver's resolveImport / dep-module bridge target language-agnostic
// directory paths, which never line up with Python file stems or Dart
// `..`-walking URIs; without this pass, every relative import landed as
// an `external::*` stub and the subsequent module-attribution sweep
// either left them alone (Dart) or mis-attributed them to a phantom
// pypi package called after the first path segment (Python).
//
// Runs serially after the main resolve loop and BEFORE
// attributeNonGoModuleImports so that any edge resolved to an internal
// file no longer participates in pypi/pub attribution. Edges whose
// target file is not in the graph stay as `external::*` so the
// module-attribution pass can decide what to do with them.
func (r *Resolver) resolveRelativeImports() {
	// Relative-import resolution for Python / Dart relative imports and
	// C-family quoted includes; skip the File-node + edge walk when the graph
	// has none of those languages.
	if !r.graphHasLanguage("python") && !r.graphHasLanguage("dart") &&
		!r.graphHasLanguage("c") && !r.graphHasLanguage("cpp") && !r.graphHasLanguage("objc") {
		return
	}
	fileLang := r.collectFileLanguages()
	var reindexBatch []graph.EdgeReindex

	// Pre-build a map of every KindFile node's ID. The relative-
	// import resolvers below check 1-2 candidate IDs per edge to
	// decide whether a target file exists; doing that as a per-edge
	// GetNode (a per-edge round-trip on a disk backend) is what made
	// this pass dominate disk-backed resolve time. One NodesByKind scan
	// materialises the set once at indexed cost; lookups become
	// O(1) map hits.
	fileIDs := make(map[string]struct{}, 1024)
	// filesByBase indexes every KindFile by its basename so a non-relative
	// C-family include (`foo/bar.h`) can be resolved against an include root
	// without enumerating the whole file set per include (the `-I` search).
	filesByBase := make(map[string][]string, 1024)
	for n := range r.graph.NodesByKind(graph.KindFile) {
		if n != nil && n.ID != "" {
			fileIDs[n.ID] = struct{}{}
			base := n.ID
			if i := strings.LastIndex(n.ID, "/"); i >= 0 {
				base = n.ID[i+1:]
			}
			filesByBase[base] = append(filesByBase[base], n.ID)
		}
	}
	resolvePython := func(stem string) string {
		if !strings.Contains(stem, "/") {
			return ""
		}
		for _, cand := range []string{stem + ".py", stem + "/__init__.py"} {
			if _, ok := fileIDs[cand]; ok {
				return cand
			}
		}
		return ""
	}
	resolveCInclude := func(importingFile, rel string) string {
		if rel == "" {
			return ""
		}
		dir := ""
		if i := strings.LastIndex(importingFile, "/"); i >= 0 {
			dir = importingFile[:i]
		}
		for _, cand := range []string{joinRelativePath(dir, rel), joinRelativePath(dir, "./"+rel)} {
			if cand != "" {
				if _, ok := fileIDs[cand]; ok {
					return cand
				}
			}
		}
		// Include-root (`-I`) search: a multi-segment include (`foo/bar.h`) that
		// is not relative to the including file resolves to the uniquely-matching
		// indexed header whose path ends with that suffix — i.e. reachable via
		// some include directory (every repo dir is a candidate root, the same
		// set a compile_commands.json `-I` list would name). Restricted to
		// multi-segment paths and a unique match so an ambiguous bare header
		// never binds a false edge.
		if strings.Contains(rel, "/") {
			base := rel
			if i := strings.LastIndex(rel, "/"); i >= 0 {
				base = rel[i+1:]
			}
			suffix := "/" + rel
			match := ""
			for _, cand := range filesByBase[base] {
				if cand == rel || strings.HasSuffix(cand, suffix) {
					if match != "" && match != cand {
						return "" // ambiguous across include roots — refuse
					}
					match = cand
				}
			}
			if match != "" {
				return match
			}
		}
		return ""
	}
	resolveDart := func(importingFile, uri string) string {
		if uri == "" || strings.HasPrefix(uri, "dart:") || strings.HasPrefix(uri, "package:") {
			return ""
		}
		dir := ""
		if i := strings.LastIndex(importingFile, "/"); i >= 0 {
			dir = importingFile[:i]
		}
		target := joinRelativePath(dir, uri)
		if target == "" {
			return ""
		}
		if _, ok := fileIDs[target]; ok {
			return target
		}
		return ""
	}

	// EdgesByKind pushes the "kind = imports" filter into the store;
	// disk backends only enumerate import edges instead of every
	// edge in the graph.
	for e := range r.graph.EdgesByKind(graph.EdgeImports) {
		lang, ok := fileLang[e.From]
		if !ok {
			continue
		}
		var path string
		var resolved string
		switch {
		case strings.HasPrefix(e.To, "unresolved::pyrel::"):
			// Python parser-emitted relative-import placeholder.
			// Always resolvable via internal-file lookup.
			path = strings.TrimPrefix(e.To, "unresolved::pyrel::")
			if lang == "python" {
				resolved = resolvePython(path)
			}
		case strings.HasPrefix(e.To, "external::"):
			// Fallthrough path for Dart relative URIs the main
			// resolveImport sweep landed at `external::*`, plus a
			// safety net for any Python relative stem that arrived
			// here without the `pyrel::` marker.
			path = strings.TrimPrefix(e.To, "external::")
			switch lang {
			case "python":
				resolved = resolvePython(path)
			case "dart":
				resolved = resolveDart(e.From, path)
			}
		default:
			// C-family quoted include: `#include "foo.h"` / `#import "Foo.h"`
			// resolves relative to the including file's directory.
			if lang == "c" || lang == "cpp" || lang == "objc" {
				if k, _ := e.Meta["include_kind"].(string); k == "quoted" &&
					strings.HasPrefix(e.To, "unresolved::import::") {
					path = strings.TrimPrefix(e.To, "unresolved::import::")
					resolved = resolveCInclude(e.From, path)
				}
			}
			if resolved == "" {
				continue
			}
		}
		if resolved == "" {
			// pyrel:: edges that don't find an internal target are
			// downgraded to `external::` so the module-attribution
			// pass + audits don't see the internal marker prefix.
			if strings.HasPrefix(e.To, "unresolved::pyrel::") {
				oldTo := e.To
				e.To = "external::" + path
				reindexBatch = append(reindexBatch, graph.EdgeReindex{Edge: e, OldTo: oldTo})
			}
			continue
		}
		oldTo := e.To
		e.To = resolved
		e.Origin = graph.OriginASTResolved
		reindexBatch = append(reindexBatch, graph.EdgeReindex{Edge: e, OldTo: oldTo})
	}
	if len(reindexBatch) > 0 {
		r.graph.ReindexEdges(reindexBatch)
	}
}

// joinRelativePath joins a relative URI onto a directory and collapses
// `.`/`..` segments. Returns "" when the path walks above the repo root
// (which we never want to silently silently fall through to an
// arbitrary file).
func joinRelativePath(dir, rel string) string {
	var parts []string
	if dir != "" {
		parts = strings.Split(dir, "/")
	}
	for _, seg := range strings.Split(rel, "/") {
		switch seg {
		case "", ".":
			// noop
		case "..":
			if len(parts) == 0 {
				return ""
			}
			parts = parts[:len(parts)-1]
		default:
			parts = append(parts, seg)
		}
	}
	return strings.Join(parts, "/")
}
