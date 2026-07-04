package indexer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/parser"
)

// TestWatcher_InertSkipPersistsMtimeToStore is the sqlite-backed sibling of
// TestWatcher_InertSkipRefreshesMtime. That test proves the in-memory
// fileMtimes map advances past an inert save; this one proves the advance
// also reaches the store's FileMtime sidecar, which is what a warm restart
// actually reads. Before the fix, RefreshFileMtime only restamped the
// in-memory map, so a single inert save during a session left the persisted
// row stale — the next warm restart's HasChangesSinceMtimes saw the file as
// changed and re-tracked the whole repo.
func TestWatcher_InertSkipPersistsMtimeToStore(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	writeTestFile(t, path, `package main

func Steady() {}
`)

	s, err := store_sqlite.Open(filepath.Join(t.TempDir(), "store.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	idx := New(graph.Store(s), newTestRegistry(), config.Default().Index, zap.NewNop())
	idx.SetRootPath(dir)
	_, err = idx.IndexCtx(context.Background(), dir)
	require.NoError(t, err)

	before := s.LoadFileMtimes("")
	require.Contains(t, before, "main.go", "the full index must persist main.go's mtime")

	w, err := NewWatcher(idx, config.WatchConfig{Enabled: true, DebounceMs: 10}, zap.NewNop())
	require.NoError(t, err)

	// Save the file with a strictly later mtime, comment-only — the
	// content-aware skip must treat this as structurally inert.
	future := time.Now().Add(2 * time.Second)
	writeTestFile(t, path, `package main

// a new comment
func Steady() {}
`)
	require.NoError(t, os.Chtimes(path, future, future))
	w.patchGraph(path, ChangeModified)

	info, statErr := os.Stat(path)
	require.NoError(t, statErr)

	after := s.LoadFileMtimes("")
	assert.Equal(t, info.ModTime().UnixNano(), after["main.go"],
		"an inert skip must persist the advanced mtime to the store, not just the in-memory map")
	assert.Greater(t, after["main.go"], before["main.go"],
		"the persisted mtime must advance past the inert save")
}

// alwaysFailExtractor is a synthetic Extractor whose Extract always fails
// after successfully reading the file's bytes — it exercises indexFile's
// result-is-nil branch deterministically, without depending on tree-sitter's
// (rarely triggered) genuine parse-error path.
type alwaysFailExtractor struct{}

func (alwaysFailExtractor) Language() string     { return "gortex-test-brokenlang" }
func (alwaysFailExtractor) Extensions() []string { return []string{".brokenlang"} }
func (alwaysFailExtractor) Extract(_ string, _ []byte) (*parser.ExtractionResult, error) {
	return nil, errors.New("synthetic extraction failure")
}

// TestIndexFile_FailedParsePersistsMtime is the sqlite-backed regression for
// the per-file indexFile gap: a file whose bytes were read successfully but
// failed to parse used to return early without ever recording an mtime, so a
// permanently unparseable file kept its persisted mtime row stale forever —
// the next warm restart's HasChangesSinceMtimes always saw it as changed and
// re-tracked the whole repo. The fix records the file's current on-disk
// mtime even on this failure path.
func TestIndexFile_FailedParsePersistsMtime(t *testing.T) {
	dir := t.TempDir()
	repoPath := filepath.Join(dir, "repo")
	require.NoError(t, os.MkdirAll(repoPath, 0o755))
	badPath := filepath.Join(repoPath, "bad.brokenlang")
	writeFile(t, badPath, "this content is deliberately unparseable by the fake extractor")

	s, err := store_sqlite.Open(filepath.Join(t.TempDir(), "store.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	reg := parser.NewRegistry()
	reg.Register(alwaysFailExtractor{})
	idx := New(graph.Store(s), reg, config.Default().Index, zap.NewNop())
	idx.SetRootPath(repoPath)

	err = idx.IndexFile(badPath)
	require.Error(t, err, "the synthetic extractor must fail to parse")

	info, statErr := os.Stat(badPath)
	require.NoError(t, statErr)

	got := s.LoadFileMtimes("")
	require.Contains(t, got, "bad.brokenlang",
		"a failed-but-readable parse must still get its mtime persisted")
	assert.Equal(t, info.ModTime().UnixNano(), got["bad.brokenlang"])
}
