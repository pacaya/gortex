package indexer

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/search"
)

// setupMultiWatcherTest creates a MultiIndexer with two repos, indexes them,
// and returns the MultiWatcher, repo dirs, and cleanup function.
func setupMultiWatcherTest(t *testing.T) (*MultiWatcher, *MultiIndexer, string, string) {
	t.Helper()

	g := graph.New()
	reg := parser.NewRegistry()
	reg.Register(languages.NewGoExtractor())
	cm := newTestConfigManager(t)

	// Create two repo directories.
	repoADir := filepath.Join(t.TempDir(), "repo-a")
	repoBDir := filepath.Join(t.TempDir(), "repo-b")
	require.NoError(t, os.MkdirAll(repoADir, 0o755))
	require.NoError(t, os.MkdirAll(repoBDir, 0o755))

	writeFile(t, filepath.Join(repoADir, "main.go"), `package main

func HelloA() {}
`)
	writeFile(t, filepath.Join(repoBDir, "main.go"), `package main

func HelloB() {}
`)

	// Add repos to config manager so ActiveRepos returns them.
	cm.Global().Repos = []config.RepoEntry{
		{Path: repoADir, Name: "repo-a"},
		{Path: repoBDir, Name: "repo-b"},
	}

	mi := NewMultiIndexer(g, reg, search.NewAuto(), cm, zap.NewNop())
	_, err := mi.IndexAll()
	require.NoError(t, err)

	configs := map[string]config.WatchConfig{
		"repo-a": {
			Enabled:    true,
			DebounceMs: 50,
			Exclude:    []string{"**/*.tmp"},
		},
		"repo-b": {
			Enabled:    true,
			DebounceMs: 50,
			Exclude:    []string{"**/*.tmp"},
		},
	}

	mw, err := NewMultiWatcher(mi, configs, zap.NewNop())
	require.NoError(t, err)

	return mw, mi, repoADir, repoBDir
}

func waitForMultiEvent(t *testing.T, mw *MultiWatcher, timeout time.Duration) GraphChangeEvent {
	t.Helper()
	select {
	case ev := <-mw.Events():
		return ev
	case <-time.After(timeout):
		t.Fatal("timeout waiting for multi-watcher event")
		return GraphChangeEvent{}
	}
}

func TestNewMultiWatcher(t *testing.T) {
	mw, _, _, _ := setupMultiWatcherTest(t)
	defer func() { _ = mw.Stop() }()

	// Should have watchers for both repos.
	assert.Len(t, mw.watchers, 2)
	assert.Contains(t, mw.watchers, "repo-a")
	assert.Contains(t, mw.watchers, "repo-b")
}

func TestNewMultiWatcher_InaccessibleRepo(t *testing.T) {
	g := graph.New()
	reg := parser.NewRegistry()
	reg.Register(languages.NewGoExtractor())
	cm := newTestConfigManager(t)

	// Create one valid repo.
	repoDir := filepath.Join(t.TempDir(), "valid-repo")
	require.NoError(t, os.MkdirAll(repoDir, 0o755))
	writeFile(t, filepath.Join(repoDir, "main.go"), `package main
func Hello() {}
`)

	cm.Global().Repos = []config.RepoEntry{
		{Path: repoDir, Name: "valid"},
	}

	mi := NewMultiIndexer(g, reg, search.NewAuto(), cm, zap.NewNop())
	_, err := mi.IndexAll()
	require.NoError(t, err)

	// Include a config for a non-existent repo prefix — should log warning and continue.
	configs := map[string]config.WatchConfig{
		"valid": {
			Enabled:    true,
			DebounceMs: 50,
		},
		"nonexistent": {
			Enabled:    true,
			DebounceMs: 50,
		},
	}

	mw, err := NewMultiWatcher(mi, configs, zap.NewNop())
	require.NoError(t, err)
	defer func() { _ = mw.Stop() }()

	// Only the valid repo should have a watcher.
	assert.Len(t, mw.watchers, 1)
	assert.Contains(t, mw.watchers, "valid")
}

func TestMultiWatcher_StartStop(t *testing.T) {
	mw, _, _, _ := setupMultiWatcherTest(t)

	require.NoError(t, mw.Start())
	require.NoError(t, mw.Stop())
}

