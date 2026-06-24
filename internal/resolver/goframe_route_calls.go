package resolver

import (
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// goframeRouteVia is the Meta["via"] tag the Go extractor stamps on a
// GoFrame route placeholder (route → request-struct type).
const goframeRouteVia = "goframe-route"

// ResolveGoFrameRoutes joins each GoFrame route to the controller method
// that handles it, by request-struct type rather than name: a route
// materialised from a `g.Meta`-tagged request struct binds to the method
// whose pointer parameter is that struct. When several methods share a
// request type, a controller bound via `g.Bind(new(Ctrl))` (the addonRoot
// set) wins, then a same-directory method. Emits both a call edge
// (route → method, for get_callers) and a handles_route edge
// (method → route, for analyze routes). Typed tier.
//
// Returns the number of routes joined to a handler.
func ResolveGoFrameRoutes(g graph.Store) int {
	if g == nil {
		return 0
	}
	byReqType := map[string][]*graph.Node{}
	for _, n := range nodesByKindsOrAll(g, graph.KindMethod, graph.KindFunction) {
		if n == nil || n.Meta == nil {
			continue
		}
		if rt, _ := n.Meta["goframe_request_type"].(string); rt != "" {
			byReqType[rt] = append(byReqType[rt], n)
		}
	}
	if len(byReqType) == 0 {
		return 0
	}

	resolved := 0
	var reindex []graph.EdgeReindex
	var batch []*graph.Edge
	for e := range g.EdgesByKind(graph.EdgeCalls) {
		if e == nil || e.Meta == nil {
			continue
		}
		if v, _ := e.Meta["via"].(string); v != goframeRouteVia {
			continue
		}
		reqType, _ := e.Meta["goframe_request_type"].(string)
		if reqType == "" {
			continue
		}
		target := goframePickMethod(g, e, byReqType[reqType])

		want := "unresolved::*." + reqType
		if target != nil {
			want = target.ID
		}
		if e.To == want {
			if target != nil {
				resolved++
			}
			continue
		}
		oldTo := e.To
		e.To = want
		if target != nil {
			e.Origin = graph.OriginASTInferred
			e.Confidence = ConfidenceTyped
			e.ConfidenceLabel = graph.ConfidenceLabelFor(graph.EdgeCalls, ConfidenceTyped)
			StampSynthesizedTyped(e, SynthGoFrameRoute)
			resolved++
			// Mirror the route in the handler→route direction so the route
			// surfaces in analyze kind=routes.
			routeID, _ := e.Meta["goframe_route"].(string)
			if routeID == "" {
				routeID = e.From
			}
			batch = append(batch, &graph.Edge{
				From: target.ID, To: routeID, Kind: graph.EdgeHandlesRoute,
				FilePath: e.FilePath, Line: e.Line,
				Origin: graph.OriginASTInferred,
				Meta:   map[string]any{"via": goframeRouteVia, "framework": "goframe"},
			})
		} else {
			e.Origin = graph.OriginASTInferred
			e.Confidence = 0
			e.ConfidenceLabel = ""
			UnstampSynthesized(e)
		}
		reindex = append(reindex, graph.EdgeReindex{Edge: e, OldTo: oldTo})
	}
	if len(reindex) > 0 {
		g.ReindexEdges(reindex)
	}
	for _, ne := range batch {
		g.AddEdge(ne)
	}
	return resolved
}

// goframePickMethod selects the handler for a route from the methods of a
// request type: candidates are first narrowed to the route's request-struct
// package (so a same-named request type in another package can never claim the
// route), then a bound controller (addonRoot) wins, then a method in the
// route's directory, then a unique match.
func goframePickMethod(g graph.Store, route *graph.Edge, cands []*graph.Node) *graph.Node {
	cands = sameBoundaryCandidates(g, route.From, cands)
	cands = goframeSamePackage(route, cands)
	if len(cands) == 0 {
		return nil
	}
	if len(cands) == 1 {
		return cands[0]
	}
	// addonRoot: prefer bound controllers.
	var boundCands []*graph.Node
	for _, c := range cands {
		if c.Meta != nil {
			if b, _ := c.Meta["goframe_bound"].(bool); b {
				boundCands = append(boundCands, c)
			}
		}
	}
	if len(boundCands) == 1 {
		return boundCands[0]
	}
	if len(boundCands) > 1 {
		cands = boundCands
	}
	// Then a same-directory method.
	routeDir := goframeDir(route.FilePath)
	var sameDir []*graph.Node
	for _, c := range cands {
		if goframeDir(c.FilePath) == routeDir {
			sameDir = append(sameDir, c)
		}
	}
	if len(sameDir) == 1 {
		return sameDir[0]
	}
	if len(sameDir) > 1 {
		cands = sameDir
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].ID < cands[j].ID })
	if len(cands) == 1 {
		return cands[0]
	}
	return nil
}

// goframeSamePackage narrows candidates to those whose request-struct package
// matches the route's. A route materialised from `pkg.CreateReq` must bind only
// to a handler taking `*pkg.CreateReq` — never to a same-named `CreateReq` in
// another package. The package qualifier is the extractor's `goframe_request_pkg`
// stamp; when the route carries none (a same-package, bare-name binding the
// extractor could not qualify), candidates pass through unfiltered so existing
// single-package resolution is never weakened.
func goframeSamePackage(route *graph.Edge, cands []*graph.Node) []*graph.Node {
	routePkg := ""
	if route.Meta != nil {
		routePkg, _ = route.Meta["goframe_request_pkg"].(string)
	}
	if routePkg == "" {
		return cands
	}
	out := make([]*graph.Node, 0, len(cands))
	for _, c := range cands {
		if c == nil || c.Meta == nil {
			continue
		}
		// A candidate with no package qualifier cannot be proven to belong to
		// the route's package, so it is excluded once the route is qualified.
		if cp, _ := c.Meta["goframe_request_pkg"].(string); cp == routePkg {
			out = append(out, c)
		}
	}
	return out
}

func goframeDir(path string) string {
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		return path[:i]
	}
	return ""
}
