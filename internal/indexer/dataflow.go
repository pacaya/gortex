package indexer

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// materializeDataflowParams runs after the regular call resolver
// pass to lift the placeholder targets carried by EdgeArgOf and
// EdgeReturnsTo edges to concrete graph IDs. The Go dataflow
// extractor (see internal/parser/languages/go_dataflow.go) emits
// these edges with an `unresolved::` text on the side that
// references the callee — exactly the shape the call resolver
// already knows how to lift. After Resolver.ResolveAll has run
// every placeholder side has been rewritten to a real function /
// method node ID; this pass then:
//
//  1. EdgeArgOf — joins the now-resolved To (a function/method
//     node) against its incoming EdgeParamOf edges to find the
//     param node at the recorded position (Meta["arg_position"]),
//     and rewrites the edge target to the param node ID. When no
//     matching param exists (variadic position past the declared
//     count, signature mismatch from extern callees, etc.) the
//     edge stays pointed at the function node — still a useful
//     dataflow hop.
//
//  2. EdgeReturnsTo — joins the placeholder From (currently the
//     enclosing caller's function ID) against the resolved
//     EdgeCalls edge from the same caller at the same line,
//     and rewrites From to the resolved callee. Falls back to
//     leaving the placeholder in place when no matching call
//     edge can be found (rare; usually means the call resolver
//     declined to lift the call edge too).
//
// Both rewrite paths use graph.RemoveEdge + graph.AddEdge so the
// shard buckets / inverted indexes stay consistent with the new
// (From, To, Kind, Line) tuple. Edges whose Meta no longer
// matches their state are stripped of the dataflow markers so a
// re-run of this pass becomes a no-op.
func (idx *Indexer) materializeDataflowParams() {
	g := idx.graph
	// Only arg_of / returns_to edges are rewritten here. Fetch exactly
	// those kinds — each an edges_by_kind index probe on the sqlite
	// backend — instead of scanning (and meta-decoding) the whole edge
	// set; every other edge in the graph is irrelevant to this pass.
	for e := range g.EdgesByKind(graph.EdgeArgOf) {
		rewriteArgOf(g, e)
	}
	for e := range g.EdgesByKind(graph.EdgeReturnsTo) {
		rewriteReturnsTo(g, e)
	}
}

// materializeDataflowParamsForFile is the single-file equivalent of
// materializeDataflowParams, used on the incremental (fsnotify /
// edit_file) re-index path so a one-line edit doesn't scan the whole
// edge set. fileEdges is the file's freshly-extracted edge slice
// (result.Edges from indexFile); only its From endpoints are read, so
// stale To/From values from before resolution don't matter.
//
// A file's arg_of / returns_to From is NOT always a node in the file,
// so node membership alone is insufficient. Two From classes exist:
//   - file nodes: returns_to's From is the caller function, and an
//     arg_of whose argument is a bare in-scope identifier has its From
//     rewritten by the resolver to that local/param — GetFileNodes
//     covers both.
//   - synthetic ids: arg_of for a selector (obj.Field), package-
//     qualified (pkg.V), global, or nested-call (f(g())) argument keeps
//     a synthetic `unresolved::` / `external::` From that never becomes
//     a file node. The resolver leaves these untouched, so the id the
//     extractor emitted (still present in fileEdges) is the id in the
//     graph.
//
// Probing the union of both, then keeping only edges whose FilePath is
// this file, yields exactly the arg_of+returns_to set the whole-graph
// pass would touch for it — faithful, not approximate. Each rewrite
// needs only the edge plus a targeted callee lookup (paramNodeAtPosition
// / findCallTarget). The batch path (Resolver.ResolveAll) still runs the
// whole-graph variant once, where amortising one scan over many files
// is the right trade.
func (idx *Indexer) materializeDataflowParamsForFile(graphPath string, fileEdges []*graph.Edge) {
	g := idx.graph
	fromSet := make(map[string]struct{})
	for _, n := range g.GetFileNodes(graphPath) {
		if n != nil && n.ID != "" {
			fromSet[n.ID] = struct{}{}
		}
	}
	for _, e := range fileEdges {
		if e != nil && (e.Kind == graph.EdgeArgOf || e.Kind == graph.EdgeReturnsTo) && e.From != "" {
			fromSet[e.From] = struct{}{}
		}
	}
	if len(fromSet) == 0 {
		return
	}
	froms := make([]string, 0, len(fromSet))
	for id := range fromSet {
		froms = append(froms, id)
	}
	// A synthetic From can be shared across files, so restrict the rewrite
	// to edges this file actually emitted: every arg_of / returns_to edge
	// carries its call-site FilePath, so the filter keeps the set exactly
	// the file's own. Collect the file's arg_of / returns_to edges plus the
	// distinct callees the arg_of rewrites target, so the per-callee param
	// lookup is batched once instead of re-fetched per argument.
	var argEdges, retEdges []*graph.Edge
	callees := make(map[string]struct{})
	for _, edges := range g.GetOutEdgesByNodeIDs(froms) {
		for _, e := range edges {
			if e == nil || e.FilePath != graphPath {
				continue
			}
			switch e.Kind {
			case graph.EdgeArgOf:
				argEdges = append(argEdges, e)
				if callee, _, ok := argOfRewriteTarget(e); ok {
					callees[callee] = struct{}{}
				}
			case graph.EdgeReturnsTo:
				retEdges = append(retEdges, e)
			}
		}
	}
	paramIdx := buildParamPositionIndex(g, callees)
	for _, e := range argEdges {
		rewriteArgOfIndexed(g, e, paramIdx)
	}
	for _, e := range retEdges {
		rewriteReturnsTo(g, e)
	}
}

