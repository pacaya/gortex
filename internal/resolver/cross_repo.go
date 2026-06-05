package resolver

import (
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
)

// CrossRepoStats holds counts from a cross-repo resolution pass.
type CrossRepoStats struct {
	Resolved       int            `json:"resolved"`
	Unresolved     int            `json:"unresolved"`
	CrossRepoEdges int            `json:"cross_repo_edges"`
	ByRepo         map[string]int `json:"by_repo"`
}

// CrossWorkspaceDepRule names one allowed dependency from a source
// workspace into another. Mirrors config.CrossWorkspaceDep but lives
// here so the resolver doesn't import internal/config (avoids a cycle
// once future steps wire workspace plumbing through manager.go).
type CrossWorkspaceDepRule struct {
	// Workspace is the *target* workspace slug — the workspace whose
	// nodes are eligible to be referenced from the source workspace.
	Workspace string
	// Modules is the list of import-path prefixes that the source
	// workspace is allowed to follow into the target. Iteration 1
	// only supports prefix-style matches (longest prefix wins).
	Modules []string
}

// CrossWorkspaceDepLookup returns the list of declared cross-workspace
// dependencies for a *source* workspace. Empty / nil result means the
// source workspace has no declared cross-workspace deps and so the
// resolver must keep cross-workspace candidates ineligible.
type CrossWorkspaceDepLookup func(sourceWorkspaceID string) []CrossWorkspaceDepRule

// CrossRepoResolver resolves unresolved edges across repository boundaries.
//
// dirIndex / lastDirIndex are scratch maps populated for the duration
// of a single Resolve* pass — they let resolveImport look up candidate
// file nodes by directory in O(1) instead of scanning the whole graph
// (which is O(N) per import edge, O(N×M) total). Maps are nil between
// passes so we don't pay the memory cost while idle.
//
// mu is the graph-wide resolver lock shared with every Resolver built
// from the same Graph. Private to CrossRepoResolver wasn't enough:
// MultiWatcher.forwardEvents calls ResolveForRepo while the per-repo
// Watcher's debounce timer concurrently calls Resolver.ResolveFile,
// and both paths iterate graph.AllEdges() / AllNodes() and mutate
// Edge.To in place. Sharing g.ResolveMutex() serialises both resolver
// types against the same graph.
//
// crossWorkspaceLookup is the workspace-boundary check. Empty (nil)
// means the resolver is in legacy mode: cross-repo / cross-workspace
// candidates resolve as if no boundary existed — for callers that
// haven't plumbed config through yet. When set, candidates whose
// WorkspaceID differs from
// the caller's are accepted only when the source workspace declared
// the target workspace via `cross_workspace_deps` AND, for import
// edges, the import path has a declared-module prefix.
type CrossRepoResolver struct {
	graph graph.Store
	// nodeByID / nodesByName: per-pass batched lookup cache, the
	// cross-repo mirror of the fields on Resolver (resolver.go).
	// Populated by warmLookupCache before the per-edge fan-out and
	// cleared on return; cachedGetNode / cachedFindNodesByName consult
	// them first. Without it the cross-repo pass fires one
	// GetNode/FindNodesByName query per pending edge — across 200k+
	// unresolved edges that is a warmup hang on disk backends.
	logger          *zap.Logger
	nodeByID        map[string]*graph.Node
	nodesByName     map[string][]*graph.Node
	nodesByNameRepo map[string]map[string][]*graph.Node
	nodesByQualName map[string]*graph.Node
	dirIndex        map[string][]*graph.Node
	lastDirIndex    map[string][]*graph.Node
	// reachableReposByFile maps a caller file's ID to the set of repo
	// prefixes that file imports (derived from resolved EdgeImports
	// edges). It is the import-reachability evidence gate: a name-only
	// cross-repo function/method/type candidate is eligible only when
	// the caller's file actually imports the candidate's repo. Without
	// it, `FindNodesByName` spanning a multi-repo graph resolves short
	// common names (`len`, `string`, `Language`, `set`) to whichever
	// repo sorts first — the name-collision false positives M3's
	// analyzer surfaced. Built once per Resolve* pass, torn down after.
	reachableReposByFile map[string]map[string]struct{}
	// depModuleIndex bridges Go imports to dep::<module> contract
	// nodes from the caller's go.mod. Same shape and rationale as
	// the field of the same name on Resolver — see resolver.go for
	// the full doc. Cross-repo always scopes by callerRepo, so a
	// dep declared by repo A's go.mod never satisfies an import in
	// repo B even if the module path matches.
	depModuleIndex       map[string][]depModuleEntry
	mu                   *sync.Mutex
	crossWorkspaceLookup CrossWorkspaceDepLookup
	// npmAlias rewrites a JS/TS import specifier that matches an
	// npm-alias dependency key in the importing file's nearest-
	// ancestor package.json. Same contract as the field of the
	// same name on Resolver — see npm_alias.go.
	npmAlias NpmAliasResolver
	// workspaceMembers maps a file path to the package-manager
	// workspace it belongs to, used to prefer a same-workspace
	// candidate on a same-named import collision. Same contract as
	// the field of the same name on Resolver — see
	// workspace_membership.go.
	workspaceMembers WorkspaceMembership
}

// NewCrossRepo creates a CrossRepoResolver for the given graph.
func NewCrossRepo(g graph.Store) *CrossRepoResolver {
	return &CrossRepoResolver{graph: g, mu: g.ResolveMutex(), logger: zap.NewNop()}
}

// SetLogger attaches a logger so ResolveAll emits pass progress (the
// cross-repo mirror of Resolver.SetLogger). A nil logger becomes a no-op.
func (cr *CrossRepoResolver) SetLogger(l *zap.Logger) {
	if l == nil {
		l = zap.NewNop()
	}
	cr.logger = l
}

