package mcp

import (
	"context"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/query"
)

type ResolvedScope struct {
	WorkspaceID string
	ProjectID   string
	RepoAllow   map[string]bool
	Applied     string
}

type ToolIntent string

const (
	IntentLocate  ToolIntent = "locate"
	IntentReach   ToolIntent = "reach"
	IntentAnalyze ToolIntent = "analyze"
)

func (s *Server) resolveScope(ctx context.Context, req mcp.CallToolRequest, intent ToolIntent) (ResolvedScope, *mcp.CallToolResult) {
	scope, err := s.resolveScopeForRequest(ctx, req, intent)
	if err != nil {
		return ResolvedScope{}, mcp.NewToolResultError(err.Error())
	}
	return scope, nil
}

func (s *Server) resolveScopeForRequest(ctx context.Context, req mcp.CallToolRequest, intent ToolIntent) (ResolvedScope, error) {
	intent = normalizeToolIntent(intent, req.Params.Name)

	repo := strings.TrimSpace(req.GetString("repo", ""))
	// The selector may be a filesystem path (the CLI defaults to the
	// caller's working directory) -- normalize to the tracked prefix so
	// the filter matches what the workspace knows the repo as.
	if repo != "*" {
		if p := s.resolveRepoPrefix(repo); p != "" {
			repo = p
		}
	}
	project := strings.TrimSpace(req.GetString("project", ""))
	ref := strings.TrimSpace(req.GetString("ref", ""))
	workspaceArg := strings.TrimSpace(req.GetString("workspace", ""))

	scopeArg := strings.TrimSpace(req.GetString("scope", ""))
	if gitDiffScopes[scopeArg] {
		scopeArg = ""
	}
	explicitNarrowing := repo != "" || project != "" || ref != "" || workspaceArg != "" || scopeArg != ""

	var scopeRepos map[string]bool
	if scopeArg != "" && repo == "" && project == "" && ref == "" {
		sc, ok := s.lookupScope(scopeArg)
		if !ok {
			return ResolvedScope{}, fmt.Errorf("unknown scope %q — run list_scopes to see saved scopes, or create one with save_scope", scopeArg)
		}
		scopeRepos = s.scopeRepoSet(sc)
		if len(scopeRepos) == 0 {
			return ResolvedScope{}, fmt.Errorf("saved scope %q names no repositories", scopeArg)
		}
	}

	if sessWS, sessProj, bound := s.sessionScope(ctx); bound {
		return s.resolveBoundSessionScope(ctx, sessWS, sessProj, workspaceArg, project, repo, ref, scopeArg, scopeRepos, intent, explicitNarrowing)
	}
	return s.resolveUnboundScope(ctx, workspaceArg, project, repo, ref, scopeArg, scopeRepos, intent, explicitNarrowing)
}

func normalizeToolIntent(intent ToolIntent, toolName string) ToolIntent {
	switch intent {
	case IntentLocate, IntentReach, IntentAnalyze:
		return intent
	default:
		return toolIntentForName(toolName)
	}
}

