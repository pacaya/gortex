package mcp

import (
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search/rerank"
)

// defaultMaxPerFile caps how many results a single source file may
// contribute to the diverse head of a search_symbols result set.
// Three is enough to surface a small cluster of related symbols from
// one file without letting a large file's many definitions crowd out
// every other file.
const defaultMaxPerFile = 3

// diversifyByFile re-orders a reranked result set so no single source
// file dominates the head. The first maxPerFile hits from each file
// keep their reranked positions; once a file reaches the cap its
// further hits are demoted below every not-yet-capped result. Nothing
// is dropped — a query that legitimately wants many symbols from one
// file still gets them, just after the diverse head.
//
// The reorder is stable: input order is preserved within both the
// head and the demoted tail, so the result is byte-identical across
// repeated invocations. The rerank breakdown is permuted in lockstep
// when it lines up with the node slice, so debug output stays
// aligned. A maxPerFile of zero or less returns the inputs untouched.
func diversifyByFile(nodes []*graph.Node, breakdown []*rerank.Candidate, maxPerFile int) ([]*graph.Node, []*rerank.Candidate) {
	if maxPerFile <= 0 || len(nodes) <= 1 {
		return nodes, breakdown
	}

	head := make([]int, 0, len(nodes))
	var tail []int
	seen := make(map[string]int, len(nodes))
	for i, n := range nodes {
		fp := ""
		if n != nil {
			fp = n.FilePath
		}
		// Nodes with no file path can't over-represent a file, so they
		// always stay in the head at their reranked position.
		if fp != "" && seen[fp] >= maxPerFile {
			tail = append(tail, i)
			continue
		}
		if fp != "" {
			seen[fp]++
		}
		head = append(head, i)
	}
	if len(tail) == 0 {
		return nodes, breakdown
	}

	order := append(head, tail...)
	outNodes := make([]*graph.Node, len(order))
	for newIdx, oldIdx := range order {
		outNodes[newIdx] = nodes[oldIdx]
	}
	outBreakdown := breakdown
	if len(breakdown) == len(nodes) {
		outBreakdown = make([]*rerank.Candidate, len(order))
		for newIdx, oldIdx := range order {
			outBreakdown[newIdx] = breakdown[oldIdx]
		}
	}
	return outNodes, outBreakdown
}
