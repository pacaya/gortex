// Package main defines the baseline adapter framework: a single Go
// surface that uniformly invokes external retrieval baselines
// (ripgrep, probe, ColGREP, grepai, CodeRankEmbed Hybrid, semble)
// against a shared query set, then reports per-baseline NDCG@10 +
// latency in a comparable table.
//
// Per-adapter contract:
//
//   - Name()       canonical baseline name, used in CLI / table cols
//   - Available()  cheap probe: does this baseline run on this box?
//   - Run(queries) per-query result list; honest empty-on-failure
//
// Go-native baselines (ripgrep, probe) shell to their respective
// binaries. Python-heavy ones (ColGREP, grepai, CodeRankEmbed) shell
// to `python3 -m <module>` and require an explicit per-package install
// that the harness documents in bench/baselines/README.md but does
// NOT attempt automatically. This keeps `gortex eval baselines
// --smoke` cheap on a fresh CI runner — it reports each baseline's
// availability without trying to install anything.
package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// adapter is the interface every baseline implementation satisfies.
type adapter interface {
	Name() string
	Available() (bool, string)
	Run(ctx context.Context, queries []string, repoRoot string, topK int) []queryResult
}

// queryResult is the per-query outcome from one adapter.
type queryResult struct {
	Query    string        `json:"query"`
	Hits     []string      `json:"hits"`
	Latency  time.Duration `json:"-"`
	LatencyMs float64      `json:"latency_ms"`
	Error    string        `json:"error,omitempty"`
}

// allAdapters returns the canonical adapter set, in stable order.
// Adding a new baseline = adding one line here + one adapter
// implementation. The smoke check uses this list directly so the
// CLI never drifts from the registry.
func allAdapters() []adapter {
	return []adapter{
		&ripgrepAdapter{},
		&probeAdapter{},
		&colgrepAdapter{},
		&grepaiAdapter{},
		&coderankAdapter{},
		&sembleAdapter{},
	}
}

// adapterByName looks up one adapter by canonical name; nil when no
// match (the caller should check before invoking).
func adapterByName(name string) adapter {
	for _, a := range allAdapters() {
		if strings.EqualFold(a.Name(), name) {
			return a
		}
	}
	return nil
}

// --- ripgrep --------------------------------------------------------

type ripgrepAdapter struct{}

func (*ripgrepAdapter) Name() string { return "ripgrep" }

func (*ripgrepAdapter) Available() (bool, string) {
	if _, err := exec.LookPath("rg"); err != nil {
		return false, "rg not on PATH"
	}
	return true, ""
}

func (*ripgrepAdapter) Run(ctx context.Context, queries []string, repoRoot string, topK int) []queryResult {
	out := make([]queryResult, 0, len(queries))
	for _, q := range queries {
		start := time.Now()
		cmd := exec.CommandContext(ctx, "rg", "--files-with-matches", q, repoRoot)
		var buf bytes.Buffer
		cmd.Stdout = &buf
		cmd.Stderr = io.Discard
		err := cmd.Run()
		latency := time.Since(start)
		r := queryResult{Query: q, Latency: latency, LatencyMs: msFrom(latency)}
		if err != nil {
			if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 {
				// rg exit code 1 = no matches; not an error.
				out = append(out, r)
				continue
			}
			r.Error = err.Error()
			out = append(out, r)
			continue
		}
		hits := parseLines(buf.String(), repoRoot, topK)
		r.Hits = hits
		out = append(out, r)
	}
	return out
}

// --- probe ----------------------------------------------------------

type probeAdapter struct{}

func (*probeAdapter) Name() string { return "probe" }

func (*probeAdapter) Available() (bool, string) {
	if _, err := exec.LookPath("probe"); err != nil {
		return false, "probe binary not on PATH (install: cargo install probe-ai)"
	}
	return true, ""
}

func (*probeAdapter) Run(ctx context.Context, queries []string, repoRoot string, topK int) []queryResult {
	out := make([]queryResult, 0, len(queries))
	for _, q := range queries {
		start := time.Now()
		cmd := exec.CommandContext(ctx, "probe", "search", "--paths-only", q, repoRoot)
		var buf bytes.Buffer
		cmd.Stdout = &buf
		cmd.Stderr = io.Discard
		err := cmd.Run()
		latency := time.Since(start)
		r := queryResult{Query: q, Latency: latency, LatencyMs: msFrom(latency)}
		if err != nil {
			r.Error = err.Error()
			out = append(out, r)
			continue
		}
		r.Hits = parseLines(buf.String(), repoRoot, topK)
		out = append(out, r)
	}
	return out
}

// --- ColGREP --------------------------------------------------------

type colgrepAdapter struct{ pythonBaselineAdapter }

func (*colgrepAdapter) Name() string { return "colgrep" }

func (a *colgrepAdapter) Available() (bool, string) {
	return a.pythonModuleAvailable("colgrep",
		"pip install colgrep (requires CUDA-capable GPU for the bundled ONNX model)")
}

func (a *colgrepAdapter) Run(ctx context.Context, queries []string, repoRoot string, topK int) []queryResult {
	return a.runPythonModule(ctx, "colgrep", queries, repoRoot, topK)
}

// --- grepai ---------------------------------------------------------

type grepaiAdapter struct{ pythonBaselineAdapter }

func (*grepaiAdapter) Name() string { return "grepai" }

func (a *grepaiAdapter) Available() (bool, string) {
	return a.pythonModuleAvailable("grepai_cli",
		"pip install grepai-cli")
}

func (a *grepaiAdapter) Run(ctx context.Context, queries []string, repoRoot string, topK int) []queryResult {
	return a.runPythonModule(ctx, "grepai_cli", queries, repoRoot, topK)
}