func TestMultiWatcher_Events_FileModify(t *testing.T) {
	mw, mi, repoADir, _ := setupMultiWatcherTest(t)
	require.NoError(t, mw.Start())
	defer func() { _ = mw.Stop() }()

	// Verify initial state.
	require.NotEmpty(t, mi.Graph().FindNodesByName("HelloA"))

	// Modify a file in repo-a.
	writeFile(t, filepath.Join(repoADir, "main.go"), `package main

func ModifiedA() {}
`)

	ev := waitForMultiEvent(t, mw, 3*time.Second)
	assert.Equal(t, ChangeModified, ev.Kind)

	// Graph should reflect the change.
	assert.NotEmpty(t, mi.Graph().FindNodesByName("ModifiedA"))
}

func TestMultiWatcher_Events_MergedFromMultipleRepos(t *testing.T) {
	mw, mi, repoADir, repoBDir := setupMultiWatcherTest(t)
	require.NoError(t, mw.Start())
	defer func() { _ = mw.Stop() }()

	// Modify repo-a.
	writeFile(t, filepath.Join(repoADir, "main.go"), `package main

func ChangedA() {}
`)
	_ = waitForMultiEvent(t, mw, 3*time.Second)

	// Modify repo-b.
	writeFile(t, filepath.Join(repoBDir, "main.go"), `package main

func ChangedB() {}
`)
	_ = waitForMultiEvent(t, mw, 3*time.Second)

	// Both changes should be reflected in the graph.
	assert.NotEmpty(t, mi.Graph().FindNodesByName("ChangedA"))
	assert.NotEmpty(t, mi.Graph().FindNodesByName("ChangedB"))
}

func TestMultiWatcher_AddRepo(t *testing.T) {
	mw, mi, _, _ := setupMultiWatcherTest(t)
	require.NoError(t, mw.Start())
	defer func() { _ = mw.Stop() }()

	// Create a new repo directory.
	newRepoDir := filepath.Join(t.TempDir(), "repo-c")
	require.NoError(t, os.MkdirAll(newRepoDir, 0o755))
	writeFile(t, filepath.Join(newRepoDir, "main.go"), `package main
func HelloC() {}
`)

	// Track the new repo in the multi-indexer first.
	_, err := mi.TrackRepo(config.RepoEntry{Path: newRepoDir, Name: "repo-c"})
	require.NoError(t, err)

	// Add watcher for the new repo.
	err = mw.AddRepo("repo-c", config.WatchConfig{
		Enabled:    true,
		DebounceMs: 50,
	})
	require.NoError(t, err)

	assert.Contains(t, mw.watchers, "repo-c")

	// Modify the new repo and verify events flow.
	writeFile(t, filepath.Join(newRepoDir, "main.go"), `package main
func ModifiedC() {}
`)
	ev := waitForMultiEvent(t, mw, 3*time.Second)
	assert.Equal(t, ChangeModified, ev.Kind)
	assert.NotEmpty(t, mi.Graph().FindNodesByName("ModifiedC"))
}

func TestMultiWatcher_AddRepo_AlreadyExists(t *testing.T) {
	mw, _, _, _ := setupMultiWatcherTest(t)
	defer func() { _ = mw.Stop() }()

	err := mw.AddRepo("repo-a", config.WatchConfig{Enabled: true, DebounceMs: 50})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "already exists")
}

func TestMultiWatcher_RemoveRepo(t *testing.T) {
	mw, _, _, _ := setupMultiWatcherTest(t)
	require.NoError(t, mw.Start())

	err := mw.RemoveRepo("repo-a")
	require.NoError(t, err)
	assert.NotContains(t, mw.watchers, "repo-a")
	assert.Contains(t, mw.watchers, "repo-b")

	// Cleanup remaining.
	_ = mw.Stop()
}

func TestMultiWatcher_RemoveRepo_NotFound(t *testing.T) {
	mw, _, _, _ := setupMultiWatcherTest(t)
	defer func() { _ = mw.Stop() }()

	err := mw.RemoveRepo("nonexistent")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "no watcher")
}

func TestMultiWatcher_EventsChannel(t *testing.T) {
	mw, _, _, _ := setupMultiWatcherTest(t)
	defer func() { _ = mw.Stop() }()

	ch := mw.Events()
	assert.NotNil(t, ch)
}