// argOfRewriteTarget reports whether an arg_of edge is a rewrite
// candidate and, if so, the resolved callee id and the argument
// position. An edge already pointing at a param node, or still
// pointing at an unresolved / external stub, is not a candidate. Shared
// by the per-edge (rewriteArgOf) and indexed (rewriteArgOfIndexed) paths
// so the guard lives in one place.
func argOfRewriteTarget(e *graph.Edge) (calleeID string, pos int, ok bool) {
	if e == nil || e.Meta == nil {
		return "", 0, false
	}
	pos, ok = argPositionFromMeta(e.Meta)
	if !ok {
		return "", 0, false
	}
	to := e.To
	if strings.Contains(to, "#param:") {
		return "", 0, false
	}
	if strings.HasPrefix(to, "unresolved::") || strings.HasPrefix(to, "external::") {
		return "", 0, false
	}
	return to, pos, true
}

// rewriteArgOf walks the resolved callee's incoming param_of edges
// and lifts the edge target from the function node to the param
// node at the recorded position. Edges that already point at a
// param node are left alone. Used by the whole-graph (cold) pass; the
// per-file pass uses the batched rewriteArgOfIndexed instead.
func rewriteArgOf(g graph.Store, e *graph.Edge) {
	calleeID, pos, ok := argOfRewriteTarget(e)
	if !ok {
		return
	}
	paramID := paramNodeAtPosition(g, calleeID, pos)
	if paramID == "" {
		return
	}
	oldTo := e.To
	g.RemoveEdge(e.From, oldTo, e.Kind)
	e.To = paramID
	g.AddEdge(e)
}

// rewriteArgOfIndexed is rewriteArgOf with the callee→position→param
// lookup served from a prebuilt index instead of a per-edge
// paramNodeAtPosition (which re-fetched the callee's entire in-edge list
// once per argument). Same rewrite, same guards.
func rewriteArgOfIndexed(g graph.Store, e *graph.Edge, paramIdx map[string]map[int]string) {
	calleeID, pos, ok := argOfRewriteTarget(e)
	if !ok {
		return
	}
	m := paramIdx[calleeID]
	if m == nil {
		return
	}
	paramID := m[pos]
	if paramID == "" {
		return
	}
	oldTo := e.To
	g.RemoveEdge(e.From, oldTo, e.Kind)
	e.To = paramID
	g.AddEdge(e)
}

// buildParamPositionIndex maps each callee id to its argument
// position → param-node-id table, built from two batched queries
// (in-edges of all callees, then the param nodes those edges point
// from). It replaces a per-arg_of-edge paramNodeAtPosition, which
// re-fetched a popular callee's whole in-edge list once per argument —
// the dominant cost of the per-file dataflow pass on a large file. The
// position is read from the param node's Meta exactly as
// paramNodeAtPosition does, with the first param at a position winning.
func buildParamPositionIndex(g graph.Store, callees map[string]struct{}) map[string]map[int]string {
	if len(callees) == 0 {
		return nil
	}
	ids := make([]string, 0, len(callees))
	for id := range callees {
		ids = append(ids, id)
	}
	inEdges := g.GetInEdgesByNodeIDs(ids)
	type ownerParam struct{ owner, param string }
	var pairs []ownerParam
	paramSet := make(map[string]struct{})
	for owner, edges := range inEdges {
		for _, e := range edges {
			if e != nil && e.Kind == graph.EdgeParamOf && e.From != "" {
				pairs = append(pairs, ownerParam{owner: owner, param: e.From})
				paramSet[e.From] = struct{}{}
			}
		}
	}
	if len(pairs) == 0 {
		return nil
	}
	paramIDs := make([]string, 0, len(paramSet))
	for id := range paramSet {
		paramIDs = append(paramIDs, id)
	}
	nodes := g.GetNodesByIDs(paramIDs)
	idx := make(map[string]map[int]string, len(inEdges))
	for _, pr := range pairs {
		n := nodes[pr.param]
		if n == nil || n.Kind != graph.KindParam {
			continue
		}
		pos, ok := intFromMeta(n.Meta, "position")
		if !ok {
			continue
		}
		m := idx[pr.owner]
		if m == nil {
			m = make(map[int]string)
			idx[pr.owner] = m
		}
		if _, exists := m[pos]; !exists {
			m[pos] = n.ID
		}
	}
	return idx
}

