package savings

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestAddObservation_PerLanguageBucket(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "savings.json")

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}

	s.AddObservation("/repo-a", "go", "get_symbol_source", 100, 200)
	s.AddObservation("/repo-a", "go", "get_symbol_source", 50, 80)
	s.AddObservation("/repo-b", "typescript", "batch_symbols", 30, 70)
	// Empty language is allowed (e.g. record() called with a nil node);
	// it should accumulate in the totals but not in any per-language bucket.
	s.AddObservation("/repo-c", "", "smart_context", 10, 20)

	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	snap := reopened.Snapshot()

	if got, want := snap.Totals.CallsCounted, int64(4); got != want {
		t.Errorf("CallsCounted = %d, want %d", got, want)
	}
	if len(snap.PerLanguage) != 2 {
		t.Errorf("PerLanguage size = %d, want 2 (empty-language observation must not create a bucket)", len(snap.PerLanguage))
	}
	if g := snap.PerLanguage["go"]; g == nil || g.CallsCounted != 2 || g.TokensSaved != 280 {
		t.Errorf("go bucket = %+v, want calls=2 saved=280 (200+80)", g)
	}
	if ts := snap.PerLanguage["typescript"]; ts == nil || ts.CallsCounted != 1 || ts.TokensSaved != 70 {
		t.Errorf("typescript bucket = %+v, want calls=1 saved=70", ts)
	}
}

func TestStartPeriodicFlush_FlushesPendingObservations(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "savings.json")

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}

	stop := s.StartPeriodicFlush(50 * time.Millisecond)
	defer stop()

	s.AddObservation("/some/repo", "", "test", 100, 200)
	if got := s.Snapshot().Totals.CallsCounted; got != 1 {
		t.Fatalf("CallsCounted before flush = %d, want 1", got)
	}

	// Wait for the ticker to fire and write to disk.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected savings file to exist after periodic flush: %v", err)
	}

	// Re-open in a fresh store and confirm the observation persisted.
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := s2.Snapshot().Totals.CallsCounted; got != 1 {
		t.Errorf("re-opened CallsCounted = %d, want 1", got)
	}
}

func TestStartPeriodicFlush_StopsCleanly(t *testing.T) {
	s, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	stop := s.StartPeriodicFlush(10 * time.Millisecond)
	stop()
	stop() // idempotent — should not panic on double-close

	// Calling StartPeriodicFlush again after stop is currently a no-op
	// (see comment on StartPeriodicFlush). This test pins that behavior.
	stop2 := s.StartPeriodicFlush(10 * time.Millisecond)
	stop2()
}

func TestFlush_MergesConcurrentProcessDeltas(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "savings.json")

	// Simulate two independent processes writing to the same savings
	// file. Both Open the empty file (baseline = 0), each adds its own
	// observations, then both Flush. Without merge-on-flush the second
	// flusher overwrites the first; with it, both contributions land.
	a, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}

	a.AddObservation("/repo-a", "", "test", 50, 150)
	a.AddObservation("/repo-a", "", "test", 50, 150)
	b.AddObservation("/repo-b", "", "test", 30, 70)

	if err := a.Flush(); err != nil {
		t.Fatalf("a.Flush: %v", err)
	}
	if err := b.Flush(); err != nil {
		t.Fatalf("b.Flush: %v", err)
	}

	// Re-open and verify merged totals = a's contribution + b's.
	final, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	snap := final.Snapshot()

	if got, want := snap.Totals.CallsCounted, int64(3); got != want {
		t.Errorf("merged CallsCounted = %d, want %d", got, want)
	}
	if got, want := snap.Totals.TokensSaved, int64(150+150+70); got != want {
		t.Errorf("merged TokensSaved = %d, want %d", got, want)
	}
	if got, want := snap.Totals.TokensReturned, int64(50+50+30); got != want {
		t.Errorf("merged TokensReturned = %d, want %d", got, want)
	}
	if len(snap.PerRepo) != 2 {
		t.Errorf("merged PerRepo size = %d, want 2", len(snap.PerRepo))
	}
	if r := snap.PerRepo["/repo-a"]; r == nil || r.CallsCounted != 2 {
		t.Errorf("repo-a calls = %v, want 2", r)
	}
	if r := snap.PerRepo["/repo-b"]; r == nil || r.CallsCounted != 1 {
		t.Errorf("repo-b calls = %v, want 1", r)
	}
}