// --- CodeRankEmbed Hybrid -------------------------------------------

type coderankAdapter struct{ pythonBaselineAdapter }

func (*coderankAdapter) Name() string { return "coderankembed" }

func (a *coderankAdapter) Available() (bool, string) {
	// CodeRankEmbed ships as a model on Hugging Face; the bench
	// invoker is a small Python script we ship in
	// bench/baselines/python/coderankembed_runner.py. The script
	// requires `pip install sentence-transformers transformers
	// torch` and is documented in bench/baselines/README.md.
	return a.pythonModuleAvailable("sentence_transformers",
		"pip install sentence-transformers transformers torch (multi-GB model download on first run)")
}

func (a *coderankAdapter) Run(ctx context.Context, queries []string, repoRoot string, topK int) []queryResult {
	return a.runPythonScript(ctx, "bench/baselines/python/coderankembed_runner.py", queries, repoRoot, topK)
}

// --- semble ---------------------------------------------------------

type sembleAdapter struct{ pythonBaselineAdapter }

func (*sembleAdapter) Name() string { return "semble" }

func (a *sembleAdapter) Available() (bool, string) {
	return a.pythonModuleAvailable("semble",
		"pip install semble")
}

func (a *sembleAdapter) Run(ctx context.Context, queries []string, repoRoot string, topK int) []queryResult {
	return a.runPythonModule(ctx, "semble.cli", queries, repoRoot, topK)
}

// --- shared Python-adapter plumbing ---------------------------------

// pythonBaselineAdapter embeds the common shell-out logic that all
// Python-based baselines reuse. Per-baseline behaviour comes from
// the module name + (optional) wrapper script path.
type pythonBaselineAdapter struct{}

// pythonModuleAvailable runs `python3 -c "import <mod>"`. Returns
// true when the import succeeds; otherwise returns false with the
// install hint so the smoke output is actionable.
func (pythonBaselineAdapter) pythonModuleAvailable(module, installHint string) (bool, string) {
	pythons := []string{"python3", "python"}
	var lastErr string
	for _, py := range pythons {
		if _, err := exec.LookPath(py); err != nil {
			continue
		}
		cmd := exec.Command(py, "-c", "import "+module)
		cmd.Stderr = io.Discard
		if err := cmd.Run(); err == nil {
			return true, ""
		} else {
			lastErr = err.Error()
		}
	}
	if lastErr == "" {
		return false, "python3 not on PATH"
	}
	return false, fmt.Sprintf("python module %q not importable (%s)", module, installHint)
}

// runPythonModule invokes the module's CLI via `python3 -m <module>`,
// forwarding the queries on stdin one per line. Mirrors how most
// Python retrieval baselines we wrap accept input.
func (pythonBaselineAdapter) runPythonModule(ctx context.Context, module string, queries []string, repoRoot string, topK int) []queryResult {
	return pythonBaselineRun(ctx, []string{"-m", module}, queries, repoRoot, topK)
}

// runPythonScript invokes a wrapper script with the same I/O
// convention. The script lives under bench/baselines/python/ and is
// shipped with the harness so each adapter has a known-good entry
// point.
func (pythonBaselineAdapter) runPythonScript(ctx context.Context, scriptPath string, queries []string, repoRoot string, topK int) []queryResult {
	return pythonBaselineRun(ctx, []string{scriptPath}, queries, repoRoot, topK)
}

// pythonBaselineRun is the shared dispatcher: pipes queries to the
// Python process on stdin and parses one-hit-per-line JSON from
// stdout. Each baseline's wrapper is responsible for emitting the
// canonical JSON shape: {"query": "...", "hits": ["path1", ...]}.
func pythonBaselineRun(ctx context.Context, args []string, queries []string, repoRoot string, topK int) []queryResult {
	out := make([]queryResult, 0, len(queries))
	for _, q := range queries {
		start := time.Now()
		// We invoke the script once per query so a per-query
		// failure doesn't drag the whole run. Higher overhead than
		// streaming but fundamentally honest.
		baseArgs := append([]string{}, args...)
		baseArgs = append(baseArgs, "--repo", repoRoot, "--top-k", fmt.Sprintf("%d", topK), "--query", q)
		cmd := exec.CommandContext(ctx, "python3", baseArgs...)
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		err := cmd.Run()
		latency := time.Since(start)
		r := queryResult{Query: q, Latency: latency, LatencyMs: msFrom(latency)}
		if err != nil {
			r.Error = fmt.Sprintf("%v: %s", err, strings.TrimSpace(stderr.String()))
			out = append(out, r)
			continue
		}
		r.Hits = parseLines(stdout.String(), repoRoot, topK)
		out = append(out, r)
	}
	return out
}

// --- helpers --------------------------------------------------------

// parseLines splits the adapter's stdout into one hit per line,
// strips the repo-root prefix so all adapters return repo-relative
// paths, dedups while preserving order, and caps at topK.
func parseLines(s, repoRoot string, topK int) []string {
	seen := map[string]bool{}
	out := []string{}
	for line := range strings.SplitSeq(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		rel := strings.TrimPrefix(line, repoRoot+"/")
		if seen[rel] {
			continue
		}
		seen[rel] = true
		out = append(out, rel)
		if topK > 0 && len(out) >= topK {
			break
		}
	}
	return out
}

func msFrom(d time.Duration) float64 {
	return float64(d.Microseconds()) / 1000.0
}

// adapterNames returns the names in canonical order for table
// rendering / smoke output.
func adapterNames() []string {
	all := allAdapters()
	names := make([]string, len(all))
	for i, a := range all {
		names[i] = a.Name()
	}
	sort.Strings(names)
	return names
}