// rewriteReturnsTo lifts the placeholder From by joining on the
// resolved EdgeCalls edge from the same caller and line.
func rewriteReturnsTo(g graph.Store, e *graph.Edge) {
	if e == nil || e.Meta == nil {
		return
	}
	if _, ok := e.Meta["returns_to_call"]; !ok {
		return
	}
	callLine, _ := intFromMeta(e.Meta, "call_line")
	if callLine == 0 {
		callLine = e.Line
	}
	callerID := e.From
	calleeText, _ := e.Meta["callee_target"].(string)
	resolvedCallee := findCallTarget(g, callerID, callLine, calleeText)
	if resolvedCallee == "" {
		return
	}
	oldFrom := e.From
	g.RemoveEdge(oldFrom, e.To, e.Kind)
	e.From = resolvedCallee
	g.AddEdge(e)
}

// findCallTarget returns the resolved To of the EdgeCalls edge
// originating from callerID at the given line. When `calleeText`
// is non-empty it's used as a tie-breaker against the original
// unresolved target string so we don't lift to the wrong call when
// two calls live on the same line. Falls back to the first match
// otherwise.
// outEdgeLightStore is implemented by backends that can return a node's
// out-edges without decoding the per-edge Meta blob. findCallTarget reads
// only endpoints/kind/line, so it opts into the cheaper fetch when the
// backend offers it (the sqlite backend, where the Meta JSON-decode
// otherwise dominates this hot lookup); other stores fall back.
type outEdgeLightStore interface {
	GetOutEdgesLight(nodeID string) []*graph.Edge
}

func findCallTarget(g graph.Store, callerID string, line int, calleeText string) string {
	var out []*graph.Edge
	if ls, ok := g.(outEdgeLightStore); ok {
		out = ls.GetOutEdgesLight(callerID)
	} else {
		out = g.GetOutEdges(callerID)
	}
	var fallback string
	for _, e := range out {
		if e.Kind != graph.EdgeCalls {
			continue
		}
		if line != 0 && e.Line != line {
			continue
		}
		if strings.HasPrefix(e.To, "unresolved::") {
			continue
		}
		if calleeText != "" && callTargetMatches(e, calleeText) {
			return e.To
		}
		if fallback == "" {
			fallback = e.To
		}
	}
	return fallback
}

// callTargetMatches reports whether a resolved call edge's text
// shape lines up with the dataflow edge's recorded callee_target.
// We compare the trailing path component of the resolved To
// against the unresolved::… form used at extraction time. Used as
// a same-line tie-breaker when more than one call lives on a
// single source line (e.g. `f(g())`).
func callTargetMatches(call *graph.Edge, calleeText string) bool {
	if call == nil || calleeText == "" {
		return false
	}
	bare := strings.TrimPrefix(calleeText, "unresolved::")
	bare = strings.TrimPrefix(bare, "extern::")
	bare = strings.TrimPrefix(bare, "*.")
	if bare == "" {
		return false
	}
	to := call.To
	if i := strings.LastIndex(to, "::"); i >= 0 {
		to = to[i+2:]
	}
	if i := strings.LastIndex(to, "."); i >= 0 {
		to = to[i+1:]
	}
	return to == bare
}

// paramNodeAtPosition returns the param node ID with the recorded
// position attached to ownerID via EdgeParamOf.
func paramNodeAtPosition(g graph.Store, ownerID string, pos int) string {
	in := g.GetInEdges(ownerID)
	for _, e := range in {
		if e.Kind != graph.EdgeParamOf {
			continue
		}
		n := g.GetNode(e.From)
		if n == nil || n.Kind != graph.KindParam {
			continue
		}
		p, ok := intFromMeta(n.Meta, "position")
		if !ok {
			continue
		}
		if p == pos {
			return n.ID
		}
	}
	return ""
}

// argPositionFromMeta extracts the recorded argument position. The
// metadata roundtrip can yield int or float64 depending on origin
// (extractor vs JSON deserialisation), so accept both.
func argPositionFromMeta(m map[string]any) (int, bool) {
	return intFromMeta(m, "arg_position")
}

func intFromMeta(m map[string]any, key string) (int, bool) {
	if m == nil {
		return 0, false
	}
	v, ok := m[key]
	if !ok {
		return 0, false
	}
	switch x := v.(type) {
	case int:
		return x, true
	case int64:
		return int(x), true
	case float64:
		return int(x), true
	}
	return 0, false
}
