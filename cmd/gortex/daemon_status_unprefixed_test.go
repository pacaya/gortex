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

// TestStatus_SoleRepoDesyncedToPrefixed_ReportsWholeStore reproduces the
// remaining #261/#270 case f8436fab did not cover: a lone repo indexed
// unprefixed (nodes carry repo_prefix="") whose metadata later flips to
// prefixed (Unprefixed=false) WITHOUT its nodes being restamped, leaving an
// empty per-prefix bucket. This is what a macOS path-case duplicate does — a
// second config entry makes willBeMultiRepo true at the next warm restart, but
// migrateLoneUnprefixedRepoCtx's guard (len(repos)==1) is false mid-warmup-loop
// so the "" nodes never migrate. `query stats` still reports the full store,
// while `daemon status` used to fall back to the frozen ~0 RepoMetadata.NodeCount
// and render the reported near-empty (nodes=1) row.
func TestStatus_SoleRepoDesyncedToPrefixed_ReportsWholeStore(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "lib.go"),
		[]byte("package lib\n\nfunc A() {}\nfunc B() { A() }\n"), 0o644))

	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{Repos: []config.RepoEntry{{Path: dir}}}
	gc.SetConfigPath(cfgPath)
	require.NoError(t, gc.Save())
	cm, err := config.NewConfigManager(cfgPath)
	require.NoError(t, err)

	reg := parser.NewRegistry()
	languages.RegisterAll(reg)

	// First index: sole configured repo → indexed unprefixed.
	g := graph.New()
	mi1 := indexer.NewMultiIndexer(g, reg, search.NewBM25(), cm, zap.NewNop())
	_, err = mi1.IndexAll()
	require.NoError(t, err)

	var priorMtimes map[string]int64
	for _, m := range mi1.AllMetadata() {
		require.True(t, m.Unprefixed, "a lone tracked repo must index unprefixed")
		priorMtimes = m.FileMtimes
	}
	require.NotEmpty(t, priorMtimes, "the first index must have recorded file mtimes to reconcile against")
	fullNodes := g.NodeCount()
	fullEdges := g.EdgeCount()
	require.Greater(t, fullNodes, 1, "sanity: the repo must have produced a real graph")

	// A second config entry (the macOS path-case duplicate) makes the next
	// warm restart see a multi-repo config and flip the lone repo to prefixed
	// metadata mode.
	require.NoError(t, cm.Global().AddRepo(config.RepoEntry{Path: t.TempDir()}))
	require.GreaterOrEqual(t, len(cm.Global().Repos), 2)

	// Warm restart: a fresh MultiIndexer over the SAME graph store reconciles
	// the repo from the persisted mtimes. mi2.repos is empty, so
	// migrateLoneUnprefixedRepoCtx is a no-op (guard needs len(repos)==1);
	// willBeMultiRepo is true, so the metadata is stamped prefixed — but with
	// nothing stale to reindex, the existing repo_prefix="" nodes are never
	// restamped, leaving an empty per-prefix bucket.
	mi2 := indexer.NewMultiIndexer(g, reg, search.NewBM25(), cm, zap.NewNop())
	_, err = mi2.ReconcileRepoCtx(context.Background(), config.RepoEntry{Path: dir}, priorMtimes)
	require.NoError(t, err)

	metas := mi2.AllMetadata()
	require.Len(t, metas, 1, "the path-case duplicate collapses to one tracked repo identity")
	var prefix string
	for p, m := range metas {
		prefix = p
		require.False(t, m.Unprefixed, "the reconcile under a 2-entry config flips the repo to prefixed mode")
	}
	require.NotEmpty(t, prefix, "the desynced repo must carry a real (non-empty) prefix")

	// The desync: the store still holds every node under repo_prefix="", so
	// the prefixed bucket is empty — the exact condition that made the old
	// per-prefix lookup miss and fall back to the near-empty frozen count.
	require.Less(t, g.RepoMemoryEstimate(prefix).NodeCount, fullNodes,
		"sanity: the prefixed bucket must be short of the store total — nodes still carry repo_prefix=''")

	c := &realController{graph: g, multiIndexer: mi2, configManager: cm, logger: zap.NewNop()}
	resp, err := c.Status(context.Background())
	require.NoError(t, err)
	require.Len(t, resp.TrackedRepos, 1)

	assert.Equal(t, fullNodes, resp.TrackedRepos[0].Nodes,
		"a sole tracked repo's status row must reflect the whole store even when its nodes are unprefixed but its metadata says prefixed")
	assert.Equal(t, fullEdges, resp.TrackedRepos[0].Edges,
		"edges must likewise reflect the whole store, matching `gortex query stats`")
}
