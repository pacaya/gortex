package semantic

import (
	"github.com/zzet/gortex/internal/graph"
)

// ConfirmEdge upgrades an edge's confidence to EXTRACTED and records the semantic source.
func ConfirmEdge(e *graph.Edge, provider string) {
	e.Confidence = 1.0
	e.ConfidenceLabel = "EXTRACTED"
	if e.Meta == nil {
		e.Meta = make(map[string]any)
	}
	e.Meta["semantic_source"] = provider
}

// RefuteEdge removes a false-positive edge from the graph.
// Returns true if the edge was removed.
func RefuteEdge(g *graph.Graph, e *graph.Edge) bool {
	return g.RemoveEdge(e.From, e.To, e.Kind)
}

// AddSemanticEdge adds a new edge discovered by semantic analysis.
func AddSemanticEdge(g *graph.Graph, from, to string, kind graph.EdgeKind, filePath string, line int, provider string) *graph.Edge {
	e := &graph.Edge{
		From:            from,
		To:              to,
		Kind:            kind,
		FilePath:        filePath,
		Line:            line,
		Confidence:      1.0,
		ConfidenceLabel: "EXTRACTED",
		Meta: map[string]any{
			"semantic_source": provider,
		},
	}
	g.AddEdge(e)
	return e
}

// EnrichNodeMeta sets semantic type information on a node.
func EnrichNodeMeta(n *graph.Node, key string, value any, provider string) {
	if n.Meta == nil {
		n.Meta = make(map[string]any)
	}
	n.Meta[key] = value
	n.Meta["semantic_source"] = provider
}

// FindMatchingEdge searches for an existing edge between two nodes of a given kind.
func FindMatchingEdge(g *graph.Graph, from, to string, kind graph.EdgeKind) *graph.Edge {
	edges := g.GetOutEdges(from)
	for _, e := range edges {
		if e.To == to && e.Kind == kind {
			return e
		}
	}
	return nil
}

// FindEdgeByTarget searches for an edge from a node to a target with any kind.
func FindEdgeByTarget(g *graph.Graph, from, to string) *graph.Edge {
	edges := g.GetOutEdges(from)
	for _, e := range edges {
		if e.To == to {
			return e
		}
	}
	return nil
}

// NodesByLanguage returns all nodes in the graph that match the given language.
func NodesByLanguage(g *graph.Graph, language string) []*graph.Node {
	var result []*graph.Node
	for _, n := range g.AllNodes() {
		if n.Language == language {
			result = append(result, n)
		}
	}
	return result
}

// EdgesByLanguage returns all edges whose source node matches the given language.
func EdgesByLanguage(g *graph.Graph, language string) []*graph.Edge {
	var result []*graph.Edge
	for _, e := range g.AllEdges() {
		fromNode := g.GetNode(e.From)
		if fromNode != nil && fromNode.Language == language {
			result = append(result, e)
		}
	}
	return result
}
