// Command baselines runs the per-baseline retrieval comparison:
// dispatch the same query set through each registered adapter,
// compare the per-query hit lists against a shared ground truth,
// and emit a per-adapter NDCG@10 + latency table.
//
// Smoke mode (`--smoke`) skips actual runs and reports which adapters
// are available on the local box — useful from CI to verify wiring
// without paying for the heavy Python baselines.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func main() {
	repo := flag.String("repo", ".", "indexed corpus path")
	queriesPath := flag.String("queries", "bench/baselines/queries.json", "JSON query set")
	truthPath := flag.String("groundtruth", "bench/baselines/groundtruth.json", "JSON per-query expected file paths")
	against := flag.String("against", "ripgrep,probe,colgrep,grepai,coderankembed,semble", "comma-separated adapter names to run (default: all)")
	topK := flag.Int("top-k", 10, "top-K candidates per query (NDCG@10 uses K=10)")
	out := flag.String("out", "", "output table path (default stdout)")
	jsonOut := flag.String("json", "", "optional JSON metrics output")
	format := flag.String("format", "markdown", "markdown | json")
	smoke := flag.Bool("smoke", false, "skip runs; only probe availability of each requested adapter")
	timeout := flag.Duration("timeout", 5*time.Minute, "per-adapter wall-clock cap")
	flag.Parse()

	requested, err := resolveAdapters(*against)
	if err != nil {
		die("%v", err)
	}

	if *smoke {
		smokeReport(requested, *format, *out)
		return
	}

	absRepo, err := filepath.Abs(*repo)
	if err != nil {
		die("repo path: %v", err)
	}
	if _, err := os.Stat(absRepo); err != nil {
		die("repo path: %v", err)
	}

	queries, err := loadQueries(*queriesPath)
	if err != nil {
		die("queries: %v", err)
	}
	truth, err := loadGroundTruth(*truthPath)
	if err != nil {
		die("groundtruth: %v", err)
	}

	rows := make([]adapterRow, 0, len(requested))
	for _, a := range requested {
		fmt.Fprintf(os.Stderr, "[baselines] %s ... ", a.Name())
		avail, why := a.Available()
		if !avail {
			fmt.Fprintf(os.Stderr, "skipped (%s)\n", why)
			rows = append(rows, adapterRow{
				Adapter: a.Name(),
				Skipped: why,
			})
			continue
		}
		t0 := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), *timeout)
		results := a.Run(ctx, queries, absRepo, *topK)
		cancel()
		elapsed := time.Since(t0)

		row := adapterRow{
			Adapter:       a.Name(),
			PerQuery:      results,
			TotalDuration: elapsed.String(),
		}
		row.MedianLatencyMs = medianLatency(results)
		row.NDCGAt10 = ndcgAt10(results, truth, *topK)
		fmt.Fprintf(os.Stderr, "NDCG@10 %.3f · median %.1fms · total %.1fs\n",
			row.NDCGAt10, row.MedianLatencyMs, elapsed.Seconds())
		rows = append(rows, row)
	}

	var primary []byte
	switch strings.ToLower(*format) {
	case "markdown", "md":
		primary = []byte(renderMarkdown(rows))
	case "json":
		primary = mustMarshalJSON(rows)
	default:
		die("unknown --format %q", *format)
	}
	if err := writeOutput(*out, primary); err != nil {
		die("write output: %v", err)
	}
	if *jsonOut != "" {
		if err := writeOutput(*jsonOut, mustMarshalJSON(rows)); err != nil {
			die("write json: %v", err)
		}
	}
}

// adapterRow is the per-baseline outcome with the columns the
// published table cares about.
type adapterRow struct {
	Adapter         string        `json:"adapter"`
	NDCGAt10        float64       `json:"ndcg_at_10"`
	MedianLatencyMs float64       `json:"median_latency_ms"`
	TotalDuration   string        `json:"total_duration"`
	PerQuery        []queryResult `json:"per_query,omitempty"`
	Skipped         string        `json:"skipped,omitempty"`
}

// resolveAdapters expands the --against flag into the requested
// adapter set, preserving the caller's order. Unknown names error
// out so a typo doesn't silently drop a baseline.
func resolveAdapters(spec string) ([]adapter, error) {
	if strings.TrimSpace(spec) == "" {
		return allAdapters(), nil
	}
	var out []adapter
	for tok := range strings.SplitSeq(spec, ",") {
		name := strings.TrimSpace(tok)
		if name == "" {
			continue
		}
		a := adapterByName(name)
		if a == nil {
			return nil, fmt.Errorf("unknown adapter %q (known: %s)",
				name, strings.Join(adapterNames(), ", "))
		}
		out = append(out, a)
	}
	if len(out) == 0 {
		return allAdapters(), nil
	}
	return out, nil
}

