package lsp

import (
	"sort"

	"github.com/zzet/gortex/internal/graph"
)

// enrichSweepReserveFraction is the slice of a repo's enrichment deadline held
// back from the targeted-edge passes (implementations + reference confirm) for
// the post-confirm hover / hierarchy sweep. Reserving it stops a round-trip-
// bound confirm pass from consuming the whole window and starving the add
// phase (edges_added stuck at 0). 0.4 keeps confirm-first — the majority of the
// window still confirms tiers — while guaranteeing the sweep runs.
const enrichSweepReserveFraction = 0.4

// enrichTarget pairs an ambiguous edge with its (repo+language) source node —
// the unit the reference-confirm pass promotes or refutes.
type enrichTarget struct {
	node *graph.Node
	edge *graph.Edge
}

// confirmGroup is the confirm pass's per-file work unit: every ambiguous
// target whose referent declaration lives in rel. Grouping lets one didOpen of
// rel serve all its targets' reference queries.
type confirmGroup struct {
	rel     string
	targets []enrichTarget
}

// groupConfirmTargets buckets confirm targets by the file of the referent
// declaration (the file findReferences is positioned in) and orders the groups
// highest-yield-first. The ordering is the value-prioritisation lever: when the
// deadline cuts the pass short, the files carrying the most ambiguous edges —
// where a confirmation resolves the most tiers — have already run. Ordering is
// deterministic (count desc, then path) so a resumed / replayed index is
// stable.
func (p *Provider) groupConfirmTargets(g graph.Store, targets []enrichTarget) []*confirmGroup {
	byFile := map[string]*confirmGroup{}
	var order []*confirmGroup
	for _, t := range targets {
		toNode := g.GetNode(t.edge.To)
		if toNode == nil {
			continue
		}
		if _, ok := lspLine(toNode); !ok {
			continue
		}
		rel := nodeRelPath(toNode)
		if rel == "" {
			continue
		}
		if !p.servesFile(rel) {
			continue // never open a referent file this server can't compile
		}
		grp := byFile[rel]
		if grp == nil {
			grp = &confirmGroup{rel: rel}
			byFile[rel] = grp
			order = append(order, grp)
		}
		grp.targets = append(grp.targets, t)
	}
	sort.SliceStable(order, func(i, j int) bool {
		if len(order[i].targets) != len(order[j].targets) {
			return len(order[i].targets) > len(order[j].targets)
		}
		return order[i].rel < order[j].rel
	})
	return order
}

// confirmRefMatchesSite reports whether the server's reference list ties the
// edge's own call site to its target declaration — the identity anchor that
// promotes an ambiguous edge to the lsp tier. A recorded site line is matched
// exactly (±1 for wrapped call expressions); an edge without one falls back to
// containment in the caller's span. Pure over its inputs, so it is safe to call
// from the parallel sweep.
func (p *Provider) confirmRefMatchesSite(refs []Location, absRoot, repoPrefix string, t enrichTarget) bool {
	callerRel := nodeRelPath(t.node)
	siteRel := edgeSiteRelPath(t.edge, repoPrefix, callerRel)
	siteLine := t.edge.Line
	for _, ref := range refs {
		// uriToPath returns a repo-relative path while node/edge FilePaths are
		// prefixed, so compare against stripped paths.
		refPath := uriToPath(ref.URI, absRoot)
		refLine := ref.Range.Start.Line + 1
		if siteLine > 0 {
			if refPath == siteRel && refLine >= siteLine-1 && refLine <= siteLine+1 {
				return true
			}
			continue
		}
		if refPath == callerRel && refLine >= t.node.StartLine && refLine <= t.node.EndLine {
			return true
		}
	}
	return false
}
