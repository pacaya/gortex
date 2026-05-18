package main

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAllAdapters_RegisteredInStableOrder(t *testing.T) {
	got := allAdapters()
	want := []string{"ripgrep", "probe", "colgrep", "grepai", "coderankembed", "semble"}
	if len(got) != len(want) {
		t.Fatalf("adapter count = %d, want %d", len(got), len(want))
	}
	for i, a := range got {
		if a.Name() != want[i] {
			t.Errorf("adapter[%d] = %q, want %q", i, a.Name(), want[i])
		}
	}
}

func TestAdapterByName(t *testing.T) {
	if a := adapterByName("ripgrep"); a == nil || a.Name() != "ripgrep" {
		t.Errorf("adapterByName(ripgrep) = %v", a)
	}
	if a := adapterByName("RIPGREP"); a == nil { // case-insensitive
		t.Error("adapterByName should be case-insensitive")
	}
	if a := adapterByName("nonexistent"); a != nil {
		t.Errorf("adapterByName(nonexistent) should be nil, got %v", a)
	}
}

func TestResolveAdapters_EmptyMeansAll(t *testing.T) {
	got, err := resolveAdapters("")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(allAdapters()) {
		t.Errorf("empty spec = %d adapters, want %d", len(got), len(allAdapters()))
	}
}

func TestResolveAdapters_Subset(t *testing.T) {
	got, err := resolveAdapters("ripgrep,probe")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Name() != "ripgrep" || got[1].Name() != "probe" {
		t.Errorf("subset = %v, want [ripgrep, probe] in order", names(got))
	}
}

func TestResolveAdapters_UnknownErrors(t *testing.T) {
	if _, err := resolveAdapters("ripgrep,bogus"); err == nil {
		t.Error("expected error for unknown adapter")
	}
}

func TestNDCGAt10_PerfectRanking(t *testing.T) {
	results := []queryResult{
		{Query: "q", Hits: []string{"a", "b", "c"}},
	}
	truth := map[string][]string{"q": {"a", "b"}}
	got := ndcgAt10(results, truth, 10)
	// IDCG = 1/log2(2) + 1/log2(3) = 1 + 0.6309 = 1.6309
	// DCG  = 1/log2(2) + 1/log2(3) + 0 = same = 1.6309
	// NDCG = 1.0
	if got < 0.99 || got > 1.01 {
		t.Errorf("perfect ranking NDCG@10 = %.4f, want 1.0", got)
	}
}

func TestNDCGAt10_WrongOrderPenalized(t *testing.T) {
	results := []queryResult{
		{Query: "q", Hits: []string{"miss1", "miss2", "a", "b"}},
	}
	truth := map[string][]string{"q": {"a", "b"}}
	got := ndcgAt10(results, truth, 10)
	// DCG  = 0 + 0 + 1/log2(4) + 1/log2(5) ≈ 0.5 + 0.431 = 0.931
	// IDCG = 1/log2(2) + 1/log2(3) ≈ 1.0 + 0.631 = 1.631
	// NDCG = 0.931/1.631 ≈ 0.571
	if got > 0.7 {
		t.Errorf("wrong-order NDCG@10 = %.4f, expected <0.7", got)
	}
	if got < 0.4 {
		t.Errorf("wrong-order NDCG@10 = %.4f, expected >0.4", got)
	}
}

func TestNDCGAt10_AllMissesScoreZero(t *testing.T) {
	results := []queryResult{{Query: "q", Hits: []string{"x", "y"}}}
	truth := map[string][]string{"q": {"a", "b"}}
	if got := ndcgAt10(results, truth, 10); got != 0 {
		t.Errorf("all-miss NDCG@10 = %.4f, want 0", got)
	}
}

func TestNDCGAt10_EmptyTruthSkipped(t *testing.T) {
	results := []queryResult{
		{Query: "q1", Hits: []string{"a"}},
		{Query: "q2", Hits: []string{"a"}},
	}
	truth := map[string][]string{"q2": {"a"}}
	// q1 has no truth → skipped; q2 has 1 hit at position 0 → NDCG=1.
	got := ndcgAt10(results, truth, 10)
	if math.Abs(got-1.0) > 0.01 {
		t.Errorf("mixed-truth NDCG@10 = %.4f, want ~1.0 (q1 should be skipped)", got)
	}
}

