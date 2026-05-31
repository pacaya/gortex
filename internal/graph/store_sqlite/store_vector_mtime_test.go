package store_sqlite_test

import (
	"math"
	"math/rand"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// openTestStore opens a fresh on-disk SQLite store in a temp dir and
// registers Close as cleanup. (modernc.org/sqlite's ":memory:" gives
// each pooled connection its OWN private database, so the conformance
// suite — and these tests — use an on-disk file shared across the pool.)
func openTestStore(t *testing.T) *store_sqlite.Store {
	t.Helper()
	s, err := store_sqlite.Open(filepath.Join(t.TempDir(), "test.sqlite"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// --- FileMtime persistence -------------------------------------------

func TestSQLiteFileMtimeRoundTrip(t *testing.T) {
	s := openTestStore(t)

	// Single-row writes.
	if err := s.SetFileMtime("repoA", "a/one.go", 100); err != nil {
		t.Fatalf("SetFileMtime: %v", err)
	}
	if err := s.SetFileMtime("repoA", "a/two.go", 200); err != nil {
		t.Fatalf("SetFileMtime: %v", err)
	}

	// Batch write (includes an overwrite of an existing key).
	batch := map[string]int64{
		"a/two.go":   250, // overwrite
		"a/three.go": 300,
		"a/four.go":  400,
	}
	if err := s.BulkSetFileMtimes("repoA", batch); err != nil {
		t.Fatalf("BulkSetFileMtimes: %v", err)
	}

	want := map[string]int64{
		"a/one.go":   100,
		"a/two.go":   250,
		"a/three.go": 300,
		"a/four.go":  400,
	}

	got, err := s.FileMtimes("repoA")
	if err != nil {
		t.Fatalf("FileMtimes: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("FileMtimes(repoA) = %v, want %v", got, want)
	}

	// LoadFileMtimes (the interface method) must agree.
	if loaded := s.LoadFileMtimes("repoA"); !reflect.DeepEqual(loaded, want) {
		t.Fatalf("LoadFileMtimes(repoA) = %v, want %v", loaded, want)
	}

	// Repo isolation: a different prefix is unaffected.
	if err := s.SetFileMtime("repoB", "b/x.go", 999); err != nil {
		t.Fatalf("SetFileMtime repoB: %v", err)
	}
	if got, _ := s.FileMtimes("repoA"); !reflect.DeepEqual(got, want) {
		t.Fatalf("repoA changed after repoB write: %v", got)
	}

	// Unknown repo: FileMtimes returns an empty (non-nil) map;
	// LoadFileMtimes returns nil (the "no data" signal).
	empty, err := s.FileMtimes("nope")
	if err != nil {
		t.Fatalf("FileMtimes(unknown): %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("FileMtimes(unknown) = %v, want empty", empty)
	}
	if loaded := s.LoadFileMtimes("nope"); loaded != nil {
		t.Fatalf("LoadFileMtimes(unknown) = %v, want nil", loaded)
	}

	// Empty batch is a no-op.
	if err := s.BulkSetFileMtimes("repoA", nil); err != nil {
		t.Fatalf("BulkSetFileMtimes(nil): %v", err)
	}
}

// --- Vector search ---------------------------------------------------

// bruteForceCosine ranks corpus against query the long way (exact cosine
// distance, ascending) so the test verifies SimilarTo independently of
// the implementation under test.
func bruteForceCosine(query []float32, corpus map[string][]float32, k int) []string {
	type sc struct {
		id   string
		dist float64
	}
	scored := make([]sc, 0, len(corpus))
	qn := l2(query)
	for id, v := range corpus {
		vn := l2(v)
		if qn == 0 || vn == 0 {
			continue
		}
		var dot float64
		for i := range query {
			dot += float64(query[i]) * float64(v[i])
		}
		scored = append(scored, sc{id: id, dist: 1 - dot/(qn*vn)})
	}
	sort.Slice(scored, func(i, j int) bool {
		if scored[i].dist == scored[j].dist {
			return scored[i].id < scored[j].id // stable tie-break
		}
		return scored[i].dist < scored[j].dist
	})
	out := make([]string, 0, k)
	for i := 0; i < k && i < len(scored); i++ {
		out = append(out, scored[i].id)
	}
	return out
}

func l2(v []float32) float64 {
	var s float64
	for _, f := range v {
		s += float64(f) * float64(f)
	}
	return math.Sqrt(s)
}

func TestSQLiteVectorSimilarTo(t *testing.T) {
	s := openTestStore(t)

	const (
		n    = 50
		dims = 16
	)
	rng := rand.New(rand.NewSource(42))

	corpus := make(map[string][]float32, n)
	items := make([]graph.VectorItem, 0, n)
	var ids []string
	for i := 0; i < n; i++ {
		id := nodeID(i)
		ids = append(ids, id)
		v := make([]float32, dims)
		for d := 0; d < dims; d++ {
			v[d] = float32(rng.NormFloat64())
		}
		corpus[id] = v
		items = append(items, graph.VectorItem{NodeID: id, Vec: v})
	}

	if err := s.BulkUpsertEmbeddings(items); err != nil {
		t.Fatalf("BulkUpsertEmbeddings: %v", err)
	}
	if err := s.BuildVectorIndex(dims); err != nil {
		t.Fatalf("BuildVectorIndex: %v", err)
	}

	// Query == a stored vector → it must rank first at distance ~0.
	queryID := ids[7]
	query := corpus[queryID]

	hits, err := s.SimilarTo(query, 5)
	if err != nil {
		t.Fatalf("SimilarTo: %v", err)
	}
	if len(hits) != 5 {
		t.Fatalf("SimilarTo returned %d hits, want 5", len(hits))
	}
	if hits[0].NodeID != queryID {
		t.Fatalf("top hit = %q, want the query vector %q", hits[0].NodeID, queryID)
	}
	if hits[0].Distance > 1e-6 {
		t.Fatalf("top hit distance = %g, want ~0", hits[0].Distance)
	}

	// Distances must be ascending.
	for i := 1; i < len(hits); i++ {
		if hits[i].Distance < hits[i-1].Distance {
			t.Fatalf("hits not ascending by distance: %v", hits)
		}
	}

	// Independent brute-force ranking must match the returned top-5 ids.
	want := bruteForceCosine(query, corpus, 5)
	gotIDs := make([]string, len(hits))
	for i, h := range hits {
		gotIDs[i] = h.NodeID
	}
	if !reflect.DeepEqual(gotIDs, want) {
		t.Fatalf("SimilarTo top-5 = %v, brute-force = %v", gotIDs, want)
	}

	// Single-add path: a new vector identical to ids[3]'s should be
	// retrievable and rank at distance ~0 for its own query.
	extra := make([]float32, dims)
	copy(extra, corpus[ids[3]])
	if err := s.UpsertEmbedding("extra::node", extra); err != nil {
		t.Fatalf("UpsertEmbedding: %v", err)
	}
	exHits, err := s.SimilarTo(extra, 3)
	if err != nil {
		t.Fatalf("SimilarTo (extra): %v", err)
	}
	if len(exHits) == 0 {
		t.Fatalf("SimilarTo(extra) returned nothing")
	}
	// Either the original ids[3] or the new extra::node (both identical
	// vectors, distance ~0) may sort first; the new one must be present
	// at distance ~0.
	foundExtra := false
	for _, h := range exHits {
		if h.NodeID == "extra::node" {
			foundExtra = true
			if h.Distance > 1e-6 {
				t.Fatalf("extra::node distance = %g, want ~0", h.Distance)
			}
		}
	}
	if !foundExtra {
		t.Fatalf("UpsertEmbedding'd vector not found in SimilarTo results: %v", exHits)
	}
}

func TestSQLiteVectorPersistence(t *testing.T) {
	const dims = 8
	path := filepath.Join(t.TempDir(), "v.sqlite")

	corpus := map[string][]float32{
		"n::1": {1, 0, 0, 0, 0, 0, 0, 0},
		"n::2": {0, 1, 0, 0, 0, 0, 0, 0},
		"n::3": {0, 0, 1, 0, 0, 0, 0, 0},
	}

	// First session: write and close.
	{
		s, err := store_sqlite.Open(path)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		items := make([]graph.VectorItem, 0, len(corpus))
		for id, v := range corpus {
			items = append(items, graph.VectorItem{NodeID: id, Vec: v})
		}
		if err := s.BulkUpsertEmbeddings(items); err != nil {
			t.Fatalf("BulkUpsertEmbeddings: %v", err)
		}
		if err := s.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
	}

	// Second session: reopen, vectors must still be queryable.
	{
		s, err := store_sqlite.Open(path)
		if err != nil {
			t.Fatalf("reopen: %v", err)
		}
		t.Cleanup(func() { _ = s.Close() })

		query := []float32{1, 0, 0, 0, 0, 0, 0, 0}
		hits, err := s.SimilarTo(query, 3)
		if err != nil {
			t.Fatalf("SimilarTo after reopen: %v", err)
		}
		if len(hits) != 3 {
			t.Fatalf("after reopen got %d hits, want 3 (persistence failed)", len(hits))
		}
		if hits[0].NodeID != "n::1" {
			t.Fatalf("after reopen top hit = %q, want n::1", hits[0].NodeID)
		}
		if hits[0].Distance > 1e-6 {
			t.Fatalf("after reopen top distance = %g, want ~0", hits[0].Distance)
		}
	}
}

func nodeID(i int) string {
	const digits = "0123456789"
	if i == 0 {
		return "node::0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{digits[i%10]}, b...)
		i /= 10
	}
	return "node::" + string(b)
}
