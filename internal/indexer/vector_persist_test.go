package indexer

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
)

// TestBulkLoad_PersistsVectorsToBackend guards the fix where the embedding
// pass run under the bulk-load shadow swap dropped the vector index on the
// floor. During a bulk index idx.graph points at the in-memory shadow, which
// does not implement graph.VectorSearcher — so the embedded vectors only ever
// reached the in-process HNSW and never the sqlite `vectors` table. They were
// lost on the next daemon restart, forcing a full (paid) re-embed every time.
//
// The fix captures the disk store at the shadow swap (idx.bulkVectorSink) and
// persists the vectors against it. This test indexes a tiny repo into a sqlite
// store with an embedder wired in, then asserts the backend's vectors table is
// populated — it was empty before the fix.
func TestBulkLoad_PersistsVectorsToBackend(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "app.py"), `
def fetch(url):
    return url

def store(value):
    return value
`)

	sqliteDir := t.TempDir()
	store, err := store_sqlite.Open(filepath.Join(sqliteDir, "store.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	// Sanity: sqlite is a BulkLoader (so the shadow swap engages, which is the
	// code path that regressed) and a VectorSearcher (so it CAN persist).
	_, isBulk := graph.Store(store).(graph.BulkLoader)
	require.True(t, isBulk, "sqlite must be a BulkLoader to exercise the shadow swap")
	_, isVec := graph.Store(store).(graph.VectorSearcher)
	require.True(t, isVec, "sqlite must be a VectorSearcher to persist embeddings")

	reg := parser.NewRegistry()
	reg.Register(languages.NewPythonExtractor())
	cfg := config.Default().Index
	cfg.Workers = 2
	idx := New(store, reg, cfg, zap.NewNop())
	idx.SetEmbedder(&poolEmbedder{})

	_, err = idx.IndexCtx(context.Background(), dir)
	require.NoError(t, err)

	var allIDs []string
	for _, n := range store.AllNodes() {
		allIDs = append(allIDs, n.ID)
	}
	require.NotEmpty(t, allIDs, "the index must have produced nodes")

	embs := store.GetEmbeddings(allIDs)
	require.NotEmpty(t, embs,
		"embedded vectors must be persisted to the sqlite backend under the bulk loader "+
			"(regression: vectors lived only in the in-process HNSW and were lost on restart)")
}