func TestNDCGAt10_ErroredQuerySkipped(t *testing.T) {
	results := []queryResult{
		{Query: "q1", Error: "boom"},
		{Query: "q2", Hits: []string{"a"}},
	}
	truth := map[string][]string{"q1": {"a"}, "q2": {"a"}}
	// q1 errored → skipped; only q2 counts → NDCG=1.
	got := ndcgAt10(results, truth, 10)
	if math.Abs(got-1.0) > 0.01 {
		t.Errorf("errored-skipped NDCG@10 = %.4f, want ~1.0", got)
	}
}

func TestMedianLatency(t *testing.T) {
	results := []queryResult{
		{LatencyMs: 30},
		{LatencyMs: 10},
		{LatencyMs: 20},
	}
	if got := medianLatency(results); got != 20 {
		t.Errorf("median = %.2f, want 20", got)
	}
}

func TestMedianLatency_ErrorsExcluded(t *testing.T) {
	results := []queryResult{
		{LatencyMs: 30, Error: "boom"},
		{LatencyMs: 10},
		{LatencyMs: 20},
	}
	if got := medianLatency(results); got != 20 {
		t.Errorf("median (errors excluded) = %.2f, want 20", got)
	}
}

func TestParseLines_DedupAndCap(t *testing.T) {
	body := `/repo/a.go
/repo/b.go
/repo/a.go
/repo/c.go
/repo/d.go
`
	got := parseLines(body, "/repo", 3)
	want := []string{"a.go", "b.go", "c.go"}
	if len(got) != 3 {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestParseLines_TopKZeroMeansUnlimited(t *testing.T) {
	body := "a\nb\nc\nd\ne\nf\ng\n"
	got := parseLines(body, "", 0)
	if len(got) != 7 {
		t.Errorf("got %d lines, want 7 (topK=0 unlimited)", len(got))
	}
}

func TestRenderMarkdown_HasHeaderAndRows(t *testing.T) {
	rows := []adapterRow{
		{Adapter: "ripgrep", NDCGAt10: 0.45, MedianLatencyMs: 12.3, TotalDuration: "1.2s"},
		{Adapter: "colgrep", Skipped: "python module not importable"},
	}
	md := renderMarkdown(rows)
	for _, want := range []string{
		"# Retrieval-baselines NDCG@10",
		"| ripgrep |",
		"0.450",
		"| colgrep |",
		"skipped:",
		"**Summary:**",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown missing %q\n----\n%s", want, md)
		}
	}
}

func TestSmokeReport_Markdown(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "smoke.md")
	smokeReport([]adapter{&ripgrepAdapter{}, &colgrepAdapter{}}, "markdown", out)
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	for _, want := range []string{
		"# Baseline-adapter smoke check",
		"| ripgrep |",
		"| colgrep |",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("smoke markdown missing %q\n----\n%s", want, body)
		}
	}
}

func TestSmokeReport_JSON(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "smoke.json")
	smokeReport([]adapter{&ripgrepAdapter{}}, "json", out)
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var got []map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("smoke JSON unparseable: %v\n%s", err, raw)
	}
	if len(got) != 1 || got[0]["adapter"] != "ripgrep" {
		t.Errorf("smoke JSON shape wrong: %v", got)
	}
	if _, ok := got[0]["available"]; !ok {
		t.Errorf("smoke JSON missing 'available' field: %v", got)
	}
}

func TestRipgrepAdapter_NoMatchesReturnsEmpty(t *testing.T) {
	a := &ripgrepAdapter{}
	avail, _ := a.Available()
	if !avail {
		t.Skip("rg not installed on this box")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "f.go"), []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out := a.Run(ctx, []string{"definitely_not_in_the_corpus_XYZ"}, dir, 10)
	if len(out) != 1 {
		t.Fatalf("got %d results, want 1", len(out))
	}
	if out[0].Error != "" {
		t.Errorf("no-match should not error, got %q", out[0].Error)
	}
	if len(out[0].Hits) != 0 {
		t.Errorf("no-match should yield empty hits, got %v", out[0].Hits)
	}
}

func TestLoadQueries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "q.json")
	if err := os.WriteFile(path, []byte(`{"queries":["a","b","c"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := loadQueries(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Errorf("got %d, want 3", len(got))
	}
}

func TestLoadGroundTruth(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gt.json")
	body := `{"queries":{"q":["a.go"]}}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := loadGroundTruth(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got["q"]) != 1 || got["q"][0] != "a.go" {
		t.Errorf("got %v, want q:[a.go]", got)
	}
}

func names(adapters []adapter) []string {
	out := make([]string, len(adapters))
	for i, a := range adapters {
		out[i] = a.Name()
	}
	return out
}