// SetCrossWorkspaceDepLookup wires the boundary rule. After this
// call, the resolver will refuse cross-workspace candidates that
// aren't covered by an explicit declaration in the source workspace's
// `cross_workspace_deps`. Legacy graphs (no WorkspaceID on either
// side) keep working — when both From and To carry empty workspace
// slugs the boundary check trivially passes.
func (cr *CrossRepoResolver) SetCrossWorkspaceDepLookup(lookup CrossWorkspaceDepLookup) {
	cr.crossWorkspaceLookup = lookup
}

// callerWorkspaceID returns the workspace slug for the From-side of
// an edge. Falls back to RepoPrefix to match Contract.Effective-
// Workspace's "missing → repo-name" rule.
func (cr *CrossRepoResolver) callerWorkspaceID(e *graph.Edge) string {
	from := cr.cachedGetNode(e.From)
	if from == nil {
		return ""
	}
	if from.WorkspaceID != "" {
		return from.WorkspaceID
	}
	return from.RepoPrefix
}

// candidateWorkspaceID extracts the same slug from a candidate node.
func candidateWorkspaceID(n *graph.Node) string {
	if n == nil {
		return ""
	}
	if n.WorkspaceID != "" {
		return n.WorkspaceID
	}
	return n.RepoPrefix
}

// crossWorkspaceEligible reports whether sourceWS is permitted to
// reach a candidate in targetWS, optionally constrained by the
// candidate's import path. importPath == "" means "any module"
// (function/method calls — they don't carry an import path so the
// only check is workspace-pair declaration).
func (cr *CrossRepoResolver) crossWorkspaceEligible(sourceWS, targetWS, importPath string) bool {
	if sourceWS == targetWS {
		return true
	}
	if cr.crossWorkspaceLookup == nil {
		// Legacy / unwired callers: no boundary enforcement.
		return true
	}
	rules := cr.crossWorkspaceLookup(sourceWS)
	for _, rule := range rules {
		if rule.Workspace != targetWS {
			continue
		}
		if importPath == "" {
			// Function/method call into a declared cross-workspace
			// dep is allowed once the workspace pair is declared —
			// iteration 1 doesn't try to require an import-path
			// match for non-import edges.
			return true
		}
		for _, m := range rule.Modules {
			if m == importPath || strings.HasPrefix(importPath, m+"/") {
				return true
			}
		}
	}
	return false
}

// pickImportCandidate chooses the best cross-repo file candidate for an
// import: a candidate in the importer's own workspace wins outright;
// otherwise the first candidate the cross-workspace policy permits is
// used. Returns nil when no candidate clears the boundary, so the caller
// falls through to its dep-module / external handling.
//
// This replaces the old "first dir match, then a single boundary check
// that bailed to external" rule, which mis-resolved when two same-named
// modules live in different workspaces — the worktree-instance case,
// where the importer's workspace has its own copy of the imported module
// but the canonical copy (in another workspace) sorted first.
func (cr *CrossRepoResolver) pickImportCandidate(callerWS, importPath string, candidates []*graph.Node) *graph.Node {
	for _, c := range candidates {
		if candidateWorkspaceID(c) == callerWS {
			return c
		}
	}
	for _, c := range candidates {
		if cr.crossWorkspaceEligible(callerWS, candidateWorkspaceID(c), importPath) {
			return c
		}
	}
	return nil
}

// pickQualNameCandidate enumerates every node that shares qualName and
// returns the one in the caller's own workspace, else the first the
// cross-workspace policy permits, else nil. The graph's qual-name index
// is single-valued, so when the same module is checked out twice (a
// canonical checkout plus a worktree instance under its own prefix) only
// one node is reachable by qual name; the two share a Name, so the
// multi-valued by-name index recovers the full candidate set. `single`
// is the node the single-valued lookup already returned — used to learn
// the shared Name without a second qual-name probe.
func (cr *CrossRepoResolver) pickQualNameCandidate(callerWS, qualName string, single *graph.Node) *graph.Node {
	if single == nil || single.Name == "" {
		return nil
	}
	all := cr.graph.FindNodesByNames([]string{single.Name})[single.Name]
	var cands []*graph.Node
	for _, c := range all {
		if c != nil && c.QualName == qualName {
			cands = append(cands, c)
		}
	}
	for _, c := range cands {
		if candidateWorkspaceID(c) == callerWS {
			return c
		}
	}
	for _, c := range cands {
		if cr.crossWorkspaceEligible(callerWS, candidateWorkspaceID(c), qualName) {
			return c
		}
	}
	return nil
}

