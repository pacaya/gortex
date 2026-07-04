package resolver

import (
	"path/filepath"
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// tryResolveViaLSP attempts to bind e to a graph node using the
// configured LSPHelper. Returns true when the edge has been
// resolved (e.To rewritten + stats incremented + Origin stamped).
// On false the caller falls through to the heuristic cascade.
//
// The target string is the unresolved-prefix-stripped form of e.To,
// matching the value resolveEdge already computed. We expect one of:
//   - "import::<path>"       → import edge, ask LSP for the module file
//   - "extern::<path>::<sym>"→ already specific, LSP rarely improves it
//   - "*.<name>"             → method/field/property call by selector
//   - "<name>"               → bare function / type / token reference
//
// LSP-hot-path is intentionally narrow: it consults the helper, asks
// for the *definition* location of the identifier at e.Line in
// e.FilePath, and binds the edge to the graph node at that location.
// The helper is responsible for opening files, serialising calls
// against the underlying language server, and applying a per-call
// timeout. A nil helper or a helper that doesn't claim e.FilePath
// short-circuits to a fast false.
func (r *Resolver) tryResolveViaLSP(e *graph.Edge, target string, stats *ResolveStats) bool {
	if r.lspHelper == nil || e == nil || e.FilePath == "" || e.Line <= 0 {
		return false
	}
	if !r.lspHelper.SupportsPath(e.FilePath) {
		return false
	}

	// Strip the resolver's structural prefixes so the helper sees a
	// bare identifier. Each branch normalises to the canonical name
	// the source-file would actually contain at e.Line — i.e. what
	// the LSP server can locate via textDocument/definition.
	name := identifierFromTarget(target)
	if name == "" {
		return false
	}

	defRelPath, defLine, ok := r.lspHelper.Definition(e.FilePath, e.Line, name)
	if !ok || defRelPath == "" || defLine <= 0 {
		return false
	}

	// Normalise path. Tsserver's response is absolute; the graph
	// keeps relative paths anchored at the repo root. The helper
	// normalises before returning, but defend against trailing
	// drift (`./` prefix, "" path).
	defRelPath = strings.TrimPrefix(defRelPath, "./")

	node := r.lookupNodeByLocation(defRelPath, defLine, name)
	if node == nil {
		return false
	}

	// Reject obviously-wrong kinds for the edge. A `calls` edge
	// landing on a KindFile or KindImport is a misresolution we'd
	// prefer to expose by falling through to the heuristic than
	// silently bind. Type-hierarchy edges must land on a type or
	// interface for the same reason resolveTypeRef gates them.
	if !lspKindAcceptableFor(e.Kind, node.Kind) {
		return false
	}

	e.To = node.ID
	if e.Confidence < 1.0 {
		e.Confidence = 1.0
	}
	e.Origin = graph.OriginLSPResolved
	if e.Meta == nil {
		e.Meta = map[string]any{}
	}
	e.Meta["resolved_by"] = "lsp"

	// Mirror the heuristic-path promotion in resolver.go: when an
	// EdgeReads target resolves to a function or method (h.foo passed
	// as a method value, or a bare `runClean` passed as a struct
	// field like `RunE: runClean`), promote to EdgeReferences so
	// get_callers and find_usages surface the reference. Without
	// this, every routing-style codebase (HTTP handlers, command
	// tables, callback maps, cobra/CLI wiring) silently looks like
	// its handlers have zero callers — the LSP hot path was binding
	// them but leaving the EdgeReads kind, which the query allowlist
	// drops. Writes stay as EdgeWrites: assigning a func value to a
	// method-typed field slot is still a write semantically.
	if e.Kind == graph.EdgeReads && (node.Kind == graph.KindMethod || node.Kind == graph.KindFunction) {
		e.Kind = graph.EdgeReferences
	}

	// Multi-repo tracking: if the resolved node lives in a
	// different repo than the caller, mark CrossRepo so the
	// downstream cross-repo materialisation pass picks it up.
	if callerRepo := r.callerRepoPrefix(e); callerRepo != "" && node.RepoPrefix != "" && node.RepoPrefix != callerRepo {
		e.CrossRepo = true
	}

	stats.Resolved++
	return true
}

// deferredLSPEdge is one entry in the bulk-mode deferred LSP batch: the live
// edge plus the pre-heuristic identifier target captured before the heuristic
// cascade mutated it. The target is snapshotted while e.To is still the
// `unresolved::` stub, because by the time the deferred batch runs the edge
// may already carry a heuristic-resolved node ID from which the original
// identifier can no longer be recovered.
type deferredLSPEdge struct {
	edge   *graph.Edge
	target string
}

// lspDeferTarget reports whether a bulk-mode ResolveAll should collect e for
// the deferred LSP batch and, when so, returns the pre-heuristic identifier
// target the helper will look up. Mirrors tryResolveViaLSP's up-front gating
// (helper present, real file position, supported extension, a bare identifier
// the helper can locate) so the batch only carries edges the helper could
// actually bind. Called from the parallel resolve workers on the live edge
// BEFORE resolveEdge runs on its clone, so e.To is still the `unresolved::`
// stub here and the derived target is the pre-heuristic one. Read-only.
func (r *Resolver) lspDeferTarget(e *graph.Edge) (string, bool) {
	if r.lspHelper == nil || e == nil || e.FilePath == "" || e.Line <= 0 {
		return "", false
	}
	if !graph.IsUnresolvedTarget(e.To) {
		return "", false
	}
	if !r.lspHelper.SupportsPath(e.FilePath) {
		return "", false
	}
	target := graph.UnresolvedName(e.To)
	if target == "" {
		target = strings.TrimPrefix(e.To, unresolvedPrefix)
	}
	if identifierFromTarget(target) == "" {
		return "", false
	}
	return target, true
}

// resolveDeferredLSP binds the LSP-eligible edges the bulk-mode compute loop
// collected through the installed helper, applying every hit via one
// ReindexEdges call. It runs AFTER the parallel chunk loop so a synchronous
// textDocument/definition round-trip never stalls the heuristic worker fan-out
// at its barrier.
//
// The batch carries EVERY LSP-eligible edge, not only the ones the heuristic
// cascade left unresolved: this is what preserves the LSP-first override the
// inline (non-bulk) path applies. The heuristic can confidently bind an edge
// to the WRONG node (e.g. a same-directory sibling that shadows a symbol whose
// real import the resolver can't expand); the type-aware helper re-binds it to
// the correct definition here, exactly as running LSP-first would have. Each
// entry's target is the pre-heuristic identifier captured before the cascade
// ran, so the helper is queried by the source-file identifier even for an edge
// whose live To now points at a heuristic-resolved node.
//
// A successful bind stamps OriginLSPResolved (via tryResolveViaLSP), which is
// also the signal the cross-package guard uses to leave these edges alone.
//
// The helper serialises its own language-server calls, so the batch walks the
// edges serially, grouped by file for locality in the helper's open-file set
// and lookupNodeByLocation's per-file index. The win over the inline path is
// that these calls no longer contend on the helper lock inside the parallel
// workers, and the balanced heuristic phase completes without LSP stragglers.
//
// Caller holds r.mu (the deferred batch is invoked from inside ResolveAll,
// while the per-pass lookup / lsp indexes are still live). Returns the number
// of edges that were heuristic-UNRESOLVED before the helper bound them — only
// those move the pass tally from Unresolved to Resolved. Overriding an
// already-resolved heuristic bind changes the target but not the count.
func (r *Resolver) resolveDeferredLSP(edges []deferredLSPEdge) int {
	if len(edges) == 0 || r.lspHelper == nil {
		return 0
	}
	byFile := make(map[string][]deferredLSPEdge, len(edges))
	files := make([]string, 0, len(edges))
	for _, de := range edges {
		if de.edge == nil {
			continue
		}
		fp := de.edge.FilePath
		if _, seen := byFile[fp]; !seen {
			files = append(files, fp)
		}
		byFile[fp] = append(byFile[fp], de)
	}
	sort.Strings(files)

	var stats ResolveStats
	newlyResolved := 0
	reindexBatch := make([]graph.EdgeReindex, 0, len(edges))
	for _, f := range files {
		for _, de := range byFile[f] {
			e := de.edge
			// A concurrent single-file edit during an inter-chunk yield may
			// have evicted this edge since it was collected; skip anything no
			// longer in the graph so we don't half-resurrect an evicted edge.
			// A resolved-but-live edge is NOT skipped: the heuristic may have
			// confidently bound it to the wrong node, and the LSP override
			// below is exactly what corrects that.
			if r.validateLiveness && !edgeStillLive(r.graph, e) {
				continue
			}
			oldTo := e.To
			wasUnresolved := graph.IsUnresolvedTarget(oldTo)
			if r.tryResolveViaLSP(e, de.target, &stats) {
				reindexBatch = append(reindexBatch, graph.EdgeReindex{Edge: e, OldTo: oldTo})
				if wasUnresolved {
					newlyResolved++
				}
			}
		}
	}
	if len(reindexBatch) > 0 {
		r.graph.ReindexEdges(reindexBatch)
	}
	return newlyResolved
}

// identifierFromTarget extracts the bare identifier from a resolver
// target string. Mirrors the branches in resolveEdge: strips the
// `*.` selector prefix and the `extern::<path>::` package qualifier.
// Returns "" for shapes the LSP-hot-path can't handle (import::,
// pyrel::, grpc:: — those are routed through dedicated passes).
func identifierFromTarget(target string) string {
	switch {
	case strings.HasPrefix(target, "*."):
		return strings.TrimPrefix(target, "*.")
	case strings.HasPrefix(target, "extern::"):
		// extern::<importPath>::<symbol>
		spec := strings.TrimPrefix(target, "extern::")
		sep := strings.LastIndex(spec, "::")
		if sep < 0 {
			return ""
		}
		return spec[sep+2:]
	case strings.HasPrefix(target, "import::"),
		strings.HasPrefix(target, "pyrel::"),
		strings.HasPrefix(target, "grpc::"):
		// LSP doesn't improve module-path resolution; let the
		// dedicated passes own these.
		return ""
	}
	return target
}

// lookupNodeByLocation finds the graph node whose declaration starts
// at (relPath, oneBasedLine). Lazily builds an O(1) index per pass
// so repeated LSP hits in the same file don't rescan the graph.
//
// `nameHint` (when non-empty) narrows the match when the cache miss
// has to walk multiple nodes that start on the same line — common
// for one-liner exports like `export const X = 1; export const Y = 2;`.
func (r *Resolver) lookupNodeByLocation(relPath string, oneBasedLine int, nameHint string) *graph.Node {
	key := lspLocKey{filePath: relPath, line: oneBasedLine}

	r.lspIndexMu.RLock()
	if r.lspIndex != nil {
		if n, ok := r.lspIndex[key]; ok {
			r.lspIndexMu.RUnlock()
			if nameHint != "" && n != nil && n.Name != nameHint {
				// Index entry was a previous resolution for a
				// different identifier on the same line — fall
				// back to a name-aware scan.
				return r.scanNodeAtLocation(relPath, oneBasedLine, nameHint)
			}
			return n
		}
	}
	r.lspIndexMu.RUnlock()

	n := r.scanNodeAtLocation(relPath, oneBasedLine, nameHint)
	if n == nil {
		return nil
	}

	r.lspIndexMu.Lock()
	if r.lspIndex == nil {
		r.lspIndex = make(map[lspLocKey]*graph.Node)
	}
	r.lspIndex[key] = n
	r.lspIndexMu.Unlock()
	return n
}

// scanNodeAtLocation finds the graph node whose declaration line
// matches (relPath, oneBasedLine). Prefers an exact StartLine hit;
// if multiple nodes share that start line, prefers a name match.
// Returns nil when no node anchors there.
func (r *Resolver) scanNodeAtLocation(relPath string, oneBasedLine int, nameHint string) *graph.Node {
	nodes := r.graph.GetFileNodes(relPath)
	if len(nodes) == 0 {
		// Fallback: tsserver may return a path with platform-
		// specific separators or a slightly different case
		// (macOS HFS+). Try the canonicalised form.
		alt := filepath.ToSlash(relPath)
		if alt != relPath {
			nodes = r.graph.GetFileNodes(alt)
		}
		if len(nodes) == 0 {
			return nil
		}
	}

	var fallback *graph.Node
	for _, n := range nodes {
		if n == nil {
			continue
		}
		if n.Kind == graph.KindFile || n.Kind == graph.KindImport {
			continue
		}
		if n.StartLine != oneBasedLine {
			continue
		}
		if nameHint == "" || n.Name == nameHint {
			return n
		}
		if fallback == nil {
			fallback = n
		}
	}
	if fallback != nil {
		return fallback
	}

	// Looser match: tsserver sometimes reports the position of the
	// identifier on a line shifted by one (e.g. the JSDoc above the
	// declaration). Accept a node whose StartLine is within ±1 of
	// the LSP location when names agree.
	if nameHint != "" {
		for _, n := range nodes {
			if n == nil || n.Kind == graph.KindFile || n.Kind == graph.KindImport {
				continue
			}
			if n.Name != nameHint {
				continue
			}
			if delta := n.StartLine - oneBasedLine; delta >= -1 && delta <= 1 {
				return n
			}
		}
	}
	return nil
}

// clearLSPIndex drops the per-pass lookup cache.
func (r *Resolver) clearLSPIndex() {
	r.lspIndexMu.Lock()
	r.lspIndex = nil
	r.lspIndexMu.Unlock()
}

// lspKindAcceptableFor reports whether a node of kind `nodeKind` is
// a sensible target for an edge of kind `edgeKind`. Mirrors the
// type-system gates the heuristic resolvers apply (e.g.
// resolveTypeRef rejects function/method candidates for extends/
// implements edges).
func lspKindAcceptableFor(edgeKind graph.EdgeKind, nodeKind graph.NodeKind) bool {
	switch edgeKind {
	case graph.EdgeExtends, graph.EdgeImplements, graph.EdgeComposes:
		return nodeKind == graph.KindType || nodeKind == graph.KindInterface
	case graph.EdgeCalls:
		switch nodeKind {
		case graph.KindFunction, graph.KindMethod, graph.KindType, graph.KindClosure:
			return true
		default:
			return false
		}
	case graph.EdgeReads, graph.EdgeWrites:
		switch nodeKind {
		case graph.KindField, graph.KindVariable, graph.KindConstant, graph.KindMethod, graph.KindFunction:
			return true
		default:
			return false
		}
	case graph.EdgeReferences, graph.EdgeInstantiates:
		switch nodeKind {
		case graph.KindFile, graph.KindImport, graph.KindPackage:
			return false
		}
		return true
	case graph.EdgeProvides, graph.EdgeConsumes:
		switch nodeKind {
		case graph.KindFile, graph.KindImport:
			return false
		}
		return true
	}
	// Default: anything goes that isn't a file/import. File/import
	// nodes are containers, never the semantic target of a code
	// reference.
	if nodeKind == graph.KindFile || nodeKind == graph.KindImport {
		return false
	}
	return true
}
