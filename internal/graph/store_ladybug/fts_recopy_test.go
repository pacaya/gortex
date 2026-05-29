//go:build ladybug

package store_ladybug

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// TestSymbolFTS_RepeatedPerRepoBulkIsDeterministic exercises the multi-repo
// per-repo re-bulk path of BulkUpsertSymbolFTS: a repo's rows are DELETEd and
// re-COPYed while sibling repos' rows stay in the table, so the COPY targets a
// NON-EMPTY SymbolFTS by design. Pre-fix this hit the same non-deterministic
// "COPY into a non-empty primary-key node table without a hash index is not
// supported" as the SymbolVec path. DROP TABLE is not an option here — it would
// wipe the sibling repos — so the fix must make the non-empty COPY robust.
func TestSymbolFTS_RepeatedPerRepoBulkIsDeterministic(t *testing.T) {
	dir, err := os.MkdirTemp("", "lbug-fts-recopy-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	s, err := Open(filepath.Join(dir, "store.lbug"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	// Cold start: repo alpha into an empty table.
	require.NoError(t, s.BulkUpsertSymbolFTS("alpha", []graph.SymbolFTSItem{
		{NodeID: "alpha/a.go::Alpha", Tokens: "alpha apple"},
	}))
	require.NoError(t, s.BuildSymbolIndex())

	// repo beta: alpha's rows remain, so this COPYs into a non-empty table.
	require.NoError(t, s.BulkUpsertSymbolFTS("beta", []graph.SymbolFTSItem{
		{NodeID: "beta/b.go::Beta", Tokens: "beta banana"},
	}))
	require.NoError(t, s.BuildSymbolIndex())

	// Re-bulk alpha repeatedly: each call deletes only alpha's rows and COPYs
	// them back while beta stays in the table (a non-empty COPY every time).
	for i := 0; i < 30; i++ {
		require.NoErrorf(t, s.BulkUpsertSymbolFTS("alpha", []graph.SymbolFTSItem{
			{NodeID: "alpha/a.go::Alpha", Tokens: "alpha apple"},
		}), "per-repo re-bulk iteration %d hit the COPY-into-non-empty rejection", i)
		require.NoErrorf(t, s.BuildSymbolIndex(), "BuildSymbolIndex iteration %d", i)
	}

	// Both repos must still be searchable: per-repo re-bulk must not wipe the
	// sibling, and alpha must have been re-added.
	beta, err := s.SearchSymbols("banana", 10)
	require.NoError(t, err)
	require.NotEmpty(t, beta, "sibling repo beta must survive alpha's per-repo re-bulk")
	alpha, err := s.SearchSymbols("apple", 10)
	require.NoError(t, err)
	require.NotEmpty(t, alpha, "alpha must be searchable after re-bulk")
}
