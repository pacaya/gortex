# Gortex on SWE-bench

This document is the public results template for SWE-bench runs
against gortex's MCP-driven agent. The harness lives at
`cmd/gortex/eval_swebench.go` and `eval/` (the Python side);
running it end-to-end takes multi-day GPU compute on the full
benchmark, so we ship the template + reproducibility instructions
here and update the numbers section after each substantive run.

## Results

**Last run: TBD** — see the "How to reproduce" section to run it
yourself; replace this section with your numbers afterward.

| model | benchmark variant | n_resolved | n_total | resolve_rate | avg tokens | avg runtime |
|-------|-------------------|-----------:|--------:|-------------:|-----------:|------------:|
| TBD   | SWE-bench Lite    | —          | —       | —            | —          | —           |
| TBD   | SWE-bench Verified| —          | —       | —            | —          | —           |
| TBD   | SWE-bench         | —          | —       | —            | —          | —           |

When a row populates: include the exact model name (e.g.
`claude-sonnet-4-20250514`), the harness commit SHA, the run date,
and the model card the per-task prompts use. Append a
`results/swebench/<run-id>/` directory with per-task JSON + the
overall summary so any reviewer can spot-check the count.

## Methodology

Gortex's SWE-bench harness is a thin agent that exposes the same
MCP tool surface as a regular session (`smart_context`, `search_symbols`,
`get_symbol_source`, `edit_file`, `verify_change`, …) and lets the
configured LLM provider drive a turn loop. Per-task budget is the
same token / wall-clock cap as the upstream SWE-bench harness so
results are comparable to other published numbers.

The runner persists per-task outputs to
`results/swebench/<run-id>/<task-id>/` so a failed task can be
re-played without re-running the whole benchmark.

Honest caveats:

- **Compute envelope.** The full SWE-bench (~2300 tasks) takes
  multi-day GPU compute even at modest concurrency; SWE-bench
  Lite (300 tasks) is the practical target for iteration. Don't
  publish "full SWE-bench" numbers without showing the run-time
  cost too.
- **Dataset license.** SWE-bench is community-maintained; check
  the upstream licence before redistributing the per-task
  artifacts.
- **Per-model variance.** Run-to-run variance is non-trivial
  (~2-5 percentage points on resolve rate); a published number
  is one sample, not a confidence interval. Re-run before citing.

## How to reproduce

```sh
# 1) Pre-flight: ensure the harness substrate is in place.
ls eval/                       # Python harness lives here
ls cmd/gortex/eval_swebench.go # Go-side CLI entry

# 2) List available SWE-bench configurations (Lite / Verified /
#    full / custom subsets).
gortex eval swebench --list-configs

# 3) Run a small smoke against SWE-bench Lite, default config.
gortex eval swebench \
    --config swebench-lite \
    --model claude-sonnet-4-20250514 \
    --workdir results/swebench/$(date +%Y%m%d-%H%M%S)/ \
    --max-tasks 5

# 4) Full-config run (multi-day; only do this when you mean it).
gortex eval swebench \
    --config swebench-lite \
    --model claude-sonnet-4-20250514 \
    --workdir results/swebench/$(date +%Y%m%d-%H%M%S)/

# 5) Aggregate the per-task JSON into a summary row.
python3 eval/scripts/aggregate_swebench.py \
    --workdir results/swebench/<run-id>/ \
    --out results/swebench/<run-id>/summary.json

# 6) Paste the numbers into the table above; commit results/.
```

See `eval/README.md` for the Python-side configuration options
(per-task token budgets, retry policy, judge model, etc.).

## Cross-links

- Other reproducible benchmarks: [`BENCHMARK.md`](BENCHMARK.md)
- Evaluation methodology: `docs/04-evaluation/` (when shipped)
- Substrate: `cmd/gortex/eval_swebench.go` + `eval/`
