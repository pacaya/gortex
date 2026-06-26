package indexer

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// TestGitWatcher_ReconcileUpdatesFreshness is the regression for the
// "repo reported stale after a reconcile event was logged" bug: after a
// commit the git-watcher reconciled the in-memory graph to the new HEAD
// and logged it, but `gortex repos` kept reporting the repo stale. The
// freshness row (repo_index_state) was written only by a FULL index, so
// the incremental reconcile left it pointing at the previous commit —
// only a daemon restart (which re-runs a full index) cleared it.
//
// The fix re-stamps that row at the new HEAD at the end of every
// reconcile. This test proves the persisted IndexedSHA advances to the
// commit the watcher caught up to.
func TestGitWatcher_ReconcileUpdatesFreshness(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available in PATH")
	}

	repoDir := t.TempDir()
	runGit(t, repoDir, "init", "-q", "-b", "main")
	runGit(t, repoDir, "config", "user.email", "test@example.com")
	runGit(t, repoDir, "config", "user.name", "Test")
	runGit(t, repoDir, "config", "commit.gpgsign", "false")

	writeFile(t, filepath.Join(repoDir, "a.go"), "package main\n\nfunc Alpha() {}\n")
	runGit(t, repoDir, "add", ".")
	runGit(t, repoDir, "commit", "-q", "-m", "first")
	firstSHA := gitHead(t, repoDir)

	// A durable backend is required: the in-memory graph is not a
	// RepoIndexStateWriter, so freshness is never persisted there.
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "store.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	reader, ok := graph.Store(store).(graph.RepoIndexStateReader)
	require.True(t, ok, "sqlite store must implement RepoIndexStateReader")

	idx := New(graph.Store(store), newTestRegistry(), config.IndexConfig{Workers: 1}, zap.NewNop())
	idx.SetRepoPrefix("repo")
	idx.SetRootPath(repoDir)
	_, err = idx.IndexCtx(testCtx(), repoDir)
	require.NoError(t, err)

	// Baseline: the full index recorded the first commit.
	st, found, err := reader.GetRepoIndexState("repo")
	require.NoError(t, err)
	require.True(t, found, "full index must persist a freshness row")
	require.Equal(t, firstSHA, st.IndexedSHA, "freshness row must record the indexed commit")

	gw, err := NewGitWatcher(repoDir, idx, zap.NewNop())
	require.NoError(t, err)
	gw.debounce = 50 * time.Millisecond
	drained := make(chan int, 1)
	gw.drained = func(n int) {
		select {
		case drained <- n:
		default:
		}
	}
	require.NoError(t, gw.Start())
	t.Cleanup(func() { _ = gw.Stop() })

	// A second commit advances HEAD on the watched branch.
	writeFile(t, filepath.Join(repoDir, "b.go"), "package main\n\nfunc Beta() {}\n")
	runGit(t, repoDir, "add", ".")
	runGit(t, repoDir, "commit", "-q", "-m", "second")
	secondSHA := gitHead(t, repoDir)
	require.NotEqual(t, firstSHA, secondSHA)

	select {
	case <-drained:
	case <-time.After(10 * time.Second):
		t.Fatal("git watcher did not reconcile within timeout")
	}

	st, found, err = reader.GetRepoIndexState("repo")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, secondSHA, st.IndexedSHA,
		"reconcile must re-stamp the freshness row at the new HEAD")
	assert.False(t, st.Dirty, "working tree is clean after the commit")
}

// gitHead returns the full HEAD commit SHA of a repo, isolated from any
// machine-global git config the same way runGit is.
func gitHead(t *testing.T, dir string) string {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
	)
	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "git rev-parse HEAD: %s", string(out))
	return strings.TrimSpace(string(out))
}
