package store_sqlite

import (
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func ftsRowCount(t *testing.T, s *Store, table, nodeID string) int {
	t.Helper()
	var n int
	q := `SELECT count(*) FROM ` + table + ` WHERE node_id = ?`
	if err := s.db.QueryRow(q, nodeID).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func ftsHits(t *testing.T, s *Store, query string) int {
	t.Helper()
	hits, err := s.SearchSymbols(query, 20)
	if err != nil {
		t.Fatalf("SearchSymbols(%q): %v", query, err)
	}
	return len(hits)
}

// TestUpsertSymbolFTS_ReplacesWithoutDuplicates is the core correctness
// guard for the rowid-map delete: re-upserting a symbol must drop its prior
// row (by docid) and leave exactly one FTS row + one map row. A wrong
// LastInsertId / stale map would leave the old tokens searchable and the
// row count at 2 — so this also proves the FTS5 docid round-trips.
func TestUpsertSymbolFTS_ReplacesWithoutDuplicates(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "fts.sqlite"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	const id = "pkg/x.go::A"
	s.AddNode(mkFnNode(id, "AlphaWidget", "pkg/x.go"))

	// Upsert three times: first insert, then two replacements that each
	// exercise the mapped-entry docid delete.
	for _, tokens := range []string{"alpha widget red", "alpha widget green", "alpha widget blue"} {
		if err := s.UpsertSymbolFTS(id, tokens); err != nil {
			t.Fatalf("UpsertSymbolFTS(%q): %v", tokens, err)
		}
	}

	if got := ftsRowCount(t, s, "symbol_fts", id); got != 1 {
		t.Fatalf("symbol_fts rows for %s = %d, want 1 (duplicate row leaked)", id, got)
	}
	if got := ftsRowCount(t, s, "symbol_fts_rowid", id); got != 1 {
		t.Fatalf("symbol_fts_rowid rows for %s = %d, want 1", id, got)
	}
	// Only the latest tokens are searchable; the superseded ones are gone.
	if got := ftsHits(t, s, "blue"); got != 1 {
		t.Fatalf("search 'blue' (current tokens) = %d hits, want 1", got)
	}
	for _, stale := range []string{"red", "green"} {
		if got := ftsHits(t, s, stale); got != 0 {
			t.Fatalf("search %q (superseded tokens) = %d hits, want 0 (delete missed)", stale, got)
		}
	}
}

// TestBulkUpsertSymbolFTS_MaintainsRowidMap proves the bulk path keeps the
// sidecar in lockstep, and that a follow-up incremental upsert on a
// bulk-loaded symbol still replaces cleanly (no duplicate).
func TestBulkUpsertSymbolFTS_MaintainsRowidMap(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "fts.sqlite"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	s.AddNode(mkFnNode("pkg/x.go::A", "AlphaWidget", "pkg/x.go"))
	s.AddNode(mkFnNode("pkg/x.go::B", "BetaWidget", "pkg/x.go"))
	if err := s.BulkUpsertSymbolFTS("", []graph.SymbolFTSItem{
		{NodeID: "pkg/x.go::A", Tokens: "alpha widget red"},
		{NodeID: "pkg/x.go::B", Tokens: "beta widget red"},
	}); err != nil {
		t.Fatalf("BulkUpsertSymbolFTS: %v", err)
	}

	var mapRows int
	if err := s.db.QueryRow(`SELECT count(*) FROM symbol_fts_rowid`).Scan(&mapRows); err != nil {
		t.Fatalf("count map: %v", err)
	}
	if mapRows != 2 {
		t.Fatalf("symbol_fts_rowid rows = %d, want 2 after bulk", mapRows)
	}

	// Incremental replace on a bulk-loaded symbol — must not duplicate.
	if err := s.UpsertSymbolFTS("pkg/x.go::A", "alpha widget blue"); err != nil {
		t.Fatalf("UpsertSymbolFTS: %v", err)
	}
	if got := ftsRowCount(t, s, "symbol_fts", "pkg/x.go::A"); got != 1 {
		t.Fatalf("symbol_fts rows after incremental replace = %d, want 1", got)
	}
	if got := ftsHits(t, s, "blue"); got != 1 {
		t.Fatalf("search 'blue' = %d, want 1", got)
	}
}

// TestBackfillSymbolFTSRowidMap simulates a database built before the
// sidecar existed (rows in symbol_fts, none in the map) and proves the
// backfill repopulates it so the next incremental upsert replaces cleanly
// instead of leaking a duplicate.
func TestBackfillSymbolFTSRowidMap(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "fts.sqlite"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	const id = "pkg/x.go::A"
	s.AddNode(mkFnNode(id, "AlphaWidget", "pkg/x.go"))

	// Simulate the legacy state: a row in symbol_fts with no map entry.
	if _, err := s.db.Exec(
		`INSERT INTO symbol_fts (node_id, repo_prefix, tokens) VALUES (?, '', ?)`,
		id, "alpha widget red"); err != nil {
		t.Fatalf("seed legacy fts row: %v", err)
	}
	if _, err := s.db.Exec(`DELETE FROM symbol_fts_rowid`); err != nil {
		t.Fatalf("clear map: %v", err)
	}

	if err := backfillSymbolFTSRowidMap(s.db); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if got := ftsRowCount(t, s, "symbol_fts_rowid", id); got != 1 {
		t.Fatalf("map rows after backfill = %d, want 1", got)
	}

	// Now an incremental upsert must replace, not duplicate.
	if err := s.UpsertSymbolFTS(id, "alpha widget blue"); err != nil {
		t.Fatalf("UpsertSymbolFTS: %v", err)
	}
	if got := ftsRowCount(t, s, "symbol_fts", id); got != 1 {
		t.Fatalf("symbol_fts rows after post-backfill replace = %d, want 1 (dup leaked)", got)
	}
	if got := ftsHits(t, s, "red"); got != 0 {
		t.Fatalf("search 'red' (superseded) = %d, want 0", got)
	}
}
