package indexer

import (
	"github.com/zzet/gortex/internal/clones"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// cloneSigMetaKey is the Node.Meta key under which a function/method's
// base64-encoded MinHash signature is stored. The graph-wide LSH pass
// reads it back out — keeping the signature on the node makes the pass
// a pure graph walk (no file IO), correct under incremental reindex,
// and safe across multi-repo graphs.
const cloneSigMetaKey = "clone_sig"

// applyCloneSignatures is the per-file half of clone detection. It runs
// inside applyCoverageDomains (gated on the "clones" coverage domain),
// slices each function/method body out of the file source, computes a
// MinHash signature, and stamps it on the node's Meta. Bodies below
// clones.MinTokens normalised tokens produce no signature and are
// silently skipped — they are dominated by boilerplate and would only
// add noise to the LSH buckets.
func applyCloneSignatures(src []byte, result *parser.ExtractionResult) {
	if result == nil || len(result.Nodes) == 0 {
		return
	}
	lines := splitLines(src)
	for _, n := range result.Nodes {
		if n == nil {
			continue
		}
		if n.Kind != graph.KindFunction && n.Kind != graph.KindMethod {
			continue
		}
		body := bodyText(lines, n.StartLine, n.EndLine)
		if body == "" {
			continue
		}
		sig, ok := clones.ComputeSignature(body)
		if !ok {
			continue
		}
		if n.Meta == nil {
			n.Meta = map[string]any{}
		}
		n.Meta[cloneSigMetaKey] = clones.EncodeSignature(sig)
	}
}

// bodyText returns the source spanning [startLine, endLine] (both
// 1-indexed, inclusive) joined by newlines. Returns "" when the range
// is degenerate or out of bounds — the caller then skips the node.
func bodyText(lines []string, startLine, endLine int) string {
	if startLine <= 0 || endLine < startLine {
		return ""
	}
	lo := startLine - 1
	hi := endLine
	if lo >= len(lines) {
		return ""
	}
	if hi > len(lines) {
		hi = len(lines)
	}
	out := lines[lo]
	for i := lo + 1; i < hi; i++ {
		out += "\n" + lines[i]
	}
	return out
}

// detectClonesAndEmitEdges is the graph-wide half of clone detection.
// It collects every function/method node carrying a clone_sig, runs
// the MinHash + LSH pass over their signatures, and materialises a
// symmetric pair of EdgeSimilarTo edges for each detected clone pair.
//
// threshold is the Jaccard similarity cutoff; pass 0 to use the
// clones package default. Returns the number of clone pairs found and
// the number of edges emitted (two per pair, modulo graph dedup).
//
// The pass is a full recompute and is idempotent: graph.AddEdge dedupes
// by edgeKey so re-emitting an unchanged pair is a no-op, and stale
// edges cannot survive — when either endpoint's file is reindexed,
// EvictFile removes that node's edges in both directions before this
// pass re-runs.
func detectClonesAndEmitEdges(g *graph.Graph, threshold float64) (pairs int, edges int) {
	if g == nil {
		return 0, 0
	}
	var items []clones.Item
	for _, n := range g.AllNodes() {
		if n == nil || n.Meta == nil {
			continue
		}
		if n.Kind != graph.KindFunction && n.Kind != graph.KindMethod {
			continue
		}
		enc, ok := n.Meta[cloneSigMetaKey].(string)
		if !ok || enc == "" {
			continue
		}
		sig, ok := clones.DecodeSignature(enc)
		if !ok {
			continue
		}
		items = append(items, clones.Item{ID: n.ID, Sig: sig})
	}
	if len(items) < 2 {
		return 0, 0
	}

	detected := clones.DetectPairs(items, threshold)
	for _, p := range detected {
		from := g.GetNode(p.A)
		to := g.GetNode(p.B)
		if from == nil || to == nil {
			continue
		}
		emitSimilarEdge(g, from, to, p.Similarity)
		emitSimilarEdge(g, to, from, p.Similarity)
		edges += 2
	}
	return len(detected), edges
}

// emitSimilarEdge adds one directed EdgeSimilarTo edge carrying the
// estimated Jaccard similarity. The edge is anchored at the source
// node's file/line for locality. Origin is ast_inferred — the
// relationship is a statistical estimate over normalised tokens, not a
// structural fact.
func emitSimilarEdge(g *graph.Graph, from, to *graph.Node, similarity float64) {
	g.AddEdge(&graph.Edge{
		From:       from.ID,
		To:         to.ID,
		Kind:       graph.EdgeSimilarTo,
		FilePath:   from.FilePath,
		Line:       from.StartLine,
		Confidence: similarity,
		Origin:     graph.OriginASTInferred,
		Meta:       map[string]any{"similarity": similarity},
	})
}
