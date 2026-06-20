package mcp

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/persistence"
)

// rationaleVirtualFile is the synthetic FilePath every projected KindRationale
// node carries. It owns the projection: a reconcile evicts this "file" and
// re-adds the fresh set, so the derived view never drifts from the memory
// sidecar (the system of record). No real source file shares this path, so the
// rebuild touches nothing else.
const rationaleVirtualFile = ".gortex/rationale"

// rationaleBodyCap bounds the body text carried on a rationale node.
const rationaleBodyCap = 2000

// rationaleKinds are the memory kinds worth projecting as first-class "why"
// nodes — the load-bearing, decision-shaped knowledge a future agent should
// find one hop from the code it explains.
var rationaleKinds = map[string]bool{
	"decision": true, "incident": true, "constraint": true, "invariant": true,
}

// eligibleForRationale reports whether a memory entry should be projected into
// the graph: a non-superseded decision / incident / constraint / invariant that
// is load-bearing (pinned or importance >= 3) and has at least one anchor to
// motivate.
func eligibleForRationale(e persistence.MemoryEntry) bool {
	if e.SupersededBy != "" {
		return false
	}
	if !rationaleKinds[strings.ToLower(strings.TrimSpace(e.Kind))] {
		return false
	}
	if !e.Pinned && e.Importance < 3 {
		return false
	}
	return len(e.SymbolIDs) > 0 || len(e.FilePaths) > 0
}

// projectMemories builds the KindRationale nodes and EdgeMotivates edges for
// every eligible memory — a derived view of the memory sidecar so a why-query
// is one graph hop from the code a decision motivated. Node IDs are
// deterministic ("rationale::<memory-id>") so re-projection is idempotent.
func projectMemories(entries []persistence.MemoryEntry) ([]*graph.Node, []*graph.Edge) {
	var nodes []*graph.Node
	var edges []*graph.Edge
	for _, e := range entries {
		if !eligibleForRationale(e) {
			continue
		}
		id := "rationale::" + e.ID
		name := strings.TrimSpace(e.Title)
		if name == "" {
			name = truncate(firstLine(e.Body), 80)
		}
		nodes = append(nodes, &graph.Node{
			ID: id, Kind: graph.KindRationale, Name: name,
			FilePath: rationaleVirtualFile, StartLine: 1,
			RepoPrefix: e.RepoPrefix,
			Meta: map[string]any{
				"rationale_kind": strings.ToLower(strings.TrimSpace(e.Kind)),
				"memory_id":      e.ID,
				"importance":     e.Importance,
				"confidence":     e.Confidence,
				"pinned":         e.Pinned,
				"section_text":   truncate(e.Body, rationaleBodyCap),
			},
		})
		seen := make(map[string]bool, len(e.SymbolIDs)+len(e.FilePaths))
		anchor := func(target string) {
			target = strings.TrimSpace(target)
			if target == "" || seen[target] {
				return
			}
			seen[target] = true
			edges = append(edges, &graph.Edge{
				From: id, To: target, Kind: graph.EdgeMotivates,
				FilePath: rationaleVirtualFile,
				Origin:   "memory_projected",
				Meta:     map[string]any{"signal": "memory_projected"},
			})
		}
		for _, sym := range e.SymbolIDs {
			anchor(sym)
		}
		for _, fp := range e.FilePaths {
			anchor(fp)
		}
	}
	return nodes, edges
}

// reconcileRationale rebuilds the rationale projection from the current memory
// sidecar contents for the given scope. Idempotent: it evicts the prior
// projection (the rationale virtual file) and installs the fresh set.
func (s *Server) reconcileRationale(scope string) {
	if s.graph == nil {
		return
	}
	var entries []persistence.MemoryEntry
	for _, mgr := range s.resolveMemoryStores(scope) {
		if mgr == nil {
			continue
		}
		entries = append(entries, mgr.allEntries()...)
	}
	nodes, edges := projectMemories(entries)
	s.graph.EvictFile(rationaleVirtualFile)
	if len(nodes) > 0 {
		s.graph.AddBatch(nodes, edges)
	}
}

// firstLine returns s up to its first newline.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// truncate caps s at n runes (byte-safe), appending nothing — the cap is a size
// guard, not a display ellipsis.
func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n]
}