// ResolveAll resolves all unresolved edges in the graph, trying same-repo
// matches first, then cross-repo search. Sets Edge.CrossRepo = true for
// cross-repo matches.
func (cr *CrossRepoResolver) ResolveAll() *CrossRepoStats {
	cr.mu.Lock()
	defer cr.mu.Unlock()

	cr.buildDirIndexes()
	defer cr.clearDirIndexes()
	cr.buildDepModuleIndex()
	defer cr.clearDepModuleIndex()
	cr.buildReachableReposIndex()
	defer cr.clearReachableReposIndex()

	stats := &CrossRepoStats{ByRepo: make(map[string]int)}

	// Predicate-shaped read: disk backends only enumerate the
	// "unresolved::*" slice (the only one this pass mutates). Batch
	// mutations to commit in chunks at the end.
	// Materialise the pending slice once so warmLookupCache can batch
	// the per-edge GetNode / FindNodesByName the cascade would otherwise
	// fire serially (the cross-repo warmup storm on disk backends).
	var pending []*graph.Edge
	for e := range cr.graph.EdgesWithUnresolvedTarget() {
		pending = append(pending, e)
	}
	cr.warmLookupCache(pending)
	defer cr.clearLookupCache()

	passStart := time.Now()
	cr.logger.Info("cross-repo resolve: pass start", zap.Int("pending", len(pending)))
	var processed atomic.Int64
	progressDone := make(chan struct{})
	go func() {
		t := time.NewTicker(3 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-progressDone:
				return
			case <-t.C:
				cr.logger.Info("cross-repo resolve: compute progress",
					zap.Int64("processed", processed.Load()),
					zap.Int("pending", len(pending)),
					zap.Duration("elapsed", time.Since(passStart)))
			}
		}
	}()

	// Resolve concurrently across NumCPU workers, mirroring the master
	// Resolver's pool. Each edge is touched by exactly one worker (disjoint
	// chunks); the per-pass caches/indexes are read-only here; each worker
	// accumulates into its OWN batch + stats — so no shared mutable state is
	// written concurrently. Batches are concatenated and applied once after
	// the barrier (cr never reindexes per-edge mid-loop, so unlike the
	// master pool no edge clone is needed); stats are summed.
	workers := runtime.NumCPU()
	// Clamp to the work count BEFORE flooring at 1: an empty pending slice
	// must leave workers >= 1 so the chunk division below can't divide by
	// zero. With workers == 1 and len(pending) == 0 the chunk is 0 and every
	// worker's [start,end) is empty, so the pass is a correct no-op.
	if workers > len(pending) {
		workers = len(pending)
	}
	if workers < 1 {
		workers = 1
	}
	perWorkerBatch := make([][]graph.EdgeReindex, workers)
	perWorkerStats := make([]*CrossRepoStats, workers)
	var wg sync.WaitGroup
	chunk := (len(pending) + workers - 1) / workers
	for w := 0; w < workers; w++ {
		start := w * chunk
		end := start + chunk
		if end > len(pending) {
			end = len(pending)
		}
		if start >= end {
			continue
		}
		wg.Add(1)
		go func(idx int, slice []*graph.Edge) {
			defer wg.Done()
			ws := &CrossRepoStats{ByRepo: make(map[string]int)}
			var batch []graph.EdgeReindex
			for _, e := range slice {
				cr.resolveEdge(e, ws, &batch)
				processed.Add(1)
			}
			perWorkerStats[idx] = ws
			perWorkerBatch[idx] = batch
		}(w, pending[start:end])
	}
	wg.Wait()
	close(progressDone)

	var reindexBatch []graph.EdgeReindex
	for i := range perWorkerBatch {
		reindexBatch = append(reindexBatch, perWorkerBatch[i]...)
	}
	for _, ws := range perWorkerStats {
		if ws == nil {
			continue
		}
		stats.Resolved += ws.Resolved
		stats.Unresolved += ws.Unresolved
		stats.CrossRepoEdges += ws.CrossRepoEdges
		for repo, n := range ws.ByRepo {
			stats.ByRepo[repo] += n
		}
	}
	cr.logger.Info("cross-repo resolve: compute done",
		zap.Int("pending", len(pending)),
		zap.Int("reindex_batch", len(reindexBatch)),
		zap.Int("workers", workers),
		zap.Duration("elapsed", time.Since(passStart)))
	if len(reindexBatch) > 0 {
		applyStart := time.Now()
		cr.graph.ReindexEdges(reindexBatch)
		cr.logger.Info("cross-repo resolve: apply done",
			zap.Int("edges", len(reindexBatch)),
			zap.Duration("elapsed", time.Since(applyStart)))
	}
	// Materialise the cross_repo_* edge layer over the freshly lifted
	// calls / implements / extends edges.
	DetectCrossRepoEdges(cr.graph)
	return stats
}

// ResolveForRepo resolves only unresolved edges originating from nodes
// in the specified repository.
func (cr *CrossRepoResolver) ResolveForRepo(repoPrefix string) *CrossRepoStats {
	cr.mu.Lock()
	defer cr.mu.Unlock()
	// One backend query for every out-edge from this repo's nodes,
	// instead of GetRepoNodes followed by GetOutEdges per node. On
	// disk backends (SQLite, DuckDB) the per-node loop
	// was O(repo_nodes) round-trips per pass — single-digit minutes
	// of warmup on a multi-repo workspace where this method runs
	// once per tracked repo.
	return cr.resolveScopedLocked(cr.graph.GetRepoEdges(repoPrefix))
}

// ResolveForFile is the watcher fast path: it re-resolves only the
// out-edges of the changed file, not the whole repo. The watcher fires
// after every single-file save, and the old ResolveForRepo path
// materialised the repo's ENTIRE edge set (hundreds of thousands of
// edges, each with its meta blob) on every keystroke-save — the
// dominant per-edit allocation flood and the cause of the
// "buffer pool is full" crash on a small resident pool. Scoping to the
// changed file's edges turns that into a GetFileNodes lookup plus one
// batched GetOutEdgesByNodeIDs, bounded by the file's size.
//
// relPath must be the repo-relative graph key — callers convert an
// absolute watcher path via Indexer.RelKey first. A path matching no
// nodes is a no-op.
//
// Scope note: this resolves edges the changed file OWNS. A new
// definition in this file that would resolve some OTHER file's pending
// unresolved edge (inbound resolution) is not re-checked here — that
// case is rare, self-heals when the referencing file is next touched,
// and is swept up by the periodic full ResolveAll. ResolveForRepo
// remains for warmup / global recompute.
func (cr *CrossRepoResolver) ResolveForFile(repoPrefix, relPath string) *CrossRepoStats {
	cr.mu.Lock()
	defer cr.mu.Unlock()
	nodes := cr.graph.GetFileNodes(relPath)
	if len(nodes) == 0 {
		return &CrossRepoStats{ByRepo: make(map[string]int)}
	}
	ids := make([]string, 0, len(nodes))
	for _, n := range nodes {
		if n != nil {
			ids = append(ids, n.ID)
		}
	}
	var edges []*graph.Edge
	for _, es := range cr.graph.GetOutEdgesByNodeIDs(ids) {
		edges = append(edges, es...)
	}
	return cr.resolveScopedLocked(edges)
}

