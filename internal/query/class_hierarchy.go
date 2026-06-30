package query

import "github.com/zzet/gortex/internal/graph"

// HierarchyDirection picks which side of the class-hierarchy graph
// ClassHierarchy traverses from the seed.
type HierarchyDirection string

const (
	HierarchyUp   HierarchyDirection = "up"
	HierarchyDown HierarchyDirection = "down"
	HierarchyBoth HierarchyDirection = "both"
)

// typeHierarchyEdgeKinds is the set traversed when the visited node is
// a type / interface. EdgeExtends covers single + multiple inheritance,
// EdgeImplements bridges concrete type ↔ interface, EdgeComposes covers
// Go struct embedding / Rust trait bounds / Python multiple inheritance
// mixins.
var typeHierarchyEdgeKinds = map[graph.EdgeKind]bool{
	graph.EdgeExtends:    true,
	graph.EdgeImplements: true,
	graph.EdgeComposes:   true,
}

// methodHierarchyEdgeKinds is the set traversed when the visited node
// is a method. EdgeOverrides is method-level: child method → parent /
// interface method.
var methodHierarchyEdgeKinds = map[graph.EdgeKind]bool{
	graph.EdgeOverrides: true,
}

// ClassHierarchy returns the inheritance subgraph rooted at seedID.
//
// Walks the graph through EdgeExtends + EdgeImplements + EdgeComposes
// for type nodes and EdgeOverrides for method nodes. Direction picks
// the side(s) of the hierarchy:
//
//   - HierarchyUp   — outgoing edges (parents / interfaces a child
//     extends or implements; parent methods this method overrides).
//   - HierarchyDown — incoming edges (subclasses / implementers; methods
//     that override this one).
//   - HierarchyBoth — union of the two.
//
// When includeMethods is true and a type / interface node is reached,
// its methods (in-edges of EdgeMemberOf whose From side is a function
// or method node) are pulled into the result and override links from
// each method are walked in the same direction(s).
//
// Workspace / project scope is enforced via opts.ScopeAllows on every
// neighbour. opts.MinTier is applied as a post-pass over the collected
// edges (consistent with the rest of the engine surface).
//
// Picks ClassHierarchyTraverser when the backend implements it: that
// path runs the BFS as one variable-length traversal per direction
// inside the engine, replacing the per-node GetNode + GetIn/OutEdges
// loop the fallback runs. On a disk backend a deep walk over a wide
// implementer set previously fired hundreds of round-trips per
// call — the pushdown drops to one or two queries.
func (e *Engine) ClassHierarchy(seedID string, direction HierarchyDirection, depth int, includeMethods bool, opts QueryOptions) *SubGraph {
	if direction == "" {
		direction = HierarchyBoth
	}
	if depth <= 0 {
		depth = 5
	}
	if depth > 64 {
		depth = 64
	}

	seed := e.g.GetNode(seedID)
	if seed == nil {
		return &SubGraph{}
	}

	if _, ok := e.g.(graph.ClassHierarchyTraverser); ok {
		return e.classHierarchyPushdown(seed, direction, depth, includeMethods, opts)
	}
	return e.classHierarchyWalk(seed, direction, depth, includeMethods, opts)
}

