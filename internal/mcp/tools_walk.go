package mcp

import (
	"context"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/zzet/gortex/internal/query"
)

// registerWalkGraphTool wires walk_graph — a token-budgeted, free-form
// graph traversal. Unlike the fixed-purpose traversal tools
// (get_call_chain, get_dependents) the caller picks the edge kinds and
// the direction, and the walk auto-stops once the estimated encoded
// size of the result reaches a token budget.
func (s *Server) registerWalkGraphTool() {
	edgeKindList := strings.Join(query.KnownEdgeKinds(), ", ")
	s.addTool(
		mcp.NewTool("walk_graph",
			mcp.WithDescription("Token-budgeted free-form graph traversal from a start symbol. Pick the edge kinds and direction; the walk expands breadth-first and stops automatically once the encoded result would exceed the token budget. Use it to explore a neighbourhood when the fixed-purpose tools (get_call_chain, get_dependents) don't match the relationship you want to follow."),
			mcp.WithString("id", mcp.Required(), mcp.Description("Start symbol node ID (e.g. pkg/server.go::HandleRequest).")),
			mcp.WithString("edge_kinds", mcp.Description("Comma-separated edge kinds to follow (default: calls). Valid kinds: "+edgeKindList+".")),
			mcp.WithString("direction", mcp.Description("Traversal direction: out (default — follow outgoing edges), in (incoming), or both (undirected).")),
			mcp.WithString("community", mcp.Description("Constrain the walk to a single detected community. Accepts a community ID or label. Neighbours in a different community are dropped; structural nodes with no community membership still pass. No-op until community detection has run.")),
			mcp.WithNumber("token_budget", mcp.Description("Approximate token ceiling for the encoded result (default 6000). The walk stops adding nodes once the estimate would exceed this.")),
			mcp.WithNumber("max_depth", mcp.Description("Hard cap on BFS depth, applied even when the token budget would allow deeper expansion (default 8).")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx (GCX1 compact wire format), or toon.")),
			mcp.WithString("repo", mcp.Description("Filter results to a specific repository prefix")),
			mcp.WithString("project", mcp.Description("Filter results to repositories in a specific project")),
			mcp.WithString("ref", mcp.Description("Filter results to repositories with a specific reference tag")),
			mcp.WithString("workspace", mcp.Description("Workspace override. In workspace-bound sessions this must match the active workspace.")),
			mcp.WithString("scope", mcp.Description("Saved scope name. Ignored for git diff scopes; explicit repo/project/ref filters take precedence.")),
		),
		s.handleWalkGraph,
	)
}

// handleWalkGraph runs WalkBudgeted with the request's parameters and
// returns the resulting subgraph. The response carries budget_hit and
// stopped_at_depth so the caller knows whether the walk was truncated.
func (s *Server) handleWalkGraph(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, err := req.RequireString("id")
	if err != nil {
		return mcp.NewToolResultError("id is required"), nil
	}

	edgeKinds, kindErr := query.ParseEdgeKindsCSV(req.GetString("edge_kinds", "calls"))
	if kindErr != nil {
		return mcp.NewToolResultError(kindErr.Error()), nil
	}
	if len(edgeKinds) == 0 {
		// An explicit empty value would otherwise mean "every kind";
		// walk_graph's documented default is calls.
		edgeKinds, _ = query.ParseEdgeKindsCSV("calls")
	}

	direction := strings.ToLower(strings.TrimSpace(req.GetString("direction", "out")))
	switch direction {
	case "", "out", "in", "both":
	default:
		return mcp.NewToolResultError(fmt.Sprintf("direction must be out, in, or both (got %q)", direction)), nil
	}

	tokenBudget := req.GetInt("token_budget", 6000)
	if tokenBudget <= 0 {
		tokenBudget = 6000
	}
	maxDepth := req.GetInt("max_depth", 8)
	if maxDepth <= 0 {
		maxDepth = 8
	}

	eng := s.engineFor(ctx)
	seed := eng.GetSymbol(id)
	if seed == nil {
		return mcp.NewToolResultError(fmt.Sprintf("symbol not found: %s", id)), nil
	}

	resolved, errResult := s.resolveScope(ctx, req, IntentReach)
	if errResult != nil {
		return errResult, nil
	}
	if !resolvedScopeAllowsNode(resolved, seed) {
		return symbolNotFoundGuidance(id), nil
	}

	// Optional community constraint: accept either a community ID or
	// label and resolve it to an ID, then hand the walk the node->comm
	// map so it can drop cross-community neighbours. A nil community
	// result (analysis not yet run) makes the filter a no-op.
	commID, nodeToComm := s.resolveCommunityFilter(req.GetString("community", ""))

	sg := eng.WalkBudgeted(id, query.WalkOptions{
		EdgeKinds:   edgeKinds,
		Direction:   direction,
		TokenBudget: tokenBudget,
		MaxDepth:    maxDepth,
		WorkspaceID: resolved.WorkspaceID,
		ProjectID:   resolved.ProjectID,
		RepoAllow:   resolved.RepoAllow,
		CommunityID: commID,
		NodeToComm:  nodeToComm,
	})

	budgetHit, stoppedAt := sg.BudgetHit, sg.StoppedAtDepth
	sg = filterSubGraphByResolvedScope(sg, resolved)
	// Keep the traversal budget fields stable after the defensive
	// post-filter.
	sg.BudgetHit = budgetHit
	sg.StoppedAtDepth = stoppedAt
	enrichSubGraphEdges(sg)
	return s.returnScopedSubGraph(ctx, req, sg, resolved)
}

// resolveCommunityFilter turns a community ID-or-label request argument
// into the (communityID, nodeToComm) pair WalkBudgeted's community gate
// needs. It mirrors winnowSymbols' label resolution: a label is mapped
// to its community ID up front; an ID passes through. When community
// detection has not run yet (s.getCommunities() is nil) or the argument
// is empty, both results are zero-valued — the walk's gate then no-ops.
//
// An unrecognised non-empty label is returned verbatim as the community
// ID; since no node carries that membership the gate drops every
// community-tagged neighbour, which is the correct "no such community"
// behaviour (an empty walk beats silently ignoring the filter).
func (s *Server) resolveCommunityFilter(arg string) (string, map[string]string) {
	want := strings.TrimSpace(arg)
	if want == "" {
		return "", nil
	}
	cr := s.getCommunities()
	if cr == nil {
		return "", nil
	}
	for _, com := range cr.Communities {
		if com.Label != "" && com.Label == want {
			return com.ID, cr.NodeToComm
		}
	}
	return want, cr.NodeToComm
}