// resolveScopedLocked lifts every unresolved target among edges to its
// real cross-repo node, then materialises the cross_repo_* parallel-edge
// layer. Shared by ResolveForRepo (whole-repo edge set) and
// ResolveForFile (one changed file's out-edges). Caller holds cr.mu.
func (cr *CrossRepoResolver) resolveScopedLocked(edges []*graph.Edge) *CrossRepoStats {
	cr.buildDirIndexes()
	defer cr.clearDirIndexes()
	cr.buildDepModuleIndex()
	defer cr.clearDepModuleIndex()
	cr.buildReachableReposIndex()
	defer cr.clearReachableReposIndex()

	stats := &CrossRepoStats{ByRepo: make(map[string]int)}
	var reindexBatch []graph.EdgeReindex
	for _, e := range edges {
		if e == nil || !strings.HasPrefix(e.To, unresolvedPrefix) {
			continue
		}
		cr.resolveEdge(e, stats, &reindexBatch)
	}
	if len(reindexBatch) > 0 {
		cr.graph.ReindexEdges(reindexBatch)
	}
	// Materialise the cross_repo_* edge layer. The pass is graph-wide
	// (cheap relative to a resolve pass) so an edge into repoPrefix
	// from another repo — lifted when that other repo was resolved —
	// also picks up its parallel edge once repoPrefix's nodes exist.
	DetectCrossRepoEdges(cr.graph)
	return stats
}

// buildDirIndexes walks the graph once and populates two lookup maps
// used by resolveImport — the only resolution path that previously
// scanned every node per edge.
//
//   - dirIndex     keys on filepath.Dir(file.FilePath) for exact matches
//     (importPath equal to the file's directory).
//   - lastDirIndex keys on the last path component of that directory,
//     covering the common case where an import path is a single name
//     like "logger" and we want any file under .../logger/.
//
// These maps are torn down via clearDirIndexes when the pass completes
// so we don't keep ~N pointers alive between resolves.
func (cr *CrossRepoResolver) buildDirIndexes() {
	cr.dirIndex = make(map[string][]*graph.Node, 128)
	cr.lastDirIndex = make(map[string][]*graph.Node, 128)
	for n := range cr.graph.NodesByKind(graph.KindFile) {
		dir := filepath.Dir(n.FilePath)
		cr.dirIndex[dir] = append(cr.dirIndex[dir], n)
		last := lastPathComponent(dir)
		if last != "" && last != dir {
			cr.lastDirIndex[last] = append(cr.lastDirIndex[last], n)
		}
	}
}

// buildDepModuleIndex mirrors Resolver.buildDepModuleIndex — see that
// method for the full rationale. Cross-repo always scopes the lookup
// by callerRepo, so the same dep node reachable here is the one in the
// importing file's own go.mod.
func (cr *CrossRepoResolver) buildDepModuleIndex() {
	by := make(map[string][]depModuleEntry)
	for n := range cr.graph.NodesByKind(graph.KindContract) {
		if !strings.HasPrefix(n.ID, "dep::") {
			continue
		}
		mp := strings.TrimPrefix(n.ID, "dep::")
		if mp == "" || strings.Contains(mp, "::") {
			continue
		}
		by[n.RepoPrefix] = append(by[n.RepoPrefix], depModuleEntry{
			modulePath: mp,
			node:       n,
		})
	}
	for k := range by {
		entries := by[k]
		sort.Slice(entries, func(i, j int) bool {
			return len(entries[i].modulePath) > len(entries[j].modulePath)
		})
	}
	cr.depModuleIndex = by
}

func (cr *CrossRepoResolver) clearDepModuleIndex() {
	cr.depModuleIndex = nil
}

// lookupDepModule returns the dep::<module> contract node whose
// module path is a prefix of importPath, scoped to callerRepo.
func (cr *CrossRepoResolver) lookupDepModule(callerRepo, importPath string) *graph.Node {
	for _, entry := range cr.depModuleIndex[callerRepo] {
		if importPath == entry.modulePath || strings.HasPrefix(importPath, entry.modulePath+"/") {
			return entry.node
		}
	}
	return nil
}

func (cr *CrossRepoResolver) clearDirIndexes() {
	cr.dirIndex = nil
	cr.lastDirIndex = nil
}

// buildReachableReposIndex walks every resolved EdgeImports edge and
// records, per caller file, the set of repo prefixes that file imports.
// This is the positive evidence the cross-repo name-only fallbacks
// consult: a candidate in repo R is eligible for caller file F only
// when F imports R. Per-repo resolution (resolver.go) runs first and
// resolves imports — including cross-repo imports, with a precise
// import-path match — so by the time this index is built the import
// graph is settled enough to be trustworthy evidence.
func (cr *CrossRepoResolver) buildReachableReposIndex() {
	idx := make(map[string]map[string]struct{})
	// Materialise the import edges and batch-load their targets in one
	// GetNodesByIDs — a per-edge GetNode(e.To) here is a query round-trip
	// per import on a disk backend, which under the cross-repo pass's
	// import population was a multi-minute cold-warmup stall (it runs
	// before the pass even logs "pass start").
	var imports []*graph.Edge
	ids := make(map[string]struct{})
	for e := range cr.graph.EdgesByKind(graph.EdgeImports) {
		imports = append(imports, e)
		if e.To != "" {
			ids[e.To] = struct{}{}
		}
	}
	if len(imports) == 0 {
		cr.reachableReposByFile = idx
		return
	}
	idList := make([]string, 0, len(ids))
	for id := range ids {
		idList = append(idList, id)
	}
	nodes := cr.graph.GetNodesByIDs(idList)
	for _, e := range imports {
		// Only resolved imports carry evidence — an unresolved import
		// target tells us nothing about which repo the caller reaches.
		to := nodes[e.To]
		if to == nil || to.RepoPrefix == "" {
			continue
		}
		set := idx[e.From]
		if set == nil {
			set = make(map[string]struct{})
			idx[e.From] = set
		}
		set[to.RepoPrefix] = struct{}{}
	}
	cr.reachableReposByFile = idx
}