func TestFlush_FlockBlocksConcurrentWriters(t *testing.T) {
	// Verify the flock is actually held: a second goroutine flushing
	// shouldn't see partial state. We hammer two stores in parallel and
	// expect the final on-disk total to equal the sum of contributions.
	dir := t.TempDir()
	path := filepath.Join(dir, "savings.json")

	const perStore = 200
	stores := make([]*Store, 4)
	for i := range stores {
		s, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		stores[i] = s
	}

	var wg sync.WaitGroup
	for i, s := range stores {
		wg.Add(1)
		go func(s *Store, repo string) {
			defer wg.Done()
			for j := 0; j < perStore; j++ {
				s.AddObservation(repo, "", "test", 1, 10)
			}
			_ = s.Flush()
		}(s, "/repo-"+string(rune('a'+i)))
	}
	wg.Wait()

	final, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	snap := final.Snapshot()
	wantCalls := int64(len(stores) * perStore)
	if got := snap.Totals.CallsCounted; got != wantCalls {
		t.Errorf("merged CallsCounted = %d, want %d (delta lost across processes)", got, wantCalls)
	}
	wantSaved := wantCalls * 10
	if got := snap.Totals.TokensSaved; got != wantSaved {
		t.Errorf("merged TokensSaved = %d, want %d", got, wantSaved)
	}
}

func TestOpen_MissingFile_ReturnsEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "savings.json")

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	snap := s.Snapshot()
	if snap.Totals.CallsCounted != 0 {
		t.Errorf("new store has CallsCounted=%d, want 0", snap.Totals.CallsCounted)
	}
	if snap.Version != schemaVersion {
		t.Errorf("new store version=%d, want %d", snap.Version, schemaVersion)
	}
}

func TestAddObservation_AccumulatesAndPersists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "savings.json")

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for range flushEvery + 5 {
		s.AddObservation("/some/repo", "", "test", 100, 900)
	}
	if err := s.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	// Re-open and verify totals survived the write.
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	snap := s2.Snapshot()
	if got, want := snap.Totals.CallsCounted, int64(flushEvery+5); got != want {
		t.Errorf("CallsCounted = %d, want %d", got, want)
	}
	if got, want := snap.Totals.TokensSaved, int64((flushEvery+5)*900); got != want {
		t.Errorf("TokensSaved = %d, want %d", got, want)
	}
	if got, want := snap.Totals.TokensReturned, int64((flushEvery+5)*100); got != want {
		t.Errorf("TokensReturned = %d, want %d", got, want)
	}
	if len(snap.PerRepo) != 1 {
		t.Errorf("PerRepo size = %d, want 1", len(snap.PerRepo))
	}
}

func TestAddObservation_ConcurrentSafe(t *testing.T) {
	s, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	const workers = 8
	const per = 250
	var wg sync.WaitGroup
	var expectedSaved atomic.Int64
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range per {
				s.AddObservation("", "", "test", 10, 100)
				expectedSaved.Add(100)
			}
		}()
	}
	wg.Wait()

	snap := s.Snapshot()
	if got, want := snap.Totals.CallsCounted, int64(workers*per); got != want {
		t.Errorf("CallsCounted = %d, want %d", got, want)
	}
	if got, want := snap.Totals.TokensSaved, expectedSaved.Load(); got != want {
		t.Errorf("TokensSaved = %d, want %d", got, want)
	}
}

func TestOpen_CorruptFile_IsBackedUp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "savings.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open should recover from corrupt file, got error: %v", err)
	}
	snap := s.Snapshot()
	if snap.Totals.CallsCounted != 0 {
		t.Errorf("corrupt recovery should start fresh, got CallsCounted=%d", snap.Totals.CallsCounted)
	}
	// Backup should exist.
	matches, _ := filepath.Glob(path + ".corrupt-*")
	if len(matches) == 0 {
		t.Errorf("expected a .corrupt-* backup file in %s", dir)
	}
}

func TestReset_ClearsStateAndRemovesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "savings.json")

	s, _ := Open(path)
	s.AddObservation("/r", "", "test", 50, 500)
	_ = s.Flush()

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected file to exist after flush: %v", err)
	}
	if err := s.Reset(); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("expected savings.json removed after reset, got %v", err)
	}
	snap := s.Snapshot()
	if snap.Totals.CallsCounted != 0 {
		t.Errorf("in-memory state should be cleared after reset, got CallsCounted=%d", snap.Totals.CallsCounted)
	}
}

func TestOpen_EmptyPath_InMemoryOnly(t *testing.T) {
	s, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	s.AddObservation("r", "", "test", 10, 100)
	if err := s.Flush(); err != nil {
		t.Errorf("Flush on in-memory store should no-op, got: %v", err)
	}
	snap := s.Snapshot()
	if snap.Totals.CallsCounted != 1 {
		t.Errorf("in-memory store should track, got CallsCounted=%d", snap.Totals.CallsCounted)
	}
}

func TestFile_Schema(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "savings.json")

	s, _ := Open(path)
	s.AddObservation("/repo-a", "", "test", 10, 100)
	_ = s.Flush()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("written file is not JSON: %v\n%s", err, data)
	}
	for _, key := range []string{"version", "first_seen", "last_updated", "totals", "per_repo"} {
		if _, ok := parsed[key]; !ok {
			t.Errorf("missing top-level key %q in persisted file", key)
		}
	}
}
