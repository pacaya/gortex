package indexer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search"
)

// setupGoRepoWithTypes creates a Go repo with a Handler type and a method —
// enough to exercise multi-repo indexing without needing the Go toolchain
// for semantic enrichment.
func setupGoRepoWithTypes(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "api"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, "go.mod"),
		[]byte("module example.com/"+name+"\n\ngo 1.21\n"),
		0o644,
	))
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, "api", "handler.go"),
		[]byte(`package api

type Handler struct{}

func (h *Handler) CreateTuck() string { return "ok" }
`),
		0o644,
	))
	return dir
}

// TestMultiRepo_ResolvesCallEdges guards against a regression where every
// call edge in multi-repo mode stayed permanently unresolved. applyRepoPrefix
// used to prefix the "unresolved::" sentinel (producing "web/unresolved::X")
// which the resolver's HasPrefix check wouldn't recognize. Symptom was
// universal: get_callers on any function in multi-repo mode returned empty.
// Here: two Go files in two tracked repos, one calls the other. After index,
// the call edge must resolve to the real target ID — not to an "unresolved::"
// placeholder.
func TestMultiRepo_ResolvesCallEdges(t *testing.T) {
	// repo A defines Greet(); repo B's Main() calls Greet — same package name
	// so Go's same-package resolution still has work to do across repos.
	repoA := filepath.Join(t.TempDir(), "lib-svc")
	require.NoError(t, os.MkdirAll(repoA, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(repoA, "greet.go"),
		[]byte("package main\n\nfunc Greet() string { return \"hi\" }\n"),
		0o644))

	repoB := filepath.Join(t.TempDir(), "app-svc")
	require.NoError(t, os.MkdirAll(repoB, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(repoB, "main.go"),
		[]byte("package main\n\nfunc Main() {\n\t_ = Greet()\n}\n"),
		0o644))

	tmpCfg := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{
		Repos: []config.RepoEntry{
			{Path: repoA, Name: "lib-svc"},
			{Path: repoB, Name: "app-svc"},
		},
	}
	gc.SetConfigPath(tmpCfg)
	require.NoError(t, gc.Save())

	cm, err := config.NewConfigManager(tmpCfg)
	require.NoError(t, err)

	g := graph.New()
	mi := NewMultiIndexer(g, newTestRegistry(), search.NewBM25(), cm, zap.NewNop())
	for _, entry := range cm.Global().Repos {
		_, err := mi.TrackRepoCtx(context.Background(), entry)
		require.NoError(t, err, "track %s", entry.Name)
	}

	// Zero edge targets may remain under the unresolved sentinel with any
	// prefix applied. The invariant is stricter than "Main → Greet resolved"
	// because the original bug showed up as contains("unresolved::") for
	// the vast majority of call edges.
	var leaked []string
	for _, e := range g.AllEdges() {
		if strings.Contains(e.To, "/unresolved::") {
			leaked = append(leaked, e.From+" → "+e.To)
		}
	}
	assert.Empty(t, leaked,
		"edges with repo-prefixed unresolved targets indicate applyRepoPrefix "+
			"polluted the resolver sentinel:\n  %s",
		strings.Join(leaked, "\n  "))

	// Positive check: the cross-repo call edge lands on the right target.
	main := "app-svc/main.go::Main"
	greet := "lib-svc/greet.go::Greet"
	found := false
	for _, e := range g.GetOutEdges(main) {
		if e.Kind == graph.EdgeCalls && e.To == greet {
			found = true
			break
		}
	}
	assert.True(t, found,
		"expected resolved EdgeCalls %s → %s; out-edges from Main: %v",
		main, greet, outEdgeSummaries(g, main))
}

func outEdgeSummaries(g *graph.Graph, id string) []string {
	var out []string
	for _, e := range g.GetOutEdges(id) {
		out = append(out, string(e.Kind)+":"+e.To)
	}
	return out
}

// TestTrackRepoCtx_FirstOfManyStillGetsPrefix guards against the bug where
// the first repo tracked via TrackRepoCtx at daemon warmup was indexed
// without a RepoPrefix because willBeMultiRepo was decided by counting
// `mi.repos` (which is empty at iteration 0). The symptom was asymmetric
// IDs across repos: one repo's nodes under "internal/api/handler.go::X",
// the rest under "worker/internal/api/handler.go::X". Halved Go edge
// density in multi-repo graphs.
func TestTrackRepoCtx_FirstOfManyStillGetsPrefix(t *testing.T) {
	repoA := setupGoRepoWithTypes(t, "repo-a")
	repoB := setupGoRepoWithTypes(t, "repo-b")

	tmpCfg := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{
		Repos: []config.RepoEntry{
			{Path: repoA, Name: "repo-a"},
			{Path: repoB, Name: "repo-b"},
		},
	}
	gc.SetConfigPath(tmpCfg)
	require.NoError(t, gc.Save())

	cm, err := config.NewConfigManager(tmpCfg)
	require.NoError(t, err)

	g := graph.New()
	mi := NewMultiIndexer(g, newTestRegistry(), search.NewBM25(), cm, zap.NewNop())

	// Simulate warmupDaemonState's loop: TrackRepoCtx each config'd repo
	// in order. The first call is the one that used to skip prefixing.
	for _, entry := range cm.Global().Repos {
		_, err := mi.TrackRepoCtx(context.Background(), entry)
		require.NoError(t, err, "tracking %s", entry.Name)
	}

	require.True(t, mi.IsMultiRepo(), "setup must produce multi-repo mode")

	// Every node must carry a non-empty RepoPrefix and its FilePath must
	// live under that prefix. Any violation means a code path bypassed
	// applyRepoPrefix.
	var missingPrefix, badFilePaths []string
	for _, n := range g.AllNodes() {
		if n.RepoPrefix == "" {
			missingPrefix = append(missingPrefix, n.ID)
			continue
		}
		if n.FilePath != "" && !strings.HasPrefix(n.FilePath, n.RepoPrefix+"/") {
			badFilePaths = append(badFilePaths,
				n.ID+" (FilePath="+n.FilePath+", RepoPrefix="+n.RepoPrefix+")")
		}
	}
	assert.Empty(t, missingPrefix,
		"nodes without RepoPrefix leaked into multi-repo graph (first-repo prefix bug):\n  %s",
		strings.Join(missingPrefix, "\n  "))
	assert.Empty(t, badFilePaths,
		"nodes with FilePath outside their RepoPrefix:\n  %s",
		strings.Join(badFilePaths, "\n  "))

	// No node ID should begin with an absolute filesystem path — that's
	// the shape stale snapshot nodes take, and no current indexing path
	// should produce it.
	for _, n := range g.AllNodes() {
		assert.False(t, strings.HasPrefix(n.ID, "/"),
			"node ID begins with absolute path: %s", n.ID)
	}

	// Both repos must have contributed Handler.CreateTuck, each under its
	// own prefix. This is the positive counterpart to the prefix check.
	want := map[string]bool{
		"repo-a/api/handler.go::Handler.CreateTuck": false,
		"repo-b/api/handler.go::Handler.CreateTuck": false,
	}
	for _, n := range g.AllNodes() {
		if _, ok := want[n.ID]; ok {
			want[n.ID] = true
		}
	}
	for id, found := range want {
		assert.True(t, found, "expected prefixed node %s not found in graph", id)
	}
}