func (cr *CrossRepoResolver) clearReachableReposIndex() {
	cr.reachableReposByFile = nil
}

// reachabilityChecker returns a per-edge closure that reports whether the
// caller of e may reach a candidate in targetRepo. It captures the caller's
// repo + import-reachability set ONCE; the per-call repoReachable re-derived
// both via cachedGetNode on every candidate, so a common cross-repo name
// with thousands of candidates paid O(candidates) redundant cache lookups
// per edge — the bulk of cr's compute wall time. Same semantics as
// repoReachable; only the per-candidate cost differs.
func (cr *CrossRepoResolver) reachabilityChecker(e *graph.Edge) func(targetRepo string) bool {
	callerRepo := cr.callerRepoPrefix(e)
	reachableRepos := cr.reachableReposByFile[cr.callerFileID(e)]
	return func(targetRepo string) bool {
		if targetRepo == "" || targetRepo == callerRepo {
			return true
		}
		if reachableRepos == nil {
			return false
		}
		_, ok := reachableRepos[targetRepo]
		return ok
	}
}

// callerFileID returns the graph ID of the file that owns the edge's
// From symbol. File node IDs equal their path, and EdgeImports edges
// are keyed From=fileID, so this is the lookup key for
// reachableReposByFile. Falls back to the edge's own FilePath when the
// From node can't be resolved.
func (cr *CrossRepoResolver) callerFileID(e *graph.Edge) string {
	if from := cr.cachedGetNode(e.From); from != nil {
		if from.Kind == graph.KindFile {
			return from.ID
		}
		if from.FilePath != "" {
			return from.FilePath
		}
	}
	return e.FilePath
}

// resolveEdge dispatches one unresolved edge through the cross-repo
// resolution paths and, when the resolution lifted the To target,
// appends a re-bind job to batch instead of committing a per-edge
// ReindexEdge transaction. The caller flushes the accumulated batch
// after the whole pass via ReindexEdges so disk backends amortise
// the commit cost.
// warmLookupCache batches the per-edge GetNode / FindNodesByName the
// cross-repo worker loop would otherwise fire serially — the mirror of
// Resolver.warmLookupCache (resolver.go). It includes the authoritative
// negative: a queried name with no node records an empty result, so the
// 200k+ external-call stubs return from the cache instead of each
// scanning the unindexed name column (the warmup hang).
func (cr *CrossRepoResolver) warmLookupCache(pending []*graph.Edge) {
	if len(pending) == 0 {
		return
	}
	idSet := make(map[string]struct{}, len(pending))
	nameSet := make(map[string]struct{}, len(pending))
	qualNameSet := make(map[string]struct{})
	for _, e := range pending {
		if e == nil {
			continue
		}
		if e.From != "" {
			idSet[e.From] = struct{}{}
		}
		bare := graph.UnresolvedName(e.To)
		if name := identifierFromTarget(bare); name != "" {
			nameSet[name] = struct{}{}
		}
		// Seed the RAW unresolved name too. This is pure scan-avoidance and
		// changes no resolution outcome: the legit cross-repo matches use the
		// bare identifier (seeded above) and resolve fine. The problem is the
		// EXTERNAL / unresolvable residual that dominates this pass (stdlib +
		// out-of-tree "calls" that never match a node): resolveFunctionCall
		// looks them up by their full target (e.g. "extern::pkg::Foo"), which
		// the stripped pre-warm key ("Foo") didn't cover, so they missed the
		// cache and fell through to a per-edge FindNodesByName scan — the
		// parallel cross-repo storm. Seeding the raw form lets them hit the
		// authoritative negative instead of scanning.
		if bare != "" {
			nameSet[bare] = struct{}{}
		}
		// Import targets: mirror resolveEdge's dispatch (TrimPrefix of the
		// bare unresolved:: form) so the seeded qual-name matches what
		// resolveImport looks up via GetNodeByQualName.
		if t := strings.TrimPrefix(e.To, unresolvedPrefix); strings.HasPrefix(t, "import::") {
			if qn := strings.TrimPrefix(t, "import::"); qn != "" {
				qualNameSet[qn] = struct{}{}
			}
		}
	}
	ids := make([]string, 0, len(idSet))
	for id := range idSet {
		ids = append(ids, id)
	}
	names := make([]string, 0, len(nameSet))
	for n := range nameSet {
		names = append(names, n)
	}
	cr.nodeByID = cr.graph.GetNodesByIDs(ids)
	cr.nodesByName = cr.graph.FindNodesByNames(names)
	// Authoritative negatives: record an empty result for every queried
	// name that has no node, so the cached lookup returns empty instead
	// of falling through to a per-edge FindNodesByName scan.
	if cr.nodesByName == nil {
		cr.nodesByName = make(map[string][]*graph.Node, len(nameSet))
	}
	for n := range nameSet {
		if _, ok := cr.nodesByName[n]; !ok {
			cr.nodesByName[n] = nil
		}
	}
	// Fold every candidate node into the id cache too, so a downstream
	// GetNode on a chosen target hits instead of going to the store.
	if cr.nodeByID == nil && len(cr.nodesByName) > 0 {
		cr.nodeByID = make(map[string]*graph.Node, len(cr.nodesByName))
	}
	for _, hits := range cr.nodesByName {
		for _, n := range hits {
			if n == nil || n.ID == "" {
				continue
			}
			if _, ok := cr.nodeByID[n.ID]; !ok {
				cr.nodeByID[n.ID] = n
			}
		}
	}
	// Index the name hits by repo so resolveFunctionCall / resolveMethodCall
	// collect ONLY the caller's reachable-repo, same-language candidates
	// instead of fetching every same-named node across all repos + languages
	// and discarding the unreachable majority per edge (the cross-repo
	// candidate-iteration cost). Every pre-warmed name gets an entry (empty
	// for an authoritative negative) so scopedCandidates can distinguish
	// "pre-warmed, no node" (return empty) from "not pre-warmed" (fall
	// through to the flat cache).
	cr.nodesByNameRepo = make(map[string]map[string][]*graph.Node, len(cr.nodesByName))
	for name, hits := range cr.nodesByName {
		byRepo := make(map[string][]*graph.Node)
		for _, n := range hits {
			if n == nil {
				continue
			}
			byRepo[n.RepoPrefix] = append(byRepo[n.RepoPrefix], n)
		}
		cr.nodesByNameRepo[name] = byRepo
	}
	// Pre-warm the import qual-name cache + authoritative negatives, so
	// resolveImport's GetNodeByQualName hits instead of scanning the
	// unindexed qual_name column per cross-repo import edge.
	if len(qualNameSet) > 0 {
		qns := make([]string, 0, len(qualNameSet))
		for q := range qualNameSet {
			qns = append(qns, q)
		}
		cr.nodesByQualName = cr.graph.GetNodesByQualNames(qns)
		if cr.nodesByQualName == nil {
			cr.nodesByQualName = make(map[string]*graph.Node, len(qualNameSet))
		}
		for q := range qualNameSet {
			if _, ok := cr.nodesByQualName[q]; !ok {
				cr.nodesByQualName[q] = nil
			}
		}
	}
}

