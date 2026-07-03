package store_sqlite

import (
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// TestEnrichmentStateRoundTrip: set/get, missing-row, upsert-in-place, and
// per-provider row isolation on the SQLite store.
func TestEnrichmentStateRoundTrip(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "store.sqlite"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	// Missing row → found=false, no error (the "never enriched" signal).
	if _, found, err := s.GetEnrichmentState("repo", "gopls"); err != nil || found {
		t.Fatalf("missing row: found=%v err=%v, want found=false, err=nil", found, err)
	}

	st := graph.EnrichmentState{
		RepoPrefix:  "repo",
		Provider:    "gopls",
		IndexedSHA:  "abc123",
		CompletedAt: 1_700_000_000,
		Coverage:    91.5,
	}
	if err := s.SetEnrichmentState(st); err != nil {
		t.Fatalf("SetEnrichmentState: %v", err)
	}

	got, found, err := s.GetEnrichmentState("repo", "gopls")
	if err != nil || !found {
		t.Fatalf("get after set: found=%v err=%v, want found=true", found, err)
	}
	if got.RepoPrefix != "repo" || got.Provider != "gopls" ||
		got.IndexedSHA != "abc123" || got.CompletedAt != 1_700_000_000 || got.Coverage != 91.5 {
		t.Fatalf("round-trip mismatch: %+v", got)
	}

	// Upsert on (repo_prefix, provider) replaces in place.
	st.IndexedSHA = "def456"
	st.Coverage = 100
	if err := s.SetEnrichmentState(st); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, _, _ = s.GetEnrichmentState("repo", "gopls")
	if got.IndexedSHA != "def456" || got.Coverage != 100 {
		t.Fatalf("upsert did not replace in place: %+v", got)
	}

	// A different provider under the same repo is a distinct row.
	if _, found, _ := s.GetEnrichmentState("repo", "scip-go"); found {
		t.Fatalf("a different provider must be its own row, got found=true")
	}
}

// TestOpenPreservesEnrichmentStateOnReopen proves the new table is picked up
// by an existing store with no wipe: a marker written on the first open
// survives the second open (which re-runs schemaSQL unconditionally) and the
// schema version does not drift. Mirrors TestOpenAtCurrentVersionIsNoOp.
func TestOpenPreservesEnrichmentStateOnReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.sqlite")

	s, err := Open(path)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	if err := s.SetEnrichmentState(graph.EnrichmentState{
		RepoPrefix: "r", Provider: "gopls", IndexedSHA: "sha1", CompletedAt: 42, Coverage: 88,
	}); err != nil {
		t.Fatalf("seed marker: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()

	got, found, err := s2.GetEnrichmentState("r", "gopls")
	if err != nil || !found {
		t.Fatalf("marker lost across reopen: found=%v err=%v (the table must survive a no-op reopen)", found, err)
	}
	if got.IndexedSHA != "sha1" || got.Coverage != 88 {
		t.Fatalf("marker corrupted across reopen: %+v", got)
	}
	if v, _ := readUserVersion(s2.db); v != currentSchemaVersion {
		t.Fatalf("schema version drifted to %d after reopen, want %d", v, currentSchemaVersion)
	}
}