// classHierarchyPushdown runs the BFS through the
// ClassHierarchyTraverser capability. Each direction issues one or
// two backend round-trips (the type-edge kinds, optionally chasing
// methods through EdgeMemberOf) instead of the per-frontier per-hop
// loop the fallback runs.
func (e *Engine) classHierarchyPushdown(
	seed *graph.Node,
	direction HierarchyDirection,
	depth int,
	includeMethods bool,
	opts QueryOptions,
) *SubGraph {
	tr := e.g.(graph.ClassHierarchyTraverser)
	walkUp := direction == HierarchyUp || direction == HierarchyBoth
	walkDown := direction == HierarchyDown || direction == HierarchyBoth

	typeKinds := []graph.EdgeKind{graph.EdgeExtends, graph.EdgeImplements, graph.EdgeComposes}
	methodKinds := []graph.EdgeKind{graph.EdgeOverrides}

	// Per-direction walks: type-hierarchy kinds rooted at seed if seed
	// is a type/interface; method-hierarchy kinds rooted at seed if
	// seed is a method/function. Methods reached via includeMethods
	// are added as separate roots in a follow-up pass.
	var rows []graph.ClassHierarchyRow
	seedIsType := seed.Kind == graph.KindType || seed.Kind == graph.KindInterface
	seedIsMethod := seed.Kind == graph.KindMethod || seed.Kind == graph.KindFunction
	if seedIsType {
		if walkUp {
			rows = append(rows, tr.ClassHierarchyTraverse(seed.ID, "up", typeKinds, depth)...)
		}
		if walkDown {
			rows = append(rows, tr.ClassHierarchyTraverse(seed.ID, "down", typeKinds, depth)...)
		}
	} else if seedIsMethod {
		if walkUp {
			rows = append(rows, tr.ClassHierarchyTraverse(seed.ID, "up", methodKinds, depth)...)
		}
		if walkDown {
			rows = append(rows, tr.ClassHierarchyTraverse(seed.ID, "down", methodKinds, depth)...)
		}
	}

	// Collect the node IDs visited so we can resolve them in one
	// batched fetch, instead of one GetNode per row.
	visited := map[string]bool{seed.ID: true}
	for _, r := range rows {
		for _, id := range r.Path {
			visited[id] = true
		}
	}

	// includeMethods folds in EdgeMemberOf hops from every visited
	// type node. The override walk on each method then runs as a
	// further pushdown call.
	memberLinks := []struct {
		from, to string
		kind     graph.EdgeKind
	}{}
	if includeMethods {
		typeIDs := make([]string, 0, len(visited))
		for id := range visited {
			n := e.g.GetNode(id)
			if n == nil {
				continue
			}
			if n.Kind == graph.KindType || n.Kind == graph.KindInterface {
				typeIDs = append(typeIDs, id)
			}
		}
		if len(typeIDs) > 0 {
			memberIns := e.g.GetInEdgesByNodeIDs(typeIDs)
			methodRoots := []string{}
			for _, id := range typeIDs {
				for _, ed := range memberIns[id] {
					if ed == nil || ed.Kind != graph.EdgeMemberOf {
						continue
					}
					member := e.g.GetNode(ed.From)
					if member == nil {
						continue
					}
					if member.Kind != graph.KindMethod && member.Kind != graph.KindFunction {
						continue
					}
					memberLinks = append(memberLinks, struct {
						from, to string
						kind     graph.EdgeKind
					}{from: member.ID, to: id, kind: graph.EdgeMemberOf})
					if !visited[member.ID] {
						visited[member.ID] = true
						methodRoots = append(methodRoots, member.ID)
					}
				}
			}
			for _, mid := range methodRoots {
				if walkUp {
					subRows := tr.ClassHierarchyTraverse(mid, "up", methodKinds, depth)
					for _, sr := range subRows {
						for _, id := range sr.Path {
							visited[id] = true
						}
					}
					rows = append(rows, methodPathsWithRoot(mid, subRows)...)
				}
				if walkDown {
					subRows := tr.ClassHierarchyTraverse(mid, "down", methodKinds, depth)
					for _, sr := range subRows {
						for _, id := range sr.Path {
							visited[id] = true
						}
					}
					rows = append(rows, methodPathsWithRoot(mid, subRows)...)
				}
			}
		}
	}

	// Resolve every visited node + collect the edge pointers in one
	// place. The capability doesn't carry edge pointers (on-disk
	// backend edges aren't first-class objects), so we re-resolve them via
	// GetOutEdgesByNodeIDs / GetInEdgesByNodeIDs once per direction.
	allIDs := make([]string, 0, len(visited))
	for id := range visited {
		allIDs = append(allIDs, id)
	}
	nodeMap := e.g.GetNodesByIDs(allIDs)
	if nodeMap[seed.ID] == nil {
		nodeMap[seed.ID] = seed
	}

	resultNodes := make([]*graph.Node, 0, len(allIDs))
	for _, id := range allIDs {
		n := nodeMap[id]
		if n == nil {
			continue
		}
		if opts.hasScopeFilter() && id != seed.ID && !opts.ScopeAllows(n) {
			continue
		}
		resultNodes = append(resultNodes, n)
	}

	// Reconstruct edges: each row's Path[i] → Path[i+1] (for i>=0)
	// carries an edge of EdgeKinds[i]. The seed's first hop is from
	// seed → Path[0]. The direction the walk came from determines
	// whether the edge points seed→neighbour or neighbour→seed.
	resultEdges := make([]*graph.Edge, 0)
	seenEdge := make(map[string]bool)
	addEdge := func(from, to string, kind graph.EdgeKind) {
		// Find the actual *Edge so the downstream FilterByMinTier
		// still has the origin / tier columns to read.
		var found *graph.Edge
		for _, ed := range e.g.GetOutEdges(from) {
			if ed == nil {
				continue
			}
			if ed.To == to && ed.Kind == kind {
				found = ed
				break
			}
		}
		if found == nil {
			// Direction-flipped lookup — happens when "down" walks
			// hand back paths whose hops are in-edges of the seed.
			for _, ed := range e.g.GetInEdges(from) {
				if ed == nil {
					continue
				}
				if ed.From == to && ed.Kind == kind {
					found = ed
					break
				}
			}
		}
		if found == nil {
			return
		}
		k := found.From + "→" + found.To + "::" + string(found.Kind) + ":" + edgeMetaTag(found)
		if seenEdge[k] {
			return
		}
		seenEdge[k] = true
		resultEdges = append(resultEdges, found)
	}
	for _, r := range rows {
		prev := seed.ID
		for i, nb := range r.Path {
			if i >= len(r.EdgeKinds) {
				break
			}
			addEdge(prev, nb, r.EdgeKinds[i])
			prev = nb
		}
	}
	for _, link := range memberLinks {
		addEdge(link.from, link.to, link.kind)
	}

	// Workspace-scope post-filter for edges (any edge whose endpoints
	// were dropped from resultNodes is also dropped).
	if opts.hasScopeFilter() {
		nodeSet := make(map[string]bool, len(resultNodes))
		for _, n := range resultNodes {
			nodeSet[n.ID] = true
		}
		filtered := resultEdges[:0]
		for _, ed := range resultEdges {
			if !nodeSet[ed.From] || !nodeSet[ed.To] {
				continue
			}
			filtered = append(filtered, ed)
		}
		resultEdges = filtered
	}

	sg := &SubGraph{
		Nodes:      resultNodes,
		Edges:      resultEdges,
		TotalNodes: len(resultNodes),
		TotalEdges: len(resultEdges),
	}
	if opts.MinTier != "" {
		sg.FilterByMinTier(opts.MinTier)
	}
	return sg
}