func (cr *CrossRepoResolver) clearLookupCache() {
	cr.nodeByID = nil
	cr.nodesByName = nil
	cr.nodesByNameRepo = nil
	cr.nodesByQualName = nil
}

// scopedCandidates returns the candidates named `name` the caller of e could
// plausibly resolve to: nodes in the caller's own repo, a repo its file
// imports (reachableReposByFile), or no repo (synthetic) — AND of the
// caller's language (a Go call can't bind a same-named TypeScript symbol).
// This applies the import + language prune at the SOURCE: cachedFindNodesByName
// returns every same-named node across all repos and languages (thousands for
// a common name), which the per-edge loops then iterate and discard; the
// per-pass name→repo index collects only the relevant few. Names absent from
// the index (not pre-warmed) fall through to the flat cache, preserving the
// negative-cache + correctness contract.
func (cr *CrossRepoResolver) scopedCandidates(e *graph.Edge, name string) []*graph.Node {
	byRepo, ok := cr.nodesByNameRepo[name]
	if !ok {
		return cr.cachedFindNodesByName(name)
	}
	if len(byRepo) == 0 {
		return nil // pre-warmed, no node (authoritative negative)
	}
	caller := cr.cachedGetNode(e.From)
	callerRepo, callerLang, callerFile := "", "", e.FilePath
	if caller != nil {
		callerRepo = caller.RepoPrefix
		callerLang = caller.Language
		if caller.Kind == graph.KindFile {
			callerFile = caller.ID
		} else if caller.FilePath != "" {
			callerFile = caller.FilePath
		}
	}
	reachableRepos := cr.reachableReposByFile[callerFile]
	var out []*graph.Node
	keep := func(repo string) {
		for _, n := range byRepo[repo] {
			if callerLang == "" || n.Language == "" || n.Language == callerLang {
				out = append(out, n)
			}
		}
	}
	keep(callerRepo)
	if callerRepo != "" {
		keep("") // synthetic / no-repo nodes are always reachable
	}
	for r := range reachableRepos {
		if r != callerRepo && r != "" {
			keep(r)
		}
	}
	return out
}

// cachedGetNode consults the per-pass id cache first, falling through to
// the store on a miss (positive-only: absence means "not pre-warmed").
func (cr *CrossRepoResolver) cachedGetNode(id string) *graph.Node {
	if id == "" {
		return nil
	}
	if cr.nodeByID != nil {
		if n, ok := cr.nodeByID[id]; ok {
			return n
		}
	}
	return cr.graph.GetNode(id)
}

// cachedFindNodesByName consults the per-pass name cache first. A
// pre-warmed name with no node returns empty (authoritative negative);
// a name absent from the cache falls through to the store.
func (cr *CrossRepoResolver) cachedFindNodesByName(name string) []*graph.Node {
	if name == "" {
		return nil
	}
	if cr.nodesByName != nil {
		if hits, ok := cr.nodesByName[name]; ok {
			return hits
		}
	}
	return cr.graph.FindNodesByName(name)
}

// cachedGetNodeByQualName serves resolveImport's qual-name lookup from the
// per-pass cache (authoritative negative for queried-but-absent import
// paths), mirroring Resolver.cachedGetNodeByQualName.
func (cr *CrossRepoResolver) cachedGetNodeByQualName(qualName string) *graph.Node {
	if qualName == "" {
		return nil
	}
	if cr.nodesByQualName != nil {
		if n, ok := cr.nodesByQualName[qualName]; ok {
			return n
		}
	}
	return cr.graph.GetNodeByQualName(qualName)
}

func (cr *CrossRepoResolver) resolveEdge(e *graph.Edge, stats *CrossRepoStats, batch *[]graph.EdgeReindex) {
	oldTo := e.To
	// UnresolvedName handles BOTH the bare `unresolved::X` and the
	// multi-repo `<repo>::unresolved::X` forms; a plain TrimPrefix only
	// strips the bare form, leaving prefixed stubs (which fix-1's widened
	// EdgesWithUnresolvedTarget now feeds this pass) with target=full-id —
	// so the lookup key matched no node and missed the per-pass name cache,
	// turning every prefixed stub into a futile per-edge FindNodesByName
	// scan. Mirrors the master Resolver.resolveEdge.
	target := graph.UnresolvedName(e.To)
	if target == "" {
		target = strings.TrimPrefix(e.To, unresolvedPrefix)
	}

	switch {
	case strings.HasPrefix(target, "import::"):
		cr.resolveImport(e, strings.TrimPrefix(target, "import::"), stats)
	case strings.HasPrefix(target, "*."):
		cr.resolveMethodCall(e, strings.TrimPrefix(target, "*."), stats)
	case e.Kind == graph.EdgeExtends || e.Kind == graph.EdgeImplements || e.Kind == graph.EdgeComposes:
		// Type-hierarchy edges never resolve to a function/method.
		// CrossRepoResolver has no type-only resolution path, and a
		// cross-repo supertype requires the child's file to import the
		// parent's repo — which would have let per-repo resolution
		// (or a precise import) land it already. Leave it unresolved
		// rather than let resolveFunctionCall match a coincidental
		// cross-repo function of the same name.
		stats.Unresolved++
	default:
		cr.resolveFunctionCall(e, target, stats)
	}

	if e.To != oldTo {
		*batch = append(*batch, graph.EdgeReindex{Edge: e, OldTo: oldTo})
	}
}

