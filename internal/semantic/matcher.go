package semantic

import (
	"path/filepath"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// SymbolMap provides bidirectional mapping between external symbol identifiers
// (SCIP URIs, go/types object IDs, LSP URIs) and Gortex node IDs.
type SymbolMap struct {
	externalToGortex map[string]string
	gortexToExternal map[string]string
}

// NewSymbolMap creates an empty symbol map.
func NewSymbolMap() *SymbolMap {
	return &SymbolMap{
		externalToGortex: make(map[string]string),
		gortexToExternal: make(map[string]string),
	}
}

// Add registers a mapping between an external symbol ID and a Gortex node ID.
func (m *SymbolMap) Add(externalID, gortexID string) {
	m.externalToGortex[externalID] = gortexID
	m.gortexToExternal[gortexID] = externalID
}

// GortexID looks up the Gortex node ID for an external symbol.
func (m *SymbolMap) GortexID(externalID string) (string, bool) {
	id, ok := m.externalToGortex[externalID]
	return id, ok
}

// ExternalID looks up the external symbol ID for a Gortex node.
func (m *SymbolMap) ExternalID(gortexID string) (string, bool) {
	id, ok := m.gortexToExternal[gortexID]
	return id, ok
}

// Size returns the number of mappings.
func (m *SymbolMap) Size() int {
	return len(m.externalToGortex)
}

// MatchNodeByFileLine finds a Gortex node by file path and line number.
// This is the primary matching strategy for SCIP and LSP results.
// It finds the innermost (smallest range) non-file node containing the line.
func MatchNodeByFileLine(g *graph.Graph, filePath string, line int) *graph.Node {
	nodes := g.GetFileNodes(filePath)

	// First: find the innermost node containing this line (smallest range).
	var best *graph.Node
	bestSize := int(^uint(0) >> 1)
	for _, n := range nodes {
		if n.Kind == graph.KindFile || n.Kind == graph.KindImport {
			continue
		}
		if n.StartLine <= line && line <= n.EndLine {
			size := n.EndLine - n.StartLine
			if size < bestSize {
				best = n
				bestSize = size
			}
		}
	}
	if best != nil {
		return best
	}

	// Fallback: find the closest node by start line (within tolerance).
	bestDist := int(^uint(0) >> 1)
	for _, n := range nodes {
		if n.Kind == graph.KindFile || n.Kind == graph.KindImport {
			continue
		}
		dist := abs(n.StartLine - line)
		if dist < bestDist {
			best = n
			bestDist = dist
		}
	}
	if bestDist <= 2 {
		return best
	}
	return nil
}

// MatchNodeByQualName finds a Gortex node by qualified name.
func MatchNodeByQualName(g *graph.Graph, qualName string) *graph.Node {
	return g.GetNodeByQualName(qualName)
}

// MatchNodeByNameInFile finds a Gortex node by name within a specific file.
func MatchNodeByNameInFile(g *graph.Graph, name, filePath string) *graph.Node {
	nodes := g.GetFileNodes(filePath)
	for _, n := range nodes {
		if n.Name == name {
			return n
		}
	}
	return nil
}

// NormalizeFilePath converts an absolute path to a repo-relative path.
func NormalizeFilePath(absPath, repoRoot string) string {
	rel, err := filepath.Rel(repoRoot, absPath)
	if err != nil {
		return absPath
	}
	return filepath.ToSlash(rel)
}

// ParseGortexID extracts the file path and symbol name from a Gortex node ID.
// Gortex IDs have the format "path/to/file.go::SymbolName".
func ParseGortexID(id string) (filePath, symbolName string) {
	idx := strings.LastIndex(id, "::")
	if idx < 0 {
		return id, ""
	}
	return id[:idx], id[idx+2:]
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
