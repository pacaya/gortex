# Retrieval baselines bench

Reproducible per-baseline NDCG@10 + latency table across six
candidate retrieval systems. The harness ships **six** adapters
behind a single uniform Go-side surface; per-adapter `Available()`
checks let `--smoke` verify wiring without paying for the heavy
ones.

Supported adapters:

| Adapter        | Type          | Install                                                            |
|----------------|---------------|--------------------------------------------------------------------|
| ripgrep        | Go-native     | `brew install ripgrep` (or distro equivalent)                      |
| probe          | Go-native     | `cargo install probe-ai`                                           |
| colgrep        | Python        | `pip install colgrep` (requires CUDA-capable GPU)                  |
| grepai         | Python        | `pip install grepai-cli`                                           |
| coderankembed  | Python+model  | `pip install sentence-transformers transformers torch` (model ~440MB on first run) |
| semble         | Python        | `pip install semble`                                               |

## Running

```sh
# Smoke check: report which adapters are available right now
go run ./bench/baselines -smoke

# Full table against the gortex repo itself, all adapters
go run ./bench/baselines

# Just the locally-installable ones (ripgrep + probe)
go run ./bench/baselines -against ripgrep,probe

# JSON output for downstream tooling
go run ./bench/baselines -format json -json bench/results/baselines.json
```

Flags:

- `-repo PATH` — corpus to query (default `.`)
- `-queries PATH` — JSON query set (default `queries.json` here)
- `-groundtruth PATH` — JSON per-query expected file paths (default
  `groundtruth.json` here)
- `-against LIST` — comma-separated adapter names; unknown names
  error out so a typo doesn't silently drop a baseline
- `-top-k N` — top-K per query (default 10, matches NDCG@10)
- `-out PATH` — primary output (default stdout)
- `-json PATH` — companion JSON metrics output
- `-format markdown|json` — primary format
- `-smoke` — skip runs; only probe `Available()` for each adapter
- `-timeout DURATION` — per-adapter wall-clock cap (default 5m)

## How NDCG@10 is computed

For each query the harness computes:

```
DCG@10  = Σ (gain_i / log2(i + 2))   for i in [0, min(K, |hits|))
IDCG@10 = Σ (1     / log2(i + 2))    for i in [0, min(K, |expected|))
NDCG@10 = DCG@10 / IDCG@10
```

with `gain_i = 1` when `hits[i]` is in the ground-truth set,
`0` otherwise. The reported number is the **mean** across queries
with a non-empty ground-truth entry. Queries the adapter failed
on (Error set) are skipped from the mean — they're shown in the
per-query JSON for debugging but don't penalize the headline.

## Adding an adapter

1. Add a struct in `adapters.go` that satisfies the `adapter`
   interface (`Name`, `Available`, `Run`).
2. Register it in `allAdapters()` — that single list drives the CLI,
   the smoke output, and `adapterByName`.
3. For Python baselines: embed `pythonBaselineAdapter` and call
   `pythonModuleAvailable` / `runPythonModule` (or
   `runPythonScript` if you need a custom wrapper). Document the
   install in the table above.
