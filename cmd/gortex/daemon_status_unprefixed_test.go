package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/search"
)

// TestStatus_UnprefixedSoloRepo_ReportsLiveNodeCount is the regression for
// issues #261/#270: a lone tracked repo indexes with RepoPrefix="" (see
// TrackRepoCtx/ReconcileRepoCtx's willBeMultiRepo gate), and both graph
// backends make that empty-prefix bucket invisible to a lookup keyed by the
// repo's real prefix — AllRepoMemoryEstimates on the in-memory backend keys
// its map by literal RepoPrefix (so a "" entry never matches the repo's
// resolved prefix string), and the SQLite backend's GROUP BY excludes
// repo_prefix="" rows outright. Either way, `gortex daemon status` used to
// fall back to whatever RepoMetadata.NodeCount was frozen at by the last
// operation that replaced the metadata entry, which goes stale the moment
// the live graph changes out from under it without retouching mi.repos.
func TestStatus_UnprefixedSoloRepo_ReportsLiveNodeCount(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "lib.go"),
		[]byte("package lib\n\nfunc A() {}\n"), 0o644))

	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{Repos: []config.RepoEntry{{Path: dir}}}
	gc.SetConfigPath(cfgPath)
	require.NoError(t, gc.Save())
	cm, err := config.NewConfigManager(cfgPath)
	require.NoError(t, err)

	g := graph.New()
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	mi := indexer.NewMultiIndexer(g, reg, search.NewBM25(), cm, zap.NewNop())
	_, err = mi.IndexAll()
	require.NoError(t, err)

	metas := mi.AllMetadata()
	require.Len(t, metas, 1, "exactly one repo is configured")
	var prefix string
	var frozenNodes int
	for p, m := range metas {
		prefix = p
		frozenNodes = m.NodeCount
		require.True(t, m.Unprefixed, "a lone tracked repo must index unprefixed")
	}

	// Simulate the live graph growing out from under the frozen
	// RepoMetadata snapshot — e.g. an LSP-enrichment or watcher-driven
	// write that lands nodes without going through a path that re-stamps
	// mi.repos[prefix]. A real node (not a bare struct) so both backends'
	// per-shard/column bookkeeping picks it up identically.
	g.AddNode(&graph.Node{
		ID:       "unprefixed::Extra",
		Kind:     graph.KindFunction,
		Name:     "Extra",
		FilePath: "extra.go",
		Language: "go",
	})

	c := &realController{graph: g, multiIndexer: mi, configManager: cm, logger: zap.NewNop()}
	resp, err := c.Status(context.Background())
	require.NoError(t, err)
	require.Len(t, resp.TrackedRepos, 1)

	// Ground truth: the in-memory backend's per-repo shard counters
	// intentionally skip nodes with no RepoPrefix (shard.repoNodeAdd),
	// so RepoMemoryEstimate("") is always zero there — the whole
	// store's NodeCount is the only accurate read for unprefixed mode,
	// which is also what Indexer.repoNodeEdgeCount falls back to.
	liveNodes := g.NodeCount()
	require.Greater(t, liveNodes, frozenNodes,
		"sanity: the live graph must have outgrown the frozen metadata snapshot for this test to be meaningful")
	assert.Equal(t, liveNodes, resp.TrackedRepos[0].Nodes,
		"a solo (unprefixed) tracked repo's status row must reflect the live graph count, not the frozen RepoMetadata snapshot")
	assert.Equal(t, prefix, resp.TrackedRepos[0].Prefix)
}
