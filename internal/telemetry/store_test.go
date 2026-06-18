package telemetry

import (
	"testing"
	"time"
)

func TestStoreRoundTrip(t *testing.T) {
	s := NewStore(t.TempDir())

	// Loading an absent day yields an empty rollup, not an error.
	r, err := s.Load("2026-06-17")
	if err != nil {
		t.Fatalf("Load absent: %v", err)
	}
	if r.Total() != 0 {
		t.Errorf("absent day Total = %d, want 0", r.Total())
	}

	r.Add("mcp_tool_call", "find_usages")
	r.Add("mcp_tool_call", "find_usages")
	r.Add("index", BucketFileCount(250))
	if err := s.Save(r); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := s.Load("2026-06-17")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Counts["mcp_tool_call:find_usages"] != 2 || got.Counts["index:100-1k"] != 1 {
		t.Errorf("round-trip mismatch: %v", got.Counts)
	}
}

func TestStoreMergeAccumulates(t *testing.T) {
	s := NewStore(t.TempDir())
	day := "2026-06-17"

	first := &Rollup{Day: day, Counts: map[string]int{}}
	first.Add("cli_command", "review")
	if err := s.Merge(first); err != nil {
		t.Fatalf("Merge first: %v", err)
	}

	second := &Rollup{Day: day, Counts: map[string]int{}}
	second.Add("cli_command", "review")
	second.Add("daemon_session", "memory")
	if err := s.Merge(second); err != nil {
		t.Fatalf("Merge second: %v", err)
	}

	got, _ := s.Load(day)
	if got.Counts["cli_command:review"] != 2 {
		t.Errorf("merge did not accumulate: %v", got.Counts)
	}
	if got.Counts["daemon_session:memory"] != 1 {
		t.Errorf("merge dropped a new counter: %v", got.Counts)
	}
}

func TestStoreLoadCompleted(t *testing.T) {
	s := NewStore(t.TempDir())
	for _, day := range []string{"2026-06-15", "2026-06-16", "2026-06-17"} {
		r := &Rollup{Day: day, Counts: map[string]int{}}
		r.Add("cli_command", "x")
		if err := s.Save(r); err != nil {
			t.Fatalf("Save %s: %v", day, err)
		}
	}

	// today = 2026-06-17 → only the two strictly-earlier days are completed.
	completed, err := s.LoadCompleted("2026-06-17")
	if err != nil {
		t.Fatalf("LoadCompleted: %v", err)
	}
	if len(completed) != 2 {
		t.Fatalf("completed days = %d, want 2", len(completed))
	}
	if completed[0].Day != "2026-06-15" || completed[1].Day != "2026-06-16" {
		t.Errorf("completed days unsorted/wrong: %s, %s", completed[0].Day, completed[1].Day)
	}

	// Days() lists all three; Delete removes one idempotently.
	days, _ := s.Days()
	if len(days) != 3 {
		t.Errorf("Days = %v, want 3", days)
	}
	if err := s.Delete("2026-06-15"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := s.Delete("2026-06-15"); err != nil {
		t.Fatalf("Delete (missing, must be idempotent): %v", err)
	}
	days, _ = s.Days()
	if len(days) != 2 {
		t.Errorf("after delete Days = %v, want 2", days)
	}
}

func TestStoreDaysEmptyDir(t *testing.T) {
	// A store whose dir was never written must report no days, not error.
	s := NewStore(t.TempDir() + "/never-created")
	days, err := s.Days()
	if err != nil {
		t.Fatalf("Days on absent dir: %v", err)
	}
	if len(days) != 0 {
		t.Errorf("Days = %v, want empty", days)
	}
	// Save then re-list to confirm creation-on-write.
	r := NewRollup(time.Date(2026, 6, 18, 0, 0, 0, 0, time.UTC))
	r.Add("index", "10k+")
	if err := s.Save(r); err != nil {
		t.Fatalf("Save creates dir: %v", err)
	}
	if days, _ := s.Days(); len(days) != 1 {
		t.Errorf("after first Save Days = %v, want 1", days)
	}
}
