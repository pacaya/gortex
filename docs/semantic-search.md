# Semantic search

**Default-on.** A baked GloVe-50d table (~3.8 MB embedded in the binary, top 20k tokens) gives every install hybrid BM25 + vector search out of the box — no flag, no model download, no native dependency. Reciprocal Rank Fusion blends the two channels, and the BM25↔vector balance is scored *continuously* from the query's shape (identifier density, separators, stopwords) rather than bucketed into a discrete class — so a half-identifier query lands between the symbol and natural-language blends instead of jumping a whole tier. After ranking, an optional pure-cosine refinement pass re-scores the top results with the exact embedding distance the rank-based fusion discards.

## Configuration

Switch or tune providers in `.gortex.yaml`:

```yaml
embedding:
  enabled: true                # default — pass `false` to disable
  provider: static             # static | local | api  (default: static)
  variant: ""                  # optional named local model (e.g. a Hugot variant); empty = the provider default
  api_url: http://localhost:11434
  api_model: nomic-embed-text
  chunk_threshold_lines: 60    # symbols longer than this get split
  chunk_window_lines: 40       # AST-aware window size
  api_concurrency: 4           # bounded worker pool for hosted providers
```

Selecting a different embedding model (`variant`, or `GORTEX_EMBEDDINGS_VARIANT`) that changes vector dimensionality re-embeds the graph on next index; the persisted index guards against a dimension mismatch.

| Provider | Quality | Offline | Native deps | Notes |
|---|---|---|---|---|
| `static` (default) | Good for identifier-shaped queries | Yes | None | Baked GloVe-50d table, CPU-only, zero setup |
| `local` (Hugot MiniLM-L6-v2) | Better for NL queries | After first run | None | Auto-downloads ~90 MB to `~/.gortex/models/` |
| `api` (Ollama / OpenAI) | Best | No | None | Bounded concurrent worker pool — tune via `api_concurrency` |

## AST sub-chunking

Symbols longer than `chunk_threshold_lines` are split into AST-aware windows (block statements, case clauses, field groups) before embedding; each window is vectorised independently and de-duplicated back to the parent symbol at query time, so a large function lands as one hit grounded in the specific chunk that matched — chunk IDs never leak into results.

## Persistent index

The vector index and the chunk → symbol map are persisted in the daemon snapshot; restarts re-warm in milliseconds without re-embedding the graph. Daemon snapshot schema is forward-compatible — older snapshots load with an empty vector layer and rebuild incrementally.

## Vocabulary bridging without an LLM

A curated equivalence table (`auth` ↔ `authentication` ↔ `login`, `delete` ↔ `remove` ↔ `destroy`, …) plus per-repo auto-concept mining from symbol-name token co-occurrence expands queries deterministically — runs alongside (and dedup against) any LLM expansion. Toggle via `search.equivalence_classes`. A weighted concept-relatedness layer sits on top of the flat classes (e.g. `auth` pulls in `token` / `session` at lower priority) without merging the distinct concepts. When an LLM expander *is* configured, `vocab_anchored: true` constrains its invented terms to tokens that actually occur in the repo's symbol vocabulary.

## HITS reranking

A hubs-and-authorities pass over the reference/call graph contributes a `hits` signal to the rerank pipeline — heavily-referenced symbols outrank shallow utility nodes, and the hub penalty (`authority / (1 + hub)`) demotes called-by-everything infra so it doesn't drown the result page.

### Edge-provenance attenuation

Centrality (HITS + PageRank) and a dedicated rerank signal weight call/reference edges by how they were resolved: the abundant LSP-dispatch / framework-wiring tier — and the weak name-only tier — are attenuated relative to the structurally-unambiguous tier. Dense LSP enrichment otherwise inflates the apparent centrality of utility and framework code over genuine domain authorities. The weighting is a no-op on graphs with no resolution provenance recorded, so it never changes ranking where the data is absent.

### Other rerank refinements

- **Generated-file demotion** — a generated file (`*.pb.go`, `mock_*.go`, `*_pb2.py`, …) is ranked below a real same-named hand-written implementation, but only when one exists.
- **Source over test** — when a query surfaces both an implementation and its test, the implementation is lifted above the test (only when both co-occur, so it never shifts the rest of the page).

### Sparse sub-word tokenization (opt-in)

An optional tokenizer stage emits sub-word n-grams whose split points come from a per-repo boundary table learned from symbol names at index time, trading exact-identifier precision for recall on typo/fragment queries. Off by default (it is reindex-required and precision-sensitive); enable with `GORTEX_SPARSE_NGRAM=1`. Applies to the BM25 backend.

## Keyword-soup defense

Boolean / OR-soup queries (`A OR B OR 'no access' OR …`) — and operator-free keyword lists (`parse decode unmarshal token jwt cache`) and comma-enumerations — defeat embedding retrieval. The query classifier detects all three, skips wasted LLM expansion, and splits the soup into terms fused via the existing BM25 expansion path; a `query_advice` nudge rides on the response. Genuine natural-language questions stay classified as concept. Tune via `search.keyword_soup_rewrite: split | nudge | off`.

## Prose corpus

Markdown headings + section bodies become first-class searchable nodes (`KindDoc`) — `search_symbols corpus: "docs"` returns ranked README / ADR / design-doc sections; `corpus: "all"` mixes them with code hits. A docs query runs its own retrieval channel (a parallel doc-biased fetch, not merely a post-filter over the code fetch) and applies a prose weight profile that suppresses code-structural rerank signals (API/type-signature, definition-bias) which are meaningless for prose. Section node IDs are derived from the heading path, so incremental reindex of a touched markdown file produces stable IDs.

## Per-keyword TaskMemory

The combo store now keys symbol associations both on the whole query and per keyword, so a new task with similar keywords inherits learned ranking from prior searches even when the exact phrasing differs. Exact-query matches still dominate; per-keyword evidence is the lower-confidence generalisation.

## Build-tag backends

Opt-in faster local backends via build tags:

```bash
go build -tags embeddings_onnx ./cmd/gortex/   # needs: brew install onnxruntime
go build -tags embeddings_gomlx ./cmd/gortex/  # auto-downloads XLA plugin
```

The `embeddings_onnx` backend (GTE-small) **never auto-downloads**: place `model.onnx` and `vocab.txt` in `~/.gortex/models/gte-small/` yourself and install the ONNX Runtime native library (`brew install onnxruntime`, or the distro equivalent). Without both, the backend reports "ONNX model not found" and the local chain falls through to the pure-Go Hugot backend.

The legacy `--embeddings` / `--embeddings-url` / `--embeddings-model` CLI flags and the `GORTEX_EMBEDDINGS*` env vars still take precedence over the config block — useful for one-shot overrides without editing `.gortex.yaml`.

## `search_symbols` `assist:` modes

- `auto` (default) — skips LLM for identifier queries, expands NL queries
- `on` — forces expansion + rerank
- `off` — pure BM25
- `deep` — adds a body-grounded verification pass; +1.5–4 s; quality is highly model-dependent — unreliable on 3B local models, fine on 7B+ or hosted

See [llm.md](llm.md) for provider configuration.