// callerRepoPrefix returns the RepoPrefix of the node that owns the edge's From field.
func (cr *CrossRepoResolver) callerRepoPrefix(e *graph.Edge) string {
	fromNode := cr.cachedGetNode(e.From)
	if fromNode != nil {
		return fromNode.RepoPrefix
	}
	return ""
}

func (cr *CrossRepoResolver) resolveFunctionCall(e *graph.Edge, funcName string, stats *CrossRepoStats) {
	candidates := cr.scopedCandidates(e, funcName)
	if len(candidates) == 0 {
		stats.Unresolved++
		return
	}

	callerRepo := cr.callerRepoPrefix(e)
	callerWS := cr.callerWorkspaceID(e)
	reachable := cr.reachabilityChecker(e)

	// 1. Prefer same-repo match.
	for _, c := range candidates {
		if (c.Kind == graph.KindFunction || c.Kind == graph.KindMethod) &&
			c.RepoPrefix == callerRepo {
			e.To = c.ID
			stats.Resolved++
			return
		}
	}

	// 2. Cross-repo fallback: first function/method match that clears
	// BOTH evidence gates —
	//   (a) import-reachability: the caller's file must actually import
	//       the candidate's repo. Without this, a bare name like `len`
	//       or `String` resolves to whichever repo sorts first.
	//   (b) workspace boundary: same-workspace cross-repo is allowed;
	//       cross-workspace requires a declared cross_workspace_deps
	//       entry covering the workspace pair.
	for _, c := range candidates {
		if c.Kind != graph.KindFunction && c.Kind != graph.KindMethod {
			continue
		}
		if !reachable(c.RepoPrefix) {
			continue
		}
		if !cr.crossWorkspaceEligible(callerWS, candidateWorkspaceID(c), "") {
			continue
		}
		e.To = c.ID
		e.CrossRepo = true
		stats.Resolved++
		stats.CrossRepoEdges++
		stats.ByRepo[c.RepoPrefix]++
		return
	}

	stats.Unresolved++
}

func (cr *CrossRepoResolver) resolveImport(e *graph.Edge, importPath string, stats *CrossRepoStats) {
	callerRepo := cr.callerRepoPrefix(e)
	callerWS := cr.callerWorkspaceID(e)

	// npm-alias rewrite: see Resolver.resolveImport. Applied here too
	// so a JS/TS import of an alias key resolves cross-repo to a
	// locally-vendored real package when the per-repo pass left it
	// unresolved.
	importPath, npmAliased := rewriteNpmAliasImport(cr.npmAlias, e.FilePath, importPath)

	// Look for a package node with matching qualified name.
	if node := cr.cachedGetNodeByQualName(importPath); node != nil {
		picked := node
		if !cr.crossWorkspaceEligible(callerWS, candidateWorkspaceID(picked), importPath) {
			// The qual-name index is single-valued, so a same-module
			// instance in the caller's own workspace (a worktree of the
			// imported module, tracked under its own prefix) can be
			// shadowed by a copy in another workspace. Enumerate every
			// node sharing this qual name and prefer the caller's
			// workspace before giving up.
			picked = cr.pickQualNameCandidate(callerWS, importPath, node)
		}
		if picked != nil {
			e.To = picked.ID
			if picked.RepoPrefix != callerRepo {
				e.CrossRepo = true
				stats.CrossRepoEdges++
				stats.ByRepo[picked.RepoPrefix]++
			}
			stats.Resolved++
			return
		}
		// A qual-name hit with no workspace-eligible instance falls
		// through to the directory-match scan below rather than bailing
		// straight to external.
	}

	// Look for file nodes whose directory matches the import path. Two
	// inverted indexes (built once per Resolve* pass) replace what used
	// to be an O(N) scan of the entire graph per import edge.
	//
	// 1. Exact dir match — `dirIndex[importPath]` covers the case where
	//    the import literally equals a known directory.
	// 2. Last-component match — `lastDirIndex[lastPathComponent(...)]`
	//    covers the common case where an import path is just a name
	//    (e.g. "logger") and any file under .../logger/ is a candidate.
	//
	// Falls back to a full graph scan if the indexes are unset (defensive
	// — only happens when resolveImport is called outside a Resolve* pass).
	// When a package-manager workspace lookup is installed every
	// same-repo candidate is collected so a same-named collision
	// across two workspace members can be resolved to the importer's
	// own workspace; otherwise the first same-repo hit short-circuits
	// the scan as before.
	collectAll := cr.workspaceMembers != nil
	var sameRepo *graph.Node
	var sameRepoAll, crossRepoAll []*graph.Node
	consider := func(n *graph.Node) {
		if n.Kind != graph.KindFile {
			return
		}
		if n.RepoPrefix == callerRepo {
			if sameRepo == nil {
				sameRepo = n
			}
			if collectAll {
				sameRepoAll = append(sameRepoAll, n)
			}
			return
		}
		// Cross-repo file candidate: require a precise import-path
		// suffix match. lastDirIndex / the full-scan fallback key on the
		// last path component only, so without this gate an import of
		// `.../tree-sitter-c/bindings/go` resolves to whichever
		// `*/bindings/go` directory sorts first. Collect every match so
		// the workspace-aware pick below can prefer the importer's own
		// workspace instead of the first one encountered.
		if dirMatchesImport(filepath.Dir(n.FilePath), importPath) {
			crossRepoAll = append(crossRepoAll, n)
		}
	}
	stop := func() bool { return sameRepo != nil && !collectAll }
	if cr.dirIndex != nil {
		for _, n := range cr.dirIndex[importPath] {
			consider(n)
			if stop() {
				break
			}
		}
		if sameRepo == nil || collectAll {
			for _, n := range cr.lastDirIndex[lastPathComponent(importPath)] {
				consider(n)
				if stop() {
					break
				}
			}
		}
	} else {
		for n := range cr.graph.NodesByKind(graph.KindFile) {
			dir := filepath.Dir(n.FilePath)
			if strings.HasSuffix(dir, lastPathComponent(importPath)) || dir == importPath {
				consider(n)
				if stop() {
					break
				}
			}
		}
	}

	if sameRepo != nil {
		// Name-collision tie-break: prefer the same-repo file in the
		// importing file's own package-manager workspace.
		if ws := cr.preferSameWorkspaceFile(e.FilePath, sameRepoAll); ws != nil {
			sameRepo = ws
		}
		e.To = sameRepo.ID
		stats.Resolved++
		return
	}
	// Cross-repo directory match: prefer a candidate in the caller's own
	// workspace, then any the cross-workspace policy permits. Never bail
	// on the first ineligible candidate — a same-workspace instance (a
	// worktree of the imported module tracked under its own prefix) may
	// appear later in the list.
	if picked := cr.pickImportCandidate(callerWS, importPath, crossRepoAll); picked != nil {
		e.To = picked.ID
		e.CrossRepo = true
		stats.Resolved++
		stats.CrossRepoEdges++
		stats.ByRepo[picked.RepoPrefix]++
		return
	}

	// No file node matched. Try the dep::<module> contract from the
	// caller's go.mod before giving up. The dep node lives in the
	// caller's own repo, so this is a same-repo edge.
	if depNode := cr.lookupDepModule(callerRepo, importPath); depNode != nil {
		e.To = depNode.ID
		stats.Resolved++
		return
	}

	// npm-alias sub-path: a rewritten import like `@acme/shared-lib/util`
	// addresses a path inside the real package — fall back to the
	// package node itself. See Resolver.resolveImport.
	if npmAliased {
		if pkg := npmPackagePrefix(importPath); pkg != "" {
			if node := cr.cachedGetNodeByQualName(pkg); node != nil &&
				cr.crossWorkspaceEligible(callerWS, candidateWorkspaceID(node), pkg) {
				e.To = node.ID
				if node.RepoPrefix != callerRepo {
					e.CrossRepo = true
					stats.CrossRepoEdges++
					stats.ByRepo[node.RepoPrefix]++
				}
				stats.Resolved++
				return
			}
		}
	}

	// External/unresolvable import.
	e.To = "external::" + importPath
	stats.Unresolved++
}