func (s *Server) resolveBoundSessionScope(ctx context.Context, sessWS, sessProj, workspaceArg, project, repo, ref, scopeArg string, scopeRepos map[string]bool, intent ToolIntent, explicitNarrowing bool) (ResolvedScope, error) {
	resolved := ResolvedScope{
		WorkspaceID: sessWS,
		ProjectID:   sessProj,
		Applied:     appliedProjectOrWorkspace(sessProj),
	}
	if project != "" {
		resolved.ProjectID = project
		resolved.Applied = "project:" + project
	}

	// A `workspace` arg may only name the session's own workspace. Any
	// other value is a cross-workspace escape attempt -- reject it
	// outright rather than silently honouring the boundary and
	// returning a confusing empty result.
	if workspaceArg != "" && workspaceArg != sessWS {
		return ResolvedScope{}, fmt.Errorf(
			"workspace %q is outside the active workspace %q; cross-workspace queries are not permitted",
			workspaceArg, sessWS)
	}
	if workspaceArg != "" && project == "" && repo == "" && ref == "" && scopeRepos == nil {
		resolved.ProjectID = ""
		resolved.RepoAllow = nil
		resolved.Applied = "workspace"
		return resolved, nil
	}

	wsRepos := map[string]bool{}
	if s.multiIndexer != nil {
		wsRepos = s.multiIndexer.ReposInWorkspace(sessWS)
	}

	if !explicitNarrowing {
		if s.scopeIntentDefaultsEnabled() {
			return s.applyIntentDefault(ctx, resolved, intent), nil
		}
		// Layer-A compatibility: no explicit narrowing in a bound session
		// stays on the session project, with the repo allow-set clamped to
		// the whole workspace for handlers that filter by repo prefix.
		resolved.RepoAllow = wsRepos
		resolved.Applied = appliedProjectOrWorkspace(resolved.ProjectID)
		return resolved, nil
	}

	// A named scope, intersected with the workspace so it can only ever
	// narrow -- a scope is a convenience, never a clamp escape.
	if scopeRepos != nil {
		intersected := intersectRepoSets(scopeRepos, wsRepos)
		if len(intersected) == 0 {
			return ResolvedScope{}, fmt.Errorf(
				"saved scope %q resolves to nothing inside the active workspace %q",
				scopeArg, sessWS)
		}
		resolved.ProjectID = ""
		resolved.RepoAllow = intersected
		resolved.Applied = "scope:" + scopeArg
		return resolved, nil
	}

	if repo == "*" && project == "" && ref == "" {
		resolved.ProjectID = ""
		resolved.RepoAllow = nil
		resolved.Applied = "workspace"
		return resolved, nil
	}
	if repo == "*" {
		repo = ""
	}

	// Explicit narrowing: resolve the args, then intersect with the
	// workspace so a repo/project/ref arg can never escape it.
	narrowed, err := s.resolveRepoFilterArgs(repo, project, ref, false)
	if err != nil {
		return ResolvedScope{}, err
	}
	if narrowed == nil {
		resolved.ProjectID = project
		resolved.RepoAllow = nil
		resolved.Applied = appliedForExplicit(project, repo, ref, nil)
		return resolved, nil
	}
	intersected := intersectRepoSets(narrowed, wsRepos)
	if len(intersected) == 0 {
		return ResolvedScope{}, fmt.Errorf(
			"repo/project/ref filter resolves to nothing inside the active workspace %q; cross-workspace queries are not permitted",
			sessWS)
	}
	resolved.ProjectID = ""
	if project != "" {
		resolved.ProjectID = project
	}
	resolved.RepoAllow = intersected
	resolved.Applied = appliedForExplicit(project, repo, ref, intersected)
	return resolved, nil
}

func (s *Server) resolveUnboundScope(ctx context.Context, workspaceArg, project, repo, ref, scopeArg string, scopeRepos map[string]bool, intent ToolIntent, explicitNarrowing bool) (ResolvedScope, error) {
	resolved := ResolvedScope{
		WorkspaceID: s.scopeWorkspace,
		ProjectID:   s.scopeProject,
		Applied:     appliedProjectOrWorkspace(s.scopeProject),
	}
	if workspaceArg != "" {
		resolved.WorkspaceID = workspaceArg
		if project == "" {
			resolved.ProjectID = ""
			resolved.Applied = "workspace"
		}
	}
	if project != "" {
		resolved.ProjectID = project
		resolved.Applied = "project:" + project
	}

	if !explicitNarrowing && s.scopeIntentDefaultsEnabled() {
		return s.applyIntentDefault(ctx, resolved, intent), nil
	}

	if scopeRepos != nil {
		resolved.ProjectID = ""
		resolved.RepoAllow = scopeRepos
		resolved.Applied = "scope:" + scopeArg
		return resolved, nil
	}
	if repo == "*" && project == "" && ref == "" {
		resolved.ProjectID = ""
		resolved.RepoAllow = nil
		resolved.Applied = "workspace"
		return resolved, nil
	}
	if repo == "*" {
		repo = ""
	}

	allowed, err := s.resolveRepoFilterArgs(repo, project, ref, !explicitNarrowing)
	if err != nil {
		return ResolvedScope{}, err
	}
	resolved.RepoAllow = allowed
	if repo != "" || project != "" || ref != "" {
		resolved.Applied = appliedForExplicit(project, repo, ref, allowed)
	} else if allowed != nil {
		resolved.Applied = appliedForExplicit(s.activeProject, "", "", allowed)
	}
	return resolved, nil
}

