package resolver

import (
	"path/filepath"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// CrossRepoStats holds counts from a cross-repo resolution pass.
type CrossRepoStats struct {
	Resolved       int            `json:"resolved"`
	Unresolved     int            `json:"unresolved"`
	CrossRepoEdges int            `json:"cross_repo_edges"`
	ByRepo         map[string]int `json:"by_repo"`
}

// CrossRepoResolver resolves unresolved edges across repository boundaries.
type CrossRepoResolver struct {
	graph *graph.Graph
}

// NewCrossRepo creates a CrossRepoResolver for the given graph.
func NewCrossRepo(g *graph.Graph) *CrossRepoResolver {
	return &CrossRepoResolver{graph: g}
}

// ResolveAll resolves all unresolved edges in the graph, trying same-repo
// matches first, then cross-repo search. Sets Edge.CrossRepo = true for
// cross-repo matches.
func (cr *CrossRepoResolver) ResolveAll() *CrossRepoStats {
	stats := &CrossRepoStats{ByRepo: make(map[string]int)}

	edges := cr.graph.AllEdges()
	for _, e := range edges {
		if !strings.HasPrefix(e.To, unresolvedPrefix) {
			continue
		}
		cr.resolveEdge(e, stats)
	}
	return stats
}

// ResolveForRepo resolves only unresolved edges originating from nodes
// in the specified repository.
func (cr *CrossRepoResolver) ResolveForRepo(repoPrefix string) *CrossRepoStats {
	stats := &CrossRepoStats{ByRepo: make(map[string]int)}

	nodes := cr.graph.GetRepoNodes(repoPrefix)
	for _, n := range nodes {
		edges := cr.graph.GetOutEdges(n.ID)
		for _, e := range edges {
			if !strings.HasPrefix(e.To, unresolvedPrefix) {
				continue
			}
			cr.resolveEdge(e, stats)
		}
	}
	return stats
}

func (cr *CrossRepoResolver) resolveEdge(e *graph.Edge, stats *CrossRepoStats) {
	target := strings.TrimPrefix(e.To, unresolvedPrefix)

	switch {
	case strings.HasPrefix(target, "import::"):
		cr.resolveImport(e, strings.TrimPrefix(target, "import::"), stats)
	case strings.HasPrefix(target, "*."):
		cr.resolveMethodCall(e, strings.TrimPrefix(target, "*."), stats)
	default:
		cr.resolveFunctionCall(e, target, stats)
	}
}

// callerRepoPrefix returns the RepoPrefix of the node that owns the edge's From field.
func (cr *CrossRepoResolver) callerRepoPrefix(e *graph.Edge) string {
	fromNode := cr.graph.GetNode(e.From)
	if fromNode != nil {
		return fromNode.RepoPrefix
	}
	return ""
}

func (cr *CrossRepoResolver) resolveFunctionCall(e *graph.Edge, funcName string, stats *CrossRepoStats) {
	candidates := cr.graph.FindNodesByName(funcName)
	if len(candidates) == 0 {
		stats.Unresolved++
		return
	}

	callerRepo := cr.callerRepoPrefix(e)

	// 1. Prefer same-repo match.
	for _, c := range candidates {
		if (c.Kind == graph.KindFunction || c.Kind == graph.KindMethod) &&
			c.RepoPrefix == callerRepo {
			e.To = c.ID
			stats.Resolved++
			return
		}
	}

	// 2. Cross-repo fallback: first function/method match from any repo.
	for _, c := range candidates {
		if c.Kind == graph.KindFunction || c.Kind == graph.KindMethod {
			e.To = c.ID
			e.CrossRepo = true
			stats.Resolved++
			stats.CrossRepoEdges++
			stats.ByRepo[c.RepoPrefix]++
			return
		}
	}

	stats.Unresolved++
}

func (cr *CrossRepoResolver) resolveImport(e *graph.Edge, importPath string, stats *CrossRepoStats) {
	callerRepo := cr.callerRepoPrefix(e)

	// Look for a package node with matching qualified name.
	node := cr.graph.GetNodeByQualName(importPath)
	if node != nil {
		e.To = node.ID
		if node.RepoPrefix != callerRepo {
			e.CrossRepo = true
			stats.CrossRepoEdges++
			stats.ByRepo[node.RepoPrefix]++
		}
		stats.Resolved++
		return
	}

	// Look for file nodes whose directory matches the import path suffix.
	// Prefer same-repo, then cross-repo.
	candidates := cr.graph.AllNodes()
	var sameRepo *graph.Node
	var crossRepo *graph.Node
	for _, n := range candidates {
		if n.Kind != graph.KindFile {
			continue
		}
		dir := filepath.Dir(n.FilePath)
		if strings.HasSuffix(dir, lastPathComponent(importPath)) || dir == importPath {
			if n.RepoPrefix == callerRepo {
				sameRepo = n
				break
			}
			if crossRepo == nil {
				crossRepo = n
			}
		}
	}

	if sameRepo != nil {
		e.To = sameRepo.ID
		stats.Resolved++
		return
	}
	if crossRepo != nil {
		e.To = crossRepo.ID
		e.CrossRepo = true
		stats.Resolved++
		stats.CrossRepoEdges++
		stats.ByRepo[crossRepo.RepoPrefix]++
		return
	}

	// External/unresolvable import.
	e.To = "external::" + importPath
	stats.Unresolved++
}

func (cr *CrossRepoResolver) resolveMethodCall(e *graph.Edge, methodName string, stats *CrossRepoStats) {
	candidates := cr.graph.FindNodesByName(methodName)
	if len(candidates) == 0 {
		stats.Unresolved++
		return
	}

	callerRepo := cr.callerRepoPrefix(e)

	// 1. Prefer same-repo match.
	for _, c := range candidates {
		if c.Kind == graph.KindMethod && c.RepoPrefix == callerRepo {
			e.To = c.ID
			stats.Resolved++
			return
		}
	}

	// 2. Cross-repo fallback.
	for _, c := range candidates {
		if c.Kind == graph.KindMethod {
			e.To = c.ID
			e.CrossRepo = true
			stats.Resolved++
			stats.CrossRepoEdges++
			stats.ByRepo[c.RepoPrefix]++
			return
		}
	}

	stats.Unresolved++
}
