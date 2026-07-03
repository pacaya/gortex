package semantic

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// newMarkerStore opens an on-disk SQLite store — the only backend that
// persists the enrichment completion marker (graph.EnrichmentStateStore). A
// memory graph does not implement it, which is what makes the gate a no-op
// there (see TestEnrichAll_MemoryBackendNeverGates).
func newMarkerStore(t *testing.T) *store_sqlite.Store {
	t.Helper()
	s, err := store_sqlite.Open(filepath.Join(t.TempDir(), "store.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// markerManager builds a Manager with one available "test-go" provider whose
// Enrich flips *ran when it runs, so a test can assert whether the marker gate
// skipped the pass before the provider was ever invoked.
func markerManager(t *testing.T) (*Manager, *bool) {
	t.Helper()
	ran := new(bool)
	cfg := Config{
		Enabled: true,
		Providers: []ProviderConfig{
			{Name: "test-go", Languages: []string{"go"}, Priority: 1, Enabled: true},
		},
	}
	mgr := NewManager(cfg, zap.NewNop())
	mgr.RegisterProvider(&mockProvider{
		name:      "test-go",
		languages: []string{"go"},
		available: true,
		enrichFunc: func(g graph.Store, root string) (*EnrichResult, error) {
			*ran = true
			return &EnrichResult{Provider: "test-go", Language: "go", CoveragePercent: 95.0}, nil
		},
	})
	return mgr, ran
}

const markerRepo = "myrepo"

func markerRoots(t *testing.T) map[string]string {
	t.Helper()
	return map[string]string{markerRepo: t.TempDir()}
}

// TestEnrichAll_SkipsWhenMarkerCurrent: a persisted marker whose sha matches
// the threaded sha, on a clean tree, skips the provider's pass entirely.
func TestEnrichAll_SkipsWhenMarkerCurrent(t *testing.T) {
	mgr, ran := markerManager(t)
	g := newMarkerStore(t)
	require.NoError(t, g.SetEnrichmentState(graph.EnrichmentState{
		RepoPrefix: markerRepo, Provider: "test-go", IndexedSHA: "abc123",
	}))

	opts := EnrichOptions{RepoState: map[string]RepoEnrichState{
		markerRepo: {SHA: "abc123", Dirty: false},
	}}
	results, partial, err := mgr.EnrichAll(g, markerRoots(t), opts)
	require.NoError(t, err)
	assert.False(t, *ran, "provider must be skipped when the marker sha matches on a clean tree")
	assert.Empty(t, results)
	assert.False(t, partial[markerRepo])
}

// TestEnrichAll_NoSkipCases: the gate never skips when the sha moved, the tree
// is dirty, no marker row exists, or the caller supplied no sha.
func TestEnrichAll_NoSkipCases(t *testing.T) {
	cases := []struct {
		name    string
		seedSHA string // "" → don't seed a marker row
		optSHA  string
		dirty   bool
	}{
		{"sha moved", "old", "new", false},
		{"dirty tree", "abc", "abc", true},
		{"missing marker", "", "abc", false},
		{"no sha supplied", "abc", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mgr, ran := markerManager(t)
			g := newMarkerStore(t)
			if c.seedSHA != "" {
				require.NoError(t, g.SetEnrichmentState(graph.EnrichmentState{
					RepoPrefix: markerRepo, Provider: "test-go", IndexedSHA: c.seedSHA,
				}))
			}
			opts := EnrichOptions{RepoState: map[string]RepoEnrichState{
				markerRepo: {SHA: c.optSHA, Dirty: c.dirty},
			}}
			results, _, err := mgr.EnrichAll(g, markerRoots(t), opts)
			require.NoError(t, err)
			assert.True(t, *ran, "provider must run (gate must not skip)")
			assert.Len(t, results, 1)
		})
	}
}

// TestEnrichAll_WritesMarkerOnCleanCompletion: a non-partial pass persists the
// completion marker with the threaded sha and the pass's coverage.
func TestEnrichAll_WritesMarkerOnCleanCompletion(t *testing.T) {
	mgr, _ := markerManager(t)
	g := newMarkerStore(t)

	opts := EnrichOptions{RepoState: map[string]RepoEnrichState{
		markerRepo: {SHA: "sha-xyz"},
	}}
	results, partial, err := mgr.EnrichAll(g, markerRoots(t), opts)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.False(t, partial[markerRepo])

	got, found, err := g.GetEnrichmentState(markerRepo, "test-go")
	require.NoError(t, err)
	require.True(t, found, "a clean non-partial completion must persist a marker")
	assert.Equal(t, "sha-xyz", got.IndexedSHA)
	assert.InDelta(t, 95.0, got.Coverage, 0.001)
	assert.NotZero(t, got.CompletedAt)
}

// TestEnrichAll_PartialWritesNoMarker: a partial pass marks the repo
// incomplete and persists NO completion marker (it must not claim the repo is
// fully enriched at this sha).
func TestEnrichAll_PartialWritesNoMarker(t *testing.T) {
	cfg := Config{
		Enabled: true,
		Providers: []ProviderConfig{
			{Name: "test-go", Languages: []string{"go"}, Priority: 1, Enabled: true},
		},
	}
	mgr := NewManager(cfg, zap.NewNop())
	mgr.RegisterProvider(&mockProvider{
		name:      "test-go",
		languages: []string{"go"},
		available: true,
		enrichFunc: func(g graph.Store, root string) (*EnrichResult, error) {
			return &EnrichResult{Provider: "test-go", Language: "go", Partial: true, CoveragePercent: 40}, nil
		},
	})
	g := newMarkerStore(t)

	opts := EnrichOptions{RepoState: map[string]RepoEnrichState{
		markerRepo: {SHA: "sha-xyz"},
	}}
	_, partial, err := mgr.EnrichAll(g, markerRoots(t), opts)
	require.NoError(t, err)
	assert.True(t, partial[markerRepo], "a partial pass marks the repo incomplete")

	_, found, err := g.GetEnrichmentState(markerRepo, "test-go")
	require.NoError(t, err)
	assert.False(t, found, "a partial pass must NOT persist a completion marker")
}

// TestEnrichAll_ForceEnvBypassesMarkerGate: GORTEX_WARMUP_FORCE_ENRICH=1
// forces the pass even when the marker is current.
func TestEnrichAll_ForceEnvBypassesMarkerGate(t *testing.T) {
	t.Setenv("GORTEX_WARMUP_FORCE_ENRICH", "1")
	mgr, ran := markerManager(t)
	g := newMarkerStore(t)
	require.NoError(t, g.SetEnrichmentState(graph.EnrichmentState{
		RepoPrefix: markerRepo, Provider: "test-go", IndexedSHA: "abc123",
	}))

	opts := EnrichOptions{RepoState: map[string]RepoEnrichState{
		markerRepo: {SHA: "abc123"},
	}}
	results, _, err := mgr.EnrichAll(g, markerRoots(t), opts)
	require.NoError(t, err)
	assert.True(t, *ran, "force env must bypass the marker skip gate")
	assert.Len(t, results, 1)
}

// TestEnrichAll_MemoryBackendNeverGates: a backend that does not persist
// enrichment state (the in-memory graph) never gates — the type assertion
// fails, so the provider always runs even with a current-looking sha.
func TestEnrichAll_MemoryBackendNeverGates(t *testing.T) {
	mgr, ran := markerManager(t)
	g := graph.New()

	opts := EnrichOptions{RepoState: map[string]RepoEnrichState{
		markerRepo: {SHA: "abc123"},
	}}
	results, _, err := mgr.EnrichAll(g, markerRoots(t), opts)
	require.NoError(t, err)
	assert.True(t, *ran, "memory backend does not persist markers, so it never gates")
	assert.Len(t, results, 1)
}
