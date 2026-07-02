package resolver

import "github.com/zzet/gortex/internal/graph"

// Member-level C# interface-dispatch synthesis.
//
// A through-interface call site — `x.Convert(1)` where `x` is typed as the
// interface — binds to the interface *member* declaration node
// (<file>::IConverter.Convert), which the extractor now emits. But the method
// a compiler would actually dispatch to at runtime lives on each concrete
// implementation of that interface. This pass mints one best-guess `calls`
// edge from the call site to the same-named member on every in-repo
// implementation, so a forward/backward call walk can reach the concrete
// bodies that a purely static binding leaves invisible.
//
// Only one implementation runs at any given site, so the fan-out is genuinely
// a guess: the edges ride at the speculative tier (OriginSpeculative +
// Meta[speculative]=true) — present-but-hidden-by-default, excluded by
// min_tier filtering, and auditable via `analyze kind=speculative`. Keeping
// them hidden is also what protects precision: a visible fan-out would attach
// every interface-dispatch call site to every implementation's member.

// csharpIfaceDispatchCap bounds the per-site fan-out. A widely-implemented
// interface (Humanizer's INumberToWordsConverter has ~40 language impls) must
// not mint an unbounded edge set from a single call site; above the cap the
// whole fan-out is dropped as noise. Mirrors speculativeHardCap.
const csharpIfaceDispatchCap = 40

// ResolveCSharpInterfaceDispatch fans calls bound to a C# interface member out
// to the concrete same-named member of each in-repo implementation. Returns
// the number of fan-out edges landed.
func ResolveCSharpInterfaceDispatch(g graph.Store) int {
	if g == nil {
		return 0
	}

	// interface type node id → its in-repo implementation type node ids.
	// EdgeImplements is From=type, To=interface; only resolved targets count.
	implsByIface := map[string][]string{}
	for e := range g.EdgesByKind(graph.EdgeImplements) {
		if e == nil || e.From == "" || graph.IsUnresolvedTarget(e.To) {
			continue
		}
		implsByIface[e.To] = append(implsByIface[e.To], e.From)
	}
	if len(implsByIface) == 0 {
		return 0
	}
	// implementation type node id → member name → concrete method node.
	membersByType := memberMethodNodesByType(g)

	// Existing resolved (From,To) call pairs, so a fan-out edge never
	// duplicates a real call (e.g. a caller that already reaches the concrete
	// member directly).
	existing := map[string]bool{}
	for e := range g.EdgesByKind(graph.EdgeCalls) {
		if e == nil || e.IsSpeculative() || graph.IsUnresolvedTarget(e.To) {
			continue
		}
		existing[e.From+"\x00"+e.To] = true
	}

	var batch []*graph.Edge
	seen := map[string]bool{}
	for e := range g.EdgesByKind(graph.EdgeCalls) {
		if e == nil || e.IsSpeculative() || graph.IsUnresolvedTarget(e.To) {
			continue
		}
		target := g.GetNode(e.To)
		if target == nil || target.Kind != graph.KindMethod || target.Language != "csharp" ||
			!csharpIsIfaceMember(target) {
			continue
		}
		ifaceTypeID := csharpMemberOwnerType(g, target.ID)
		if ifaceTypeID == "" {
			continue
		}
		impls := implsByIface[ifaceTypeID]
		if len(impls) == 0 || len(impls) > csharpIfaceDispatchCap {
			continue
		}
		method := target.Name
		for _, implTypeID := range impls {
			byName := membersByType[implTypeID]
			if byName == nil {
				continue
			}
			impl := byName[method]
			if impl == nil || impl.ID == target.ID {
				continue
			}
			// In-repo only: the implementation must live in the interface's
			// repo (cross-repo dispatch is CrossRepoResolver's domain).
			if impl.RepoPrefix != target.RepoPrefix {
				continue
			}
			if existing[e.From+"\x00"+impl.ID] {
				continue
			}
			k := e.From + "\x00" + impl.ID
			if seen[k] {
				continue
			}
			seen[k] = true
			batch = append(batch, csharpIfaceDispatchEdge(e, impl.ID, ifaceTypeID, len(impls)))
		}
	}
	for _, ne := range batch {
		g.AddEdge(ne)
	}
	return len(batch)
}

// csharpIsIfaceMember reports whether n is a bodyless (or default) interface
// member declaration emitted by the C# extractor.
func csharpIsIfaceMember(n *graph.Node) bool {
	if n == nil || n.Meta == nil {
		return false
	}
	v, _ := n.Meta["iface_member"].(bool)
	return v
}

// csharpMemberOwnerType returns the type/interface node id a member belongs to,
// via its EdgeMemberOf edge, or "" when unresolved.
func csharpMemberOwnerType(g graph.Store, memberID string) string {
	for _, oe := range g.GetOutEdges(memberID) {
		if oe.Kind == graph.EdgeMemberOf && !graph.IsUnresolvedTarget(oe.To) {
			return oe.To
		}
	}
	return ""
}

// csharpIfaceDispatchEdge builds one speculative fan-out call edge from the
// call site e to a concrete implementation member. Confidence is 1/N over the
// fan-out width, floored/capped like the speculative-dispatch pass.
func csharpIfaceDispatchEdge(e *graph.Edge, to, ifaceTypeID string, fanout int) *graph.Edge {
	conf := 1.0
	if fanout > 0 {
		conf = 1.0 / float64(fanout)
	}
	if conf > 0.45 {
		conf = 0.45
	}
	if conf < 0.05 {
		conf = 0.05
	}
	ne := &graph.Edge{
		From: e.From, To: to, Kind: graph.EdgeCalls,
		FilePath: e.FilePath, Line: e.Line,
		Origin:          graph.OriginSpeculative,
		Confidence:      conf,
		ConfidenceLabel: graph.ConfidenceLabelFor(graph.EdgeCalls, conf),
		Meta: map[string]any{
			graph.MetaSpeculative: true,
			"via":                 "csharp-iface-dispatch",
			"iface_type":          ifaceTypeID,
			"candidate_count":     fanout,
		},
	}
	StampSynthesized(ne, SynthCSharpIfaceDispatch)
	return ne
}