func (cr *CrossRepoResolver) resolveMethodCall(e *graph.Edge, methodName string, stats *CrossRepoStats) {
	candidates := cr.scopedCandidates(e, methodName)
	if len(candidates) == 0 {
		stats.Unresolved++
		return
	}

	callerRepo := cr.callerRepoPrefix(e)
	callerWS := cr.callerWorkspaceID(e)
	receiverType := edgeReceiverType(e)
	reachable := cr.reachabilityChecker(e)

	// If we have a type hint, try exact type match first.
	if receiverType != "" {
		// Same-repo + exact type.
		for _, c := range candidates {
			if c.Kind == graph.KindMethod &&
				c.RepoPrefix == callerRepo &&
				nodeReceiverType(c) == receiverType {
				e.To = c.ID
				e.Confidence = 0.95
				stats.Resolved++
				return
			}
		}
		// Cross-repo + exact type — bounded by the import-reachability
		// and workspace evidence gates.
		for _, c := range candidates {
			if c.Kind != graph.KindMethod || nodeReceiverType(c) != receiverType {
				continue
			}
			if !reachable(c.RepoPrefix) {
				continue
			}
			if !cr.crossWorkspaceEligible(callerWS, candidateWorkspaceID(c), "") {
				continue
			}
			e.To = c.ID
			e.CrossRepo = true
			e.Confidence = 0.85
			stats.Resolved++
			stats.CrossRepoEdges++
			stats.ByRepo[c.RepoPrefix]++
			return
		}
	}

	// Fallback: name-only matching (methods first, then functions for pkg.Func() calls).
	for _, c := range candidates {
		if c.Kind == graph.KindMethod && c.RepoPrefix == callerRepo {
			e.To = c.ID
			stats.Resolved++
			return
		}
	}
	for _, c := range candidates {
		if c.Kind != graph.KindMethod {
			continue
		}
		if !reachable(c.RepoPrefix) {
			continue
		}
		if !cr.crossWorkspaceEligible(callerWS, candidateWorkspaceID(c), "") {
			continue
		}
		e.To = c.ID
		e.CrossRepo = true
		stats.Resolved++
		stats.CrossRepoEdges++
		stats.ByRepo[c.RepoPrefix]++
		return
	}
	for _, c := range candidates {
		if c.Kind == graph.KindFunction && c.RepoPrefix == callerRepo {
			e.To = c.ID
			stats.Resolved++
			return
		}
	}
	for _, c := range candidates {
		if c.Kind != graph.KindFunction {
			continue
		}
		if !reachable(c.RepoPrefix) {
			continue
		}
		if !cr.crossWorkspaceEligible(callerWS, candidateWorkspaceID(c), "") {
			continue
		}
		e.To = c.ID
		e.CrossRepo = true
		stats.Resolved++
		stats.CrossRepoEdges++
		stats.ByRepo[c.RepoPrefix]++
		return
	}

	stats.Unresolved++
}
