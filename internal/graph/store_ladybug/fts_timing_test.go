//go:build ladybug

package store_ladybug

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

func benchFTSItems(repo string, n int) []graph.SymbolFTSItem {
	items := make([]graph.SymbolFTSItem, n)
	for i := range items {
		items[i] = graph.SymbolFTSItem{
			NodeID: fmt.Sprintf("%s/pkg/f%06d.go::Symbol%06d", repo, i, i),
			Tokens: fmt.Sprintf("symbol%06d handle request parse token alpha beta gamma", i),
		}
	}
	return items
}

// TestFTSBulkStrategyTiming compares three ways to land a repo's FTS corpus
// into SymbolFTS at a realistic row count:
//
//	A  direct COPY into an EMPTY table          (the old fast path / baseline)
//	B  staging table: COPY into temp + MERGE     (the committed fix)
//	C  LOAD FROM '<csv>' MERGE                    (single-query, no temp table)
//
// B and C run into a NON-EMPTY SymbolFTS (a sibling repo seeded first) — the
// per-repo multi-repo scenario that direct COPY (A) cannot serve. Run with:
//
//	go test -tags ladybug -run TestFTSBulkStrategyTiming -v ./internal/graph/store_ladybug/
func TestFTSBulkStrategyTiming(t *testing.T) {
	if testing.Short() {
		t.Skip("timing")
	}
	const n = 20000
	target := benchFTSItems("target", n)

	// fresh store with the target CSV written; optionally seed a sibling repo
	// so the measured load targets a non-empty SymbolFTS.
	setup := func(seedSibling bool) (*Store, string) {
		dir := t.TempDir()
		s, err := Open(filepath.Join(dir, "store.lbug"))
		require.NoError(t, err)
		if seedSibling {
			require.NoError(t, s.BulkUpsertSymbolFTS("sibling", benchFTSItems("sibling", n)))
		}
		csv := filepath.Join(dir, "target.csv")
		require.NoError(t, writeSymbolFTSTSV(csv, target))
		return s, csv
	}
	lit := func(p string) string { return escapeCypherStringLit(p) }

	// A — direct COPY into an empty table (baseline).
	func() {
		s, csv := setup(false)
		defer func() { _ = s.Close() }()
		s.writeMu.Lock()
		defer s.writeMu.Unlock()
		start := time.Now()
		require.NoError(t, runCypherSafe(s, fmt.Sprintf("COPY SymbolFTS FROM '%s' (HEADER=false, DELIM='\\t')", lit(csv))))
		t.Logf("A direct COPY  (empty)      : %8s  for %d rows", time.Since(start).Round(time.Millisecond), n)
	}()

	// B — staging COPY + MERGE into a non-empty table (the committed fix).
	func() {
		s, csv := setup(true)
		defer func() { _ = s.Close() }()
		s.writeMu.Lock()
		defer s.writeMu.Unlock()
		_ = runCypherSafe(s, fmt.Sprintf(`CALL DROP_FTS_INDEX('SymbolFTS', '%s')`, ftsIndexName))
		start := time.Now()
		_ = runCypherSafe(s, `DROP TABLE IF EXISTS SymbolFTSStage`)
		require.NoError(t, runCypherSafe(s, `CREATE NODE TABLE SymbolFTSStage(id STRING, tokens STRING, PRIMARY KEY(id))`))
		require.NoError(t, runCypherSafe(s, fmt.Sprintf("COPY SymbolFTSStage FROM '%s' (HEADER=false, DELIM='\\t')", lit(csv))))
		require.NoError(t, runCypherSafe(s, `MATCH (st:SymbolFTSStage) MERGE (f:SymbolFTS {id: st.id}) SET f.tokens = st.tokens`))
		_ = runCypherSafe(s, `DROP TABLE IF EXISTS SymbolFTSStage`)
		t.Logf("B staging COPY+MERGE (n-e)  : %8s  for %d rows", time.Since(start).Round(time.Millisecond), n)
	}()

	// C — LOAD FROM '<csv>' MERGE into a non-empty table (single query).
	func() {
		s, csv := setup(true)
		defer func() { _ = s.Close() }()
		s.writeMu.Lock()
		defer s.writeMu.Unlock()
		_ = runCypherSafe(s, fmt.Sprintf(`CALL DROP_FTS_INDEX('SymbolFTS', '%s')`, ftsIndexName))
		start := time.Now()
		q := fmt.Sprintf("LOAD FROM '%s' (header=false, delim='\\t') MERGE (f:SymbolFTS {id: column0}) SET f.tokens = column1", lit(csv))
		require.NoError(t, runCypherSafe(s, q), "LOAD FROM ... MERGE")
		t.Logf("C LOAD FROM MERGE (n-e)     : %8s  for %d rows", time.Since(start).Round(time.Millisecond), n)
	}()
}
