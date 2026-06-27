# Cold-index perf regression harness

A `go test` target that cold-indexes a small, fixed Go fixture through the real
indexer pipeline and guards against wall-clock regressions.

## What it measures

Each run constructs a fresh graph + indexer (the same `indexer.New` / `Index`
path the daemon uses) and cold-indexes `testdata/fixture`. Per pass it records:

- **wall-clock** — time for the `Index` call
- **allocated bytes** — `runtime.MemStats.TotalAlloc` delta across the pass
- **GC CPU fraction** — `runtime.MemStats.GCCPUFraction` (plus `NumGC` and
  `PauseTotalNs` deltas)

It times several passes (`GORTEX_BENCH_INDEX_RUNS`, default 8) and keeps the
**minimum** wall-clock as the representative figure — the minimum is the most
stable estimator, since jitter only ever adds time. Only wall-clock is gated;
allocation and GC fraction are recorded so later GC-tuning and bulk-persistence
work can watch them move.

## The gate

The best wall-clock is compared against `testdata/baseline.json`. The test
fails when it exceeds the baseline by more than **15%**.

The baseline is recorded **without** the race detector. Under `-race`, time and
allocation inflate several-fold, so the harness prints the numbers but skips the
gate — `go test -race ./...` stays green.

## Running

```bash
go test ./bench/index-perf/                       # measure + gate
go test -run ColdIndex -v ./bench/index-perf/     # print the numbers
```

## Regenerating the baseline

Run on the target machine after an intentional change:

```bash
GORTEX_BENCH_INDEX_UPDATE_BASELINE=1 go test ./bench/index-perf/
```

Commit the updated `testdata/baseline.json`.

## Knobs

| Env var | Effect |
| --- | --- |
| `GORTEX_BENCH_INDEX_UPDATE_BASELINE=1` | rewrite the committed baseline and pass |
| `GORTEX_BENCH_INDEX_FIXTURE=/abs/dir` | cold-index another tree instead of the fixture |
| `GORTEX_BENCH_INDEX_RUNS=16` | number of timed cold-index passes |

The fixture lives under `testdata/` so the Go toolchain (and `go test ./...`,
`go vet`, golangci-lint) ignores it, while the indexer still parses it because
the harness points the index walk directly at the directory.
