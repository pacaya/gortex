package mcp

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/query"
)

// buildBarrelIndex indexes a three-file barrel fixture through the real
// extractor + resolver pipeline: a canonical module, a barrel that forwards it,
// and a consumer that imports through the barrel and calls the function.
func buildBarrelIndex(t *testing.T) graph.Store {
	t.Helper()
	dir := t.TempDir()
	write := func(rel, src string) {
		p := filepath.Join(dir, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, []byte(src), 0o644))
	}
	write("src/middleware/persist.ts", "export function persist(config: unknown): unknown {\n  return config;\n}\n")
	write("src/middleware.ts", "export { persist } from './middleware/persist';\n")
	write("src/consumer.ts", "import { persist } from './middleware';\n\nexport function useStore(): unknown {\n  return persist({ name: 'x' });\n}\n")

	g := graph.New()
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	cfg := config.Default()
	idx := indexer.New(g, reg, cfg.Index, zap.NewNop())
	_, err := idx.Index(dir)
	require.NoError(t, err)
	idx.ResolveAll()
	return g
}

func usageFroms(sg *query.SubGraph) map[string]bool {
	out := map[string]bool{}
	for _, e := range sg.Edges {
		out[e.From] = true
	}
	return out
}

func edgeKeys(sg *query.SubGraph) map[string]bool {
	out := map[string]bool{}
	for _, e := range sg.Edges {
		out[e.From+"->"+e.To+":"+string(e.Kind)] = true
	}
	return out
}

// TestBarrelReExport_ResolvesAsNodeWithDelegatedUsages is the end-to-end
// acceptance: the barrel binding resolves as a real node, find_usages on it
// returns the consumer's sites (its own import plus the canonical
// declaration's call via delegation), and the canonical declaration's own
// usages are a strict superset of what they were before the re-export node
// existed (the consumer call stays on canonical; a node-level re-export edge is
// added).
func TestBarrelReExport_ResolvesAsNodeWithDelegatedUsages(t *testing.T) {
	g := buildBarrelIndex(t)

	const (
		barrel     = "src/middleware.ts::persist"
		barrelFile = "src/middleware.ts"
		canonical  = "src/middleware/persist.ts::persist"
		consumer   = "src/consumer.ts"
		consumerFn = "src/consumer.ts::useStore"
	)

	// (a) The public façade binding resolves as a real node carrying the marker.
	bn := g.GetNode(barrel)
	require.NotNil(t, bn, "barrel re-export node %q must exist", barrel)
	require.True(t, isReExportNode(bn), "barrel node must carry the reexport marker")
	require.Equal(t, graph.KindVariable, bn.Kind)
	require.NotNil(t, g.GetNode(canonical), "canonical declaration must exist")

	// The node → canonical forward edge resolves through the import machinery.
	require.Equal(t, canonical, reExportNodeCanonical(g, barrel, 0),
		"barrel node must forward to the canonical declaration")

	eng := query.NewEngine(g)

	// Baseline: the canonical declaration's usages before any delegation. The
	// consumer's *call* binds to the canonical function (the barrel hop keeps
	// import evidence), so this set already contains it.
	canonUsages := eng.FindUsages(canonical)
	require.True(t, usageFroms(canonUsages)[consumerFn],
		"canonical usages must retain the consumer's call site (no regression)")
	// The node-level re-export edge is additive on canonical — a superset.
	require.True(t, edgeKeys(canonUsages)[barrel+"->"+canonical+":"+string(graph.EdgeReExports)],
		"canonical usages must gain the node-level re-export edge")

	// (b) find_usages on the barrel node: its own direct refs — the consumer's
	// per-binding import binds here — merged with the canonical's usages via
	// delegation, exactly as the handler does.
	barrelUsages := eng.FindUsages(barrel)
	require.True(t, usageFroms(barrelUsages)[consumer],
		"barrel node's direct usages must include the consumer's import site")
	require.Equal(t, canonical, reExportNodeCanonical(g, barrel, 0))
	mergeUsageSubGraph(barrelUsages, eng.FindUsages(canonical))

	froms := usageFroms(barrelUsages)
	require.True(t, froms[consumer], "merged usages must include the consumer import site")
	require.True(t, froms[consumerFn], "merged usages must include the consumer call site (delegated)")

	// (c) Delegation is complete: every canonical usage is present in the
	// barrel's merged usage set.
	merged := edgeKeys(barrelUsages)
	for k := range edgeKeys(canonUsages) {
		require.True(t, merged[k], "merged barrel usages missing canonical usage %q", k)
	}
}
