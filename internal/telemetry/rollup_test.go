package telemetry

import (
	"testing"
	"time"
)

func TestBucketFileCount(t *testing.T) {
	cases := []struct {
		n    int
		want string
	}{
		{0, "<100"}, {99, "<100"},
		{100, "100-1k"}, {999, "100-1k"},
		{1000, "1k-10k"}, {9999, "1k-10k"},
		{10000, "10k+"}, {5_000_000, "10k+"},
	}
	for _, c := range cases {
		if got := BucketFileCount(c.n); got != c.want {
			t.Errorf("BucketFileCount(%d) = %q, want %q", c.n, got, c.want)
		}
	}
}

func TestBucketDuration(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{5 * time.Second, "<10s"},
		{10 * time.Second, "10-60s"},
		{59 * time.Second, "10-60s"},
		{time.Minute, "1-5m"},
		{4 * time.Minute, "1-5m"},
		{5 * time.Minute, "5m+"},
		{time.Hour, "5m+"},
	}
	for _, c := range cases {
		if got := BucketDuration(c.d); got != c.want {
			t.Errorf("BucketDuration(%s) = %q, want %q", c.d, got, c.want)
		}
	}
}

func TestRollupAddAllowList(t *testing.T) {
	r := NewRollup(time.Date(2026, 6, 18, 9, 0, 0, 0, time.UTC))
	if r.Day != "2026-06-18" {
		t.Fatalf("Day = %q, want 2026-06-18", r.Day)
	}

	// Allow-listed key counts.
	if !r.Add("mcp_tool_call", "search_symbols") {
		t.Error("allow-listed metric was dropped")
	}
	r.Add("mcp_tool_call", "search_symbols")
	if got := r.Counts["mcp_tool_call:search_symbols"]; got != 2 {
		t.Errorf("dim counter = %d, want 2", got)
	}

	// A key not on the allow-list is dropped entirely.
	if r.Add("secret_path", "/Users/x/private.go") {
		t.Error("non-allow-listed metric must be dropped")
	}
	if len(r.Counts) != 1 {
		t.Errorf("dropped metric leaked into counts: %v", r.Counts)
	}

	// A path-like dimension on an allowed key is discarded; the bare key
	// still counts (no leak of the path).
	r.Add("index", "/abs/path/with/slashes")
	if _, leaked := r.Counts["index:/abs/path/with/slashes"]; leaked {
		t.Error("path dimension leaked into a counter name")
	}
	if got := r.Counts["index"]; got != 1 {
		t.Errorf("bare key counter = %d, want 1", got)
	}

	// A bucket-label dimension passes.
	r.Add("index", BucketFileCount(5000))
	if got := r.Counts["index:1k-10k"]; got != 1 {
		t.Errorf("bucket dim counter = %d, want 1", got)
	}
}

func TestRollupMerge(t *testing.T) {
	day := time.Date(2026, 6, 18, 0, 0, 0, 0, time.UTC)
	a := NewRollup(day)
	a.Add("cli_command", "index")
	b := NewRollup(day)
	b.Add("cli_command", "index")
	b.Add("daemon_session", "sqlite")
	a.Merge(b)
	if got := a.Counts["cli_command:index"]; got != 2 {
		t.Errorf("merged counter = %d, want 2", got)
	}
	if got := a.Counts["daemon_session:sqlite"]; got != 1 {
		t.Errorf("merged new counter = %d, want 1", got)
	}
	if a.Total() != 3 {
		t.Errorf("Total = %d, want 3", a.Total())
	}

	// A mismatched-day merge is ignored (no corruption).
	other := NewRollup(day.AddDate(0, 0, 1))
	other.Add("cli_command", "x")
	a.Merge(other)
	if a.Total() != 3 {
		t.Errorf("mismatched-day merge mutated rollup: Total = %d, want 3", a.Total())
	}
}

func TestIsAllowedMetric(t *testing.T) {
	for _, k := range []string{"mcp_tool_call", "cli_command", "index", "daemon_session"} {
		if !IsAllowedMetric(k) {
			t.Errorf("%q should be allow-listed", k)
		}
	}
	for _, k := range []string{"", "file_path", "user_query", "anything_else"} {
		if IsAllowedMetric(k) {
			t.Errorf("%q must not be allow-listed", k)
		}
	}
}