func (s *Server) scopeIntentDefaultsEnabled() bool {
	if s == nil {
		return true
	}
	return s.scopeIntentDefaults
}

func (s *Server) applyIntentDefault(ctx context.Context, resolved ResolvedScope, intent ToolIntent) ResolvedScope {
	resolved.ProjectID = ""
	resolved.RepoAllow = nil
	resolved.Applied = "workspace"
	if intent != IntentLocate {
		return resolved
	}
	repo, _ := s.sessionLocality(ctx)
	if repo == "" {
		return resolved
	}
	resolved.RepoAllow = map[string]bool{repo: true}
	resolved.Applied = "repo:" + repo
	return resolved
}

func appliedProjectOrWorkspace(project string) string {
	if project != "" {
		return "project:" + project
	}
	return "workspace"
}

func appliedForExplicit(project, repo, ref string, allowed map[string]bool) string {
	switch {
	case repo != "" && repo != "*":
		return "repo:" + repo
	case project != "":
		return "project:" + project
	case ref != "":
		return "ref:" + ref
	case len(allowed) == 1:
		for p := range allowed {
			return "repo:" + p
		}
	case len(allowed) > 1:
		return fmt.Sprintf("repos:%d", len(allowed))
	}
	return "workspace"
}

func scopeApplied(scope ResolvedScope) string {
	if scope.Applied != "" {
		return scope.Applied
	}
	if len(scope.RepoAllow) == 1 {
		for repo := range scope.RepoAllow {
			return "repo:" + repo
		}
	}
	if len(scope.RepoAllow) > 1 {
		return fmt.Sprintf("repos:%d", len(scope.RepoAllow))
	}
	return appliedProjectOrWorkspace(scope.ProjectID)
}

func decorateResultWithScope(res *mcp.CallToolResult, scope ResolvedScope) *mcp.CallToolResult {
	if res == nil {
		return nil
	}
	fields := map[string]any{
		"scope_applied":    scopeApplied(scope),
		"scope_widen_hint": `to widen: pass repo:"*" or project:<name> or scope:<name>`,
	}
	if res.Meta == nil {
		res.Meta = mcp.NewMetaFromMap(fields)
		return res
	}
	if res.Meta.AdditionalFields == nil {
		res.Meta.AdditionalFields = map[string]any{}
	}
	for k, v := range fields {
		res.Meta.AdditionalFields[k] = v
	}
	return res
}

func withScopeResult(res *mcp.CallToolResult, err error, scope ResolvedScope) (*mcp.CallToolResult, error) {
	if err != nil {
		return res, err
	}
	return decorateResultWithScope(res, scope), nil
}

func (s *Server) respondScopedJSONOrTOON(ctx context.Context, req mcp.CallToolRequest, payload any, scope ResolvedScope) (*mcp.CallToolResult, error) {
	res, err := s.respondJSONOrTOON(ctx, req, payload)
	return withScopeResult(res, err, scope)
}

func (s *Server) returnScopedSubGraph(ctx context.Context, req mcp.CallToolRequest, sg *query.SubGraph, scope ResolvedScope) (*mcp.CallToolResult, error) {
	res, err := s.returnSubGraph(ctx, req, sg)
	return withScopeResult(res, err, scope)
}

func intersectRepoSets(a, b map[string]bool) map[string]bool {
	out := make(map[string]bool)
	for p := range a {
		if b[p] {
			out[p] = true
		}
	}
	return out
}
