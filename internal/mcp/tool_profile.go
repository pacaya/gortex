package mcp

import (
	"context"
	"slices"
	"sort"

	"github.com/mark3labs/mcp-go/mcp"
)

// IsToolEnabled reports whether a tool is reachable in this server's
// current profile — registered either as a live tool (in tools/list)
// or as a deferred tool behind tools_search. An empty name, or a name
// that was never registered, returns false.
func (s *Server) IsToolEnabled(name string) bool {
	if name == "" {
		return false
	}
	if _, ok := s.mcpServer.ListTools()[name]; ok {
		return true
	}
	if s.lazy != nil && slices.Contains(s.lazy.DeferredNames(), name) {
		return true
	}
	return false
}

// toolStatus classifies one tool name as live (eagerly in tools/list),
// deferred (hidden behind tools_search), or absent (not registered).
func (s *Server) toolStatus(name string) string {
	if _, ok := s.mcpServer.ListTools()[name]; ok {
		return "live"
	}
	if s.lazy != nil && slices.Contains(s.lazy.DeferredNames(), name) {
		return "deferred"
	}
	return "absent"
}

// liveToolNames returns the sorted names of every tool currently in
// tools/list (the eagerly-visible surface).
func (s *Server) liveToolNames() []string {
	live := s.mcpServer.ListTools()
	out := make([]string, 0, len(live))
	for n := range live {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// registerToolProfileTool wires the `tool_profile` introspection tool.
func (s *Server) registerToolProfileTool() {
	s.addTool(
		mcp.NewTool("tool_profile",
			mcp.WithDescription("Report the active MCP tool profile so the agent knows what is actually available instead of guessing. With no arguments: returns `{lazy_enabled, total, live_count, deferred_count, live[], deferred[], scopes{}}` — `live` tools are in the current tools/list, `deferred` tools are reachable via `tools_search`. With `tool:\"<name>\"`: returns `{tool, enabled, status, scope}` for that one tool (status ∈ live | deferred | absent)."),
			mcp.WithString("tool", mcp.Description("Optional — report only this tool's enabled status and scope instead of the whole profile.")),
		),
		s.handleToolProfile,
	)
}

func (s *Server) handleToolProfile(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	scopes := s.toolScopes.snapshot()

	// Per-tool advisory mode.
	if name, _ := args["tool"].(string); name != "" {
		return s.respondJSONOrTOON(ctx, req, map[string]any{
			"tool":    name,
			"enabled": s.IsToolEnabled(name),
			"status":  s.toolStatus(name),
			"scope":   scopes[name],
		})
	}

	// Full-profile mode.
	live := s.liveToolNames()
	var deferred []string
	lazyEnabled := false
	if s.lazy != nil {
		lazyEnabled = s.lazy.Enabled()
		deferred = s.lazy.DeferredNames()
	}
	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"lazy_enabled":   lazyEnabled,
		"total":          len(live) + len(deferred),
		"live_count":     len(live),
		"deferred_count": len(deferred),
		"live":           live,
		"deferred":       deferred,
		"scopes":         scopes,
	})
}