// methodPathsWithRoot rebases the traversal rows so the seed prefix
// in their paths reflects the method root they came from rather than
// the outer ClassHierarchy seed. Returned rows are otherwise
// unchanged.
func methodPathsWithRoot(root string, rows []graph.ClassHierarchyRow) []graph.ClassHierarchyRow {
	out := make([]graph.ClassHierarchyRow, len(rows))
	for i, r := range rows {
		newPath := append([]string{root}, r.Path...)
		newKinds := append([]graph.EdgeKind{}, r.EdgeKinds...)
		// The seed→Path[0] hop is encoded by EdgeMemberOf in the outer
		// addEdge pass, so we keep the EdgeKinds slice aligned with
		// the slice the caller iterates ([0]=Path[0]→Path[1]).
		out[i] = graph.ClassHierarchyRow{Path: newPath[1:], EdgeKinds: newKinds}
		_ = newPath
	}
	return out
}

// classHierarchyWalk is the in-memory BFS path. Kept verbatim so the
// in-memory backend has the same shape it had before the pushdown
// landed.
func (e *Engine) classHierarchyWalk(
	seed *graph.Node,
	direction HierarchyDirection,
	depth int,
	includeMethods bool,
	opts QueryOptions,
) *SubGraph {
	walkUp := direction == HierarchyUp || direction == HierarchyBoth
	walkDown := direction == HierarchyDown || direction == HierarchyBoth

	visitedNodes := make(map[string]bool)
	visitedEdges := make(map[string]bool)
	var resultNodes []*graph.Node
	var resultEdges []*graph.Edge

	addNode := func(n *graph.Node) {
		if n == nil || visitedNodes[n.ID] {
			return
		}
		visitedNodes[n.ID] = true
		resultNodes = append(resultNodes, n)
	}

	edgeKey := func(ed *graph.Edge) string {
		return ed.From + "→" + ed.To + "::" + string(ed.Kind) + ":" + edgeMetaTag(ed)
	}
	addEdge := func(ed *graph.Edge) {
		if ed == nil {
			return
		}
		k := edgeKey(ed)
		if visitedEdges[k] {
			return
		}
		visitedEdges[k] = true
		resultEdges = append(resultEdges, ed)
	}

	addNode(seed)

	type queued struct {
		id    string
		depth int
	}
	queue := []queued{{id: seed.ID, depth: 0}}

	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if cur.depth >= depth {
			continue
		}

		curNode := e.g.GetNode(cur.id)
		if curNode == nil {
			continue
		}

		isType := curNode.Kind == graph.KindType || curNode.Kind == graph.KindInterface
		isMethod := curNode.Kind == graph.KindMethod || curNode.Kind == graph.KindFunction

		if includeMethods && isType {
			for _, mEdge := range e.g.GetInEdges(cur.id) {
				if mEdge.Kind != graph.EdgeMemberOf {
					continue
				}
				member := e.g.GetNode(mEdge.From)
				if member == nil {
					continue
				}
				if member.Kind != graph.KindMethod && member.Kind != graph.KindFunction {
					continue
				}
				if opts.hasScopeFilter() && !opts.ScopeAllows(member) {
					continue
				}
				addNode(member)
				addEdge(mEdge)
				queue = append(queue, queued{id: member.ID, depth: cur.depth})
			}
		}

		var kindSet map[graph.EdgeKind]bool
		switch {
		case isType:
			kindSet = typeHierarchyEdgeKinds
		case isMethod:
			kindSet = methodHierarchyEdgeKinds
		default:
			continue
		}

		if walkUp {
			for _, ed := range e.g.GetOutEdges(cur.id) {
				if !kindSet[ed.Kind] {
					continue
				}
				neighbor := e.g.GetNode(ed.To)
				if neighbor == nil {
					continue
				}
				if opts.hasScopeFilter() && !opts.ScopeAllows(neighbor) {
					continue
				}
				addEdge(ed)
				if !visitedNodes[neighbor.ID] {
					addNode(neighbor)
					queue = append(queue, queued{id: neighbor.ID, depth: cur.depth + 1})
				}
			}
		}
		if walkDown {
			for _, ed := range e.g.GetInEdges(cur.id) {
				if !kindSet[ed.Kind] {
					continue
				}
				neighbor := e.g.GetNode(ed.From)
				if neighbor == nil {
					continue
				}
				if opts.hasScopeFilter() && !opts.ScopeAllows(neighbor) {
					continue
				}
				addEdge(ed)
				if !visitedNodes[neighbor.ID] {
					addNode(neighbor)
					queue = append(queue, queued{id: neighbor.ID, depth: cur.depth + 1})
				}
			}
		}
	}

	sg := &SubGraph{
		Nodes:      resultNodes,
		Edges:      resultEdges,
		TotalNodes: len(resultNodes),
		TotalEdges: len(resultEdges),
	}
	if opts.MinTier != "" {
		sg.FilterByMinTier(opts.MinTier)
	}
	return sg
}

// edgeMetaTag is a small disambiguator for edges that share From / To /
// Kind but carry distinct metadata (e.g. multiple EdgeOverrides between
// the same method pair via different language sources). Falls back to
// the edge file:line when no semantic_source is set.
func edgeMetaTag(ed *graph.Edge) string {
	if ed.Meta != nil {
		if src, ok := ed.Meta["semantic_source"].(string); ok && src != "" {
			return src
		}
	}
	if ed.FilePath != "" {
		return ed.FilePath
	}
	return ""
}
