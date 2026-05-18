package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph"
)

func TestPctMs_NearestRank(t *testing.T) {
	// Build [1, 2, …, 100] ms.
	xs := make([]time.Duration, 100)
	for i := range xs {
		xs[i] = time.Duration(i+1) * time.Millisecond
	}
	cases := map[int]float64{
		50: 51.0,  // idx = 50*100/100 = 50 → sorted[50] = 51ms
		95: 96.0,  // idx = 95 → 96ms
		99: 100.0, // idx = 99 → 100ms
	}
	for p, want := range cases {
		got := pctMs(xs, p)
		if got != want {
			t.Errorf("pctMs(%d) = %.2f, want %.2f", p, got, want)
		}
	}
}

func TestPctMs_EmptyReturnsZero(t *testing.T) {
	if got := pctMs(nil, 50); got != 0 {
		t.Errorf("pctMs(nil) = %.2f, want 0", got)
	}
}

func TestPctMs_SingleSampleAllPctReturnIt(t *testing.T) {
	xs := []time.Duration{5 * time.Millisecond}
	for _, p := range []int{50, 95, 99} {
		if got := pctMs(xs, p); got != 5.0 {
			t.Errorf("pctMs(single, %d) = %.2f, want 5.0", p, got)
		}
	}
}

func TestMeanMs(t *testing.T) {
	xs := []time.Duration{
		1 * time.Millisecond,
		2 * time.Millisecond,
		3 * time.Millisecond,
		4 * time.Millisecond,
	}
	if got := meanMs(xs); got != 2.5 {
		t.Errorf("meanMs = %.2f, want 2.5", got)
	}
	if got := meanMs(nil); got != 0 {
		t.Errorf("meanMs(nil) = %.2f, want 0", got)
	}
}

func TestFmtMs_Buckets(t *testing.T) {
	cases := map[float64]string{
		0:      "—",
		0.25:   "0.25ms",
		3.7:    "3.7ms",
		1500:   "1.50s",
		60_000: "60.00s",
	}
	for in, want := range cases {
		if got := fmtMs(in); got != want {
			t.Errorf("fmtMs(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestFilterCalls_KeepsRequestedSubset(t *testing.T) {
	g := graph.New()
	all := defaultCalls(g, 10)
	got := filterCalls(all, []string{"graph_stats", "search_symbols"})
	if len(got) != 2 {
		t.Fatalf("got %d, want 2", len(got))
	}
	gotNames := map[string]bool{}
	for _, c := range got {
		gotNames[c.Tool] = true
	}
	if !gotNames["graph_stats"] || !gotNames["search_symbols"] {
		t.Errorf("got %v, want graph_stats + search_symbols", gotNames)
	}
}

func TestDefaultCalls_PopulatesSubstrate(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "f.go::Sym", Name: "Sym", Kind: graph.KindFunction, FilePath: "f.go"})
	g.AddNode(&graph.Node{ID: "file:f.go", Kind: graph.KindFile, FilePath: "f.go"})

	calls := defaultCalls(g, 50)
	if len(calls) == 0 {
		t.Fatal("defaultCalls returned 0 entries")
	}
	// At least the headline tools must be present.
	want := map[string]bool{
		"graph_stats": true, "search_symbols": true,
		"get_symbol_source": true, "smart_context": true,
	}
	got := map[string]bool{}
	for _, c := range calls {
		got[c.Tool] = true
	}
	for w := range want {
		if !got[w] {
			t.Errorf("default call set missing %q", w)
		}
	}
}

func TestDefaultCalls_SkipsTargetlessSubstrate(t *testing.T) {
	g := graph.New()
	// No function / method nodes → get_symbol_source / get_callers /
	// find_usages should report SkipIfMissing=true.
	calls := defaultCalls(g, 10)
	for _, c := range calls {
		if c.Tool == "get_symbol_source" || c.Tool == "get_callers" || c.Tool == "find_usages" {
			if c.SkipIfMissing == nil || !c.SkipIfMissing(g) {
				t.Errorf("%s should skip when no callable symbols available", c.Tool)
			}
		}
	}
}

func TestRenderMarkdown_HasHeaderAndRows(t *testing.T) {
	rows := []result{
		{Tool: "graph_stats", Iters: 200, P50Ms: 1.2, P95Ms: 4.5, P99Ms: 8.9, MeanMs: 2.0, MaxMs: 12.5},
		{Tool: "smart_context", Iters: 40, P50Ms: 50.0, P95Ms: 200.0, P99Ms: 300.0, MeanMs: 80.0, MaxMs: 350.0},
		{Tool: "skipped_one", Skipped: "no substrate"},
	}
	g := graph.New()
	md := renderMarkdown(rows, "/tmp/repo", g)
	for _, want := range []string{
		"# Daemon-mode MCP-tool latency",
		"| graph_stats |",
		"| smart_context |",
		"| skipped_one |",
		"skipped:",
		"**Summary:**",
		"2/3 tools",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown missing %q\n----\n%s", want, md)
		}
	}
}

func TestRenderCSV_HasHeaderAndRows(t *testing.T) {
	rows := []result{
		{Tool: "graph_stats", Iters: 200, P50Ms: 1.2, P95Ms: 4.5, P99Ms: 8.9},
	}
	out := renderCSV(rows)
	if !strings.HasPrefix(out, "tool,iters,p50_ms,p95_ms,p99_ms,") {
		t.Errorf("CSV header missing or wrong:\n%s", out)
	}
	if !strings.Contains(out, "graph_stats,200,") {
		t.Errorf("CSV body missing graph_stats row:\n%s", out)
	}
}

func TestMustMarshalJSON_RoundTrip(t *testing.T) {
	rows := []result{
		{Tool: "graph_stats", Iters: 100, P95Ms: 5.5, P99Ms: 8.2},
	}
	raw := mustMarshalJSON(rows)
	var got []result
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("round-trip: %v", err)
	}
	if len(got) != 1 || got[0].Tool != "graph_stats" || got[0].P95Ms != 5.5 {
		t.Errorf("round-trip lost data: %+v", got)
	}
}

func TestSummary_AllSkippedYieldsExplicitMessage(t *testing.T) {
	rows := []result{{Tool: "x", Skipped: "y"}, {Tool: "z", Skipped: "w"}}
	got := summary(rows)
	if !strings.Contains(got, "no tools ran") {
		t.Errorf("summary should explicitly say no tools ran, got %q", got)
	}
}

func TestSummary_MedianP95AndP99(t *testing.T) {
	rows := []result{
		{Tool: "a", P95Ms: 1, P99Ms: 2},
		{Tool: "b", P95Ms: 5, P99Ms: 10},
		{Tool: "c", P95Ms: 10, P99Ms: 20},
	}
	got := summary(rows)
	// Median p95 of [1, 5, 10] = 5
	if !strings.Contains(got, "Median p95 across tools: 5.0ms") {
		t.Errorf("summary missing median p95=5.0ms; got:\n%s", got)
	}
}