// smokeReport prints one row per requested adapter showing
// available / not-available with the install hint. Output respects
// --format; default markdown.
func smokeReport(adapters []adapter, format, out string) {
	if strings.ToLower(format) == "json" {
		rows := make([]map[string]any, 0, len(adapters))
		for _, a := range adapters {
			avail, why := a.Available()
			row := map[string]any{
				"adapter":   a.Name(),
				"available": avail,
			}
			if why != "" {
				row["why"] = why
			}
			rows = append(rows, row)
		}
		raw, _ := json.MarshalIndent(rows, "", "  ")
		_ = writeOutput(out, append(raw, '\n'))
		return
	}
	var b strings.Builder
	fmt.Fprintln(&b, "# Baseline-adapter smoke check")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "| adapter | available | reason |")
	fmt.Fprintln(&b, "|---------|:---------:|--------|")
	for _, a := range adapters {
		avail, why := a.Available()
		mark := "✗"
		if avail {
			mark = "✓"
		}
		if why == "" {
			why = "—"
		}
		fmt.Fprintf(&b, "| %s | %s | %s |\n", a.Name(), mark, why)
	}
	_ = writeOutput(out, []byte(b.String()))
}

// ndcgAt10 computes NDCG@10 across the result set. Per query: gain
// is 1 for ground-truth hits, 0 otherwise; DCG = sum gain_i /
// log2(i+2); IDCG = DCG of the perfect ordering. Mean across
// queries; 0 when the result set is empty.
func ndcgAt10(results []queryResult, truth map[string][]string, k int) float64 {
	if len(results) == 0 {
		return 0
	}
	var sum float64
	counted := 0
	for _, r := range results {
		if r.Error != "" {
			continue
		}
		expected := truth[r.Query]
		if len(expected) == 0 {
			continue
		}
		expSet := map[string]bool{}
		for _, e := range expected {
			expSet[e] = true
		}
		// DCG@k
		var dcg float64
		hits := r.Hits
		if len(hits) > k {
			hits = hits[:k]
		}
		for i, h := range hits {
			if expSet[h] {
				dcg += 1.0 / math.Log2(float64(i+2))
			}
		}
		// IDCG@k: best case is min(len(expected), k) hits at the top.
		idealHits := min(len(expected), k)
		var idcg float64
		for i := range idealHits {
			idcg += 1.0 / math.Log2(float64(i+2))
		}
		if idcg == 0 {
			continue
		}
		sum += dcg / idcg
		counted++
	}
	if counted == 0 {
		return 0
	}
	return sum / float64(counted)
}

func medianLatency(results []queryResult) float64 {
	vs := make([]float64, 0, len(results))
	for _, r := range results {
		if r.Error == "" {
			vs = append(vs, r.LatencyMs)
		}
	}
	if len(vs) == 0 {
		return 0
	}
	sort.Float64s(vs)
	return vs[len(vs)/2]
}

// --- rendering ------------------------------------------------------

func renderMarkdown(rows []adapterRow) string {
	var b strings.Builder
	fmt.Fprintln(&b, "# Retrieval-baselines NDCG@10 + speed")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "_NDCG@10 against the ground-truth set + median per-query latency for each baseline. Adapters reporting `skipped` weren't available on this box; see the install hints in `bench/baselines/README.md`._")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "| adapter | NDCG@10 | median latency | total | status |")
	fmt.Fprintln(&b, "|---------|--------:|---------------:|------:|--------|")
	for _, r := range rows {
		if r.Skipped != "" {
			fmt.Fprintf(&b, "| %s | — | — | — | skipped: %s |\n", r.Adapter, r.Skipped)
			continue
		}
		fmt.Fprintf(&b, "| %s | %.3f | %s | %s | ✓ |\n",
			r.Adapter, r.NDCGAt10, fmtMs(r.MedianLatencyMs), r.TotalDuration)
	}
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, summaryLine(rows))
	return b.String()
}

func summaryLine(rows []adapterRow) string {
	ran := 0
	bestName := ""
	var bestScore float64
	for _, r := range rows {
		if r.Skipped != "" {
			continue
		}
		ran++
		if r.NDCGAt10 > bestScore {
			bestScore = r.NDCGAt10
			bestName = r.Adapter
		}
	}
	if ran == 0 {
		return "_no adapters available — install at least one baseline (see bench/baselines/README.md)_"
	}
	return fmt.Sprintf("**Summary:** %d/%d adapter(s) ran. Best NDCG@10: %s @ %.3f.",
		ran, len(rows), bestName, bestScore)
}

func fmtMs(v float64) string {
	if v == 0 {
		return "—"
	}
	if v < 1 {
		return fmt.Sprintf("%.2fms", v)
	}
	if v < 1000 {
		return fmt.Sprintf("%.1fms", v)
	}
	return fmt.Sprintf("%.2fs", v/1000.0)
}

// --- helpers --------------------------------------------------------

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "baselines: "+format+"\n", args...)
	os.Exit(1)
}

func writeOutput(path string, body []byte) error {
	if path == "" {
		_, err := os.Stdout.Write(body)
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, body, 0o644)
}

func mustMarshalJSON(rows []adapterRow) []byte {
	b, err := json.MarshalIndent(rows, "", "  ")
	if err != nil {
		die("marshal json: %v", err)
	}
	return append(b, '\n')
}

func loadQueries(path string) ([]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Queries []string `json:"queries"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return doc.Queries, nil
}

func loadGroundTruth(path string) (map[string][]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Queries map[string][]string `json:"queries"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if doc.Queries == nil {
		doc.Queries = map[string][]string{}
	}
	return doc.Queries, nil
}
