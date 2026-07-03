package resolver

import (
	"strconv"

	"github.com/zzet/gortex/internal/graph"
)

// Receiver-type gating for C# member-call attribution.
//
// The extractor stamps Meta["receiver_type"] on a member-call candidate when
// the local type environment knows the receiver. When such a call cannot bind
// to a member of that exact type (nor of a base/interface it derives from) and
// a weak resolver tier falls back to a same-named member on an *unrelated*
// type, the attribution is wrong: an edge that names its receiver type must not
// attach to a same-named member of an unrelated type. This pass demotes those
// edges to the speculative tier so they drop out of every default query and
// min_tier filter — while a genuine inherited / interface-dispatch call (where
// the target's receiver is a super-type of the receiver_type) and a valid
// extension-method binding are both preserved, so the gate adds no false
// negatives.
//
// The demotion re-writes the edge (remove + re-add as speculative) rather than
// mutating it in place: an in-place Origin/Meta change is a no-op on the
// non-memory backends, which return decoded copies from EdgesByKind. RemoveEdge
// is keyed only by (from,to,kind), so every edge in an affected group is
// snapshotted and re-added, preserving legitimately-typed sibling calls.

// demoteCSharpMisattributedMemberCalls demotes weak-tier C# member calls whose
// bound target belongs to a type unrelated to the edge's receiver_type. Returns
// the number of edges demoted.
func demoteCSharpMisattributedMemberCalls(g graph.Store) int {
	if g == nil {
		return 0
	}
	// C# type/interface name → node ids, and each type's super-type / interface
	// (up) edges, for hierarchy-relatedness checks by name.
	nameToTypeIDs := map[string][]string{}
	for _, n := range nodesByKindsOrAll(g, graph.KindType, graph.KindInterface) {
		if n == nil || n.Language != "csharp" || n.Name == "" {
			continue
		}
		nameToTypeIDs[n.Name] = append(nameToTypeIDs[n.Name], n.ID)
	}
	if len(nameToTypeIDs) == 0 {
		return 0
	}
	up := map[string][]string{}
	for _, k := range []graph.EdgeKind{graph.EdgeExtends, graph.EdgeImplements} {
		for e := range g.EdgesByKind(k) {
			if e == nil || e.From == "" || graph.IsUnresolvedTarget(e.To) {
				continue
			}
			up[e.From] = append(up[e.From], e.To)
		}
	}

	groupKey := func(from, to string, kind graph.EdgeKind) string {
		return from + "\x00" + to + "\x00" + string(kind)
	}
	edgeKey := func(e *graph.Edge) string {
		return e.From + "\x00" + e.To + "\x00" + string(e.Kind) + "\x00" + e.FilePath + "\x00" + strconv.Itoa(e.Line)
	}

	// Pass 1: which edges to demote, and which (from,to,kind) groups they touch.
	demote := map[string]bool{}
	affected := map[string]bool{}
	for e := range g.EdgesByKind(graph.EdgeCalls) {
		if !csharpShouldDemote(g, e, nameToTypeIDs, up) {
			continue
		}
		demote[edgeKey(e)] = true
		affected[groupKey(e.From, e.To, e.Kind)] = true
	}
	if len(affected) == 0 {
		return 0
	}

	// Pass 2: snapshot every edge in an affected group (siblings included) so
	// the coarse RemoveEdge can be reversed precisely.
	groups := map[string][]*graph.Edge{}
	for e := range g.EdgesByKind(graph.EdgeCalls) {
		gk := groupKey(e.From, e.To, e.Kind)
		if affected[gk] {
			groups[gk] = append(groups[gk], e)
		}
	}

	demoted := 0
	for _, edges := range groups {
		if len(edges) == 0 {
			continue
		}
		f := edges[0]
		g.RemoveEdge(f.From, f.To, f.Kind)
		for _, e := range edges {
			if demote[edgeKey(e)] {
				e.Origin = graph.OriginSpeculative
				if e.Meta == nil {
					e.Meta = map[string]any{}
				}
				e.Meta[graph.MetaSpeculative] = true
				e.Meta["demoted"] = "receiver_type_mismatch"
				demoted++
			}
			g.AddEdge(e)
		}
	}
	return demoted
}

// csharpShouldDemote reports whether a resolved C# member-call edge is a
// same-named-unrelated-type misattribution that should be demoted.
func csharpShouldDemote(g graph.Store, e *graph.Edge, nameToTypeIDs, up map[string][]string) bool {
	if e == nil || e.Meta == nil || e.IsSpeculative() || graph.IsUnresolvedTarget(e.To) {
		return false
	}
	rt, _ := e.Meta["receiver_type"].(string)
	if rt == "" {
		return false
	}
	// A valid extension binding names the extension's static host class as the
	// target receiver, which is by definition unrelated to the receiver it
	// extends — never demote those.
	if res, _ := e.Meta["resolution"].(string); res == "extension_method" {
		return false
	}
	// Only the weak tiers are gated; never demote ast_resolved / lsp evidence.
	// An empty Origin resolves to its confidence-derived tier.
	eff := e.Origin
	if eff == "" {
		eff = graph.DefaultOriginFor(e.Kind, e.Confidence, "")
	}
	if graph.OriginRank(eff) > graph.OriginRank(graph.OriginASTInferred) {
		return false
	}
	caller := g.GetNode(e.From)
	if caller == nil || caller.Language != "csharp" {
		return false
	}
	target := g.GetNode(e.To)
	if target == nil || target.Kind != graph.KindMethod || target.Language != "csharp" || target.Meta == nil {
		return false
	}
	// An extension target reached without the extension_method resolution tag
	// (e.g. a locality pick) is still a legitimate extension — keep it.
	if isCSharpExtension(target) {
		return false
	}
	tr, _ := target.Meta["receiver"].(string)
	if tr == "" || tr == rt {
		return false
	}
	// Only demote when both endpoints are known indexed types — otherwise we
	// cannot establish that the mismatch is a genuinely unrelated-type
	// misattribution, and keeping the edge avoids a false negative.
	if len(nameToTypeIDs[rt]) == 0 || len(nameToTypeIDs[tr]) == 0 {
		return false
	}
	// A related receiver (the target lives on a base type / interface the
	// receiver_type derives from) is a legitimate polymorphic call — keep.
	return !csharpTypesRelated(nameToTypeIDs, up, rt, tr)
}

// csharpTypesRelated reports whether type names a and b are related through the
// C# type hierarchy in either direction (one derives from / implements the
// other, transitively).
func csharpTypesRelated(nameToTypeIDs, up map[string][]string, a, b string) bool {
	if a == b {
		return true
	}
	return csharpNameReaches(nameToTypeIDs, up, a, b) || csharpNameReaches(nameToTypeIDs, up, b, a)
}

// csharpNameReaches reports whether any type named `from` reaches any type named
// `to` by following super-type / interface (up) edges transitively.
func csharpNameReaches(nameToTypeIDs, up map[string][]string, from, to string) bool {
	targets := map[string]bool{}
	for _, id := range nameToTypeIDs[to] {
		targets[id] = true
	}
	if len(targets) == 0 {
		return false
	}
	visited := map[string]bool{}
	queue := append([]string{}, nameToTypeIDs[from]...)
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if visited[cur] {
			continue
		}
		visited[cur] = true
		for _, p := range up[cur] {
			if targets[p] {
				return true
			}
			if !visited[p] {
				queue = append(queue, p)
			}
		}
	}
	return false
}
