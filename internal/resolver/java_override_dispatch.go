package resolver

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// javaOverrideDispatchCap bounds how many overrides a single ambiguous call
// may fan out to. A name shared by more definitions than this is too generic
// to attribute confidently, so the call is left ambiguous rather than sprayed
// across the graph.
const javaOverrideDispatchCap = 8

// resolveJavaOverrideDispatch fans out an ambiguous Java member call whose
// same-name candidates are overrides related through the class hierarchy into
// one call edge per override — the call-hierarchy semantics jdtls and gopls
// present, where a call on a supertype-typed receiver is a usage of every
// override in that hierarchy. Without this, a `x.toString()` site whose static
// type is a base class stays unresolved (two candidate overrides, no exact
// type match) and reports as a usage of neither override.
//
// The primary edge binds to the common base override; each derived override
// gets a parallel edge. All land at the ast_inferred tier with
// Meta["dispatch"]="override" — visible in default find_usages (recall parity
// with the language server), not gated behind the speculative filter — and no
// longer classify as ambiguous_multi_match. Scoped to Java so Go/TS/Python
// dispatch presentation is unchanged.
func (r *Resolver) resolveJavaOverrideDispatch() int {
	g := r.graph
	if g == nil {
		return 0
	}
	ancestors := javaTypeAncestors(g)
	if len(ancestors) == 0 {
		return 0
	}

	type fanout struct {
		edge   *graph.Edge
		base   *graph.Node
		others []*graph.Node
	}
	var jobs []fanout
	for e := range g.EdgesByKind(graph.EdgeCalls) {
		if e == nil || e.IsSpeculative() {
			continue
		}
		name := javaUnresolvedMemberName(e.To)
		if name == "" || strings.HasSuffix(name, ".<init>") {
			continue
		}
		caller := r.cachedGetNode(e.From)
		if caller == nil || caller.Language != "java" {
			continue
		}
		cands := javaOverrideCandidates(r.cachedFindNodesByNameInRepo(name, r.callerRepoPrefix(e)))
		if len(cands) < 2 || len(cands) > javaOverrideDispatchCap {
			continue
		}
		base, others := pickJavaOverrideBase(cands, ancestors)
		if base == nil {
			continue
		}
		jobs = append(jobs, fanout{edge: e, base: base, others: others})
	}

	n := 0
	for _, j := range jobs {
		oldTo := j.edge.To
		j.edge.To = j.base.ID
		j.edge.Origin = graph.OriginASTInferred
		j.edge.Confidence = 0.5
		if j.edge.Meta == nil {
			j.edge.Meta = map[string]any{}
		}
		j.edge.Meta["dispatch"] = "override"
		g.ReindexEdges([]graph.EdgeReindex{{Edge: j.edge, OldTo: oldTo}})
		n++
		for _, o := range j.others {
			g.AddEdge(&graph.Edge{
				From: j.edge.From, To: o.ID, Kind: graph.EdgeCalls,
				FilePath: j.edge.FilePath, Line: j.edge.Line,
				Origin: graph.OriginASTInferred, Confidence: 0.5,
				Meta: map[string]any{"dispatch": "override"},
			})
			n++
		}
	}
	return n
}

// javaUnresolvedMemberName returns the method name of an `unresolved::*.<name>`
// member-call target, or "" for any other target shape.
func javaUnresolvedMemberName(to string) string {
	name := graph.UnresolvedName(to)
	if name == "" {
		return ""
	}
	rest, ok := strings.CutPrefix(name, "*.")
	if !ok || strings.Contains(rest, "::") {
		return ""
	}
	return rest
}

// javaOverrideCandidates filters name-matched nodes to the in-repo Java method
// definitions, one per declaring type (deduped by receiver), excluding stubs
// and definitions with no declaring type.
func javaOverrideCandidates(raw []*graph.Node) []*graph.Node {
	var out []*graph.Node
	seen := map[string]bool{}
	for _, n := range raw {
		if n == nil || n.Language != "java" || n.Kind != graph.KindMethod {
			continue
		}
		if graph.IsStub(n.ID) || graph.IsUnresolvedTarget(n.ID) {
			continue
		}
		recv := nodeReceiverType(n)
		if recv == "" || seen[recv] {
			continue
		}
		seen[recv] = true
		out = append(out, n)
	}
	return out
}

// pickJavaOverrideBase returns the candidate whose declaring type is an
// ancestor-or-self of every other candidate's declaring type (the common base
// the others override), plus the remaining candidates. Returns (nil, nil) when
// the candidates are not a single override hierarchy — the precision gate that
// keeps unrelated same-name methods from being sprayed together.
func pickJavaOverrideBase(cands []*graph.Node, ancestors map[string]map[string]bool) (*graph.Node, []*graph.Node) {
	for i, base := range cands {
		rb := nodeReceiverType(base)
		isBaseOfAll := true
		for k, other := range cands {
			if k == i {
				continue
			}
			ro := nodeReceiverType(other)
			if ro == rb {
				continue
			}
			if anc := ancestors[ro]; anc == nil || !anc[rb] {
				isBaseOfAll = false
				break
			}
		}
		if isBaseOfAll {
			others := make([]*graph.Node, 0, len(cands)-1)
			for k, c := range cands {
				if k != i {
					others = append(others, c)
				}
			}
			return base, others
		}
	}
	return nil, nil
}

// javaTypeAncestors builds, for each Java type/interface simple name, the
// transitive set of its supertype simple names via extends/implements/composes
// edges. Empty when the graph indexes no Java hierarchy.
func javaTypeAncestors(g graph.Store) map[string]map[string]bool {
	// Direct parent map by simple name.
	direct := map[string]map[string]bool{}
	for _, row := range structuralParentEdges(g) {
		from := g.GetNode(row.FromID)
		to := g.GetNode(row.ToID)
		if from == nil || to == nil || from.Language != "java" || to.Language != "java" {
			continue
		}
		if from.Name == "" || to.Name == "" || from.Name == to.Name {
			continue
		}
		set := direct[from.Name]
		if set == nil {
			set = map[string]bool{}
			direct[from.Name] = set
		}
		set[to.Name] = true
	}
	if len(direct) == 0 {
		return nil
	}
	// Transitive closure via DFS from each type.
	closure := make(map[string]map[string]bool, len(direct))
	var visit func(t string, acc map[string]bool, seen map[string]bool)
	visit = func(t string, acc, seen map[string]bool) {
		for p := range direct[t] {
			if seen[p] {
				continue
			}
			seen[p] = true
			acc[p] = true
			visit(p, acc, seen)
		}
	}
	for t := range direct {
		acc := map[string]bool{}
		visit(t, acc, map[string]bool{t: true})
		closure[t] = acc
	}
	return closure
}
