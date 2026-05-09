// Instructions shared across every doc-aware adapter. Centralising the
// body here avoids per-adapter drift: Cursor's .cursor/rules file,
// Copilot's .github/copilot-instructions.md, Codex's AGENTS.md, and
// Claude Code's CLAUDE.md all read from the same constant, so when the
// "prefer Gortex over Read/Grep" story evolves we update it once and
// every agent sees the change on the next `gortex init`.
//
// The claudecode adapter extends this body with its own slash-commands
// appendix — that part is Claude-Code-specific and lives in
// claudecode/content.go, keyed off the same sentinel so idempotency
// checks line up across adapters.
package agents

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// InstructionsSentinel is the substring every doc-aware adapter checks
// for when deciding whether to append the instructions block. If it's
// already present (wherever it came from — a prior `gortex init`, a
// user-copied block, another adapter writing to a shared rules file
// like AGENTS.md) we skip to stay idempotent.
const InstructionsSentinel = "## MANDATORY: Use Gortex MCP tools"

// CommunitiesStartMarker / CommunitiesEndMarker fence the generated
// community-routing block that `gortex init` writes into per-repo
// instructions files. Fenced (not just start-only) because this block
// is regenerated on every `init` re-run as the codebase evolves, so
// we need to identify and overwrite it precisely without clobbering
// user edits around it.
const (
	CommunitiesStartMarker = "<!-- gortex:communities:start -->"
	CommunitiesEndMarker   = "<!-- gortex:communities:end -->"
)

// GlobalRulesStartMarker / GlobalRulesEndMarker fence the rule block
// that `gortex install` merges into ~/.claude/CLAUDE.md. The block is
// idempotent (re-running install replaces it in place) and removable
// (user can delete the marked region by hand without other side
// effects). Distinct from the communities markers above because this
// block lives at user level and survives every project init.
const (
	GlobalRulesStartMarker = "<!-- gortex:rules:start -->"
	GlobalRulesEndMarker   = "<!-- gortex:rules:end -->"
)

// GlobalInstructionsBody is the rule block written into the
// user-level ~/.claude/CLAUDE.md by `gortex install`. Mirrors
// InstructionsBody (the per-project rules) but trimmed to the
// always-applicable parts — multi-repo specifics, project-skill
// generation, and contracts hygiene are project-scoped and stay in
// per-repo CLAUDE.md.
const GlobalInstructionsBody = `## MANDATORY: Use Gortex MCP tools instead of Read/Grep/Glob

A Gortex daemon is configured machine-wide via the ` + "`gortex` MCP server" + `. Whenever you are operating on indexed source code (any repo registered with the daemon — check ` + "`gortex daemon status`" + `), you MUST prefer graph queries over file reads. PreToolUse hooks deny ` + "`Read`" + ` / ` + "`Grep`" + ` / ` + "`Glob`" + ` against indexed source — the deny message names the right tool.

### Search and Navigation

| Instead of...                         | You MUST use...                          |
|---------------------------------------|------------------------------------------|
| ` + "`Grep`" + ` / ` + "`grep`" + ` / ` + "`rg`" + ` for a symbol      | ` + "`search_symbols`" + ` (BM25 + camelCase-aware)|
| ` + "`Grep`" + ` for references                 | ` + "`find_usages`" + ` (zero false positives)     |
| ` + "`Grep`" + ` to find callers                | ` + "`get_callers`" + ` / ` + "`get_call_chain`" + `         |
| ` + "`Glob`" + ` over source files (` + "`**/*.go`" + `)  | ` + "`get_repo_outline`" + ` / ` + "`search_symbols`" + `    |
| Multiple ` + "`Read`" + ` calls to explore      | ` + "`smart_context`" + ` (one call)               |

### Reading Source

| Instead of...                         | You MUST use...                          |
|---------------------------------------|------------------------------------------|
| ` + "`Read`" + ` whole file for one function    | ` + "`get_symbol_source`" + ` (80% fewer tokens)   |
| ` + "`Read`" + ` to understand a file           | ` + "`get_file_summary`" + ` / ` + "`get_editing_context`" + ` |
| ` + "`Read`" + ` to check a signature           | ` + "`get_symbol`" + ` (signature in ` + "`meta.signature`" + `) |
| ` + "`Read`" + ` to trace calls                 | ` + "`get_call_chain`" + ` / ` + "`get_callers`" + `         |

### Editing and Refactoring

| Instead of...                         | You MUST use...                          |
|---------------------------------------|------------------------------------------|
| ` + "`Edit`" + ` whole file by string match    | ` + "`edit_file`" + ` (Gortex MCP — no pre-Read required, atomic write, auto-reindex; pass ` + "`dry_run`" + ` to preview) |
| ` + "`Write`" + ` a new file or full rewrite   | ` + "`write_file`" + ` (no pre-Read required; creates parent dirs; pass ` + "`dry_run`" + ` to preview) |
| Read→Edit roundtrip for one symbol    | ` + "`edit_symbol`" + ` (edit by ID)               |
| Manual find-and-replace for renames   | ` + "`rename_symbol`" + ` (cross-file refs)        |
| Sequencing multi-file edits yourself  | ` + "`batch_edit`" + ` (dependency-ordered)        |

### Dataflow (CPG-lite)

The ` + "`flow_between`" + ` and ` + "`taint_paths`" + ` MCP tools answer **"where does this value flow?"** by walking the new dataflow edges (` + "`value_flow`" + ` intra-procedural; ` + "`arg_of`" + ` caller-arg→callee-param; ` + "`returns_to`" + ` callee→assignment).

| Instead of...                         | You MUST use...                          |
|---------------------------------------|------------------------------------------|
| Tracing a value through helpers by hand | ` + "`flow_between(source_id, sink_id, max_depth=8)`" + ` — ranked dataflow paths between two symbols |
| Grepping for sources / sinks         | ` + "`taint_paths(source_pattern, sink_pattern)`" + ` — pattern-driven sweep. Patterns: bare token = name substring; ` + "`exact:Foo`" + `; ` + "`path:dir/`" + `; ` + "`kind:method`" + `. Sinks auto-expand functions to their params. |

### Code Quality and Analysis

The ` + "`analyze`" + ` MCP tool is a unified dispatcher. Pass ` + "`kind: \"<name>\"`" + ` for one of:

- Structural: ` + "`dead_code`" + `, ` + "`hotspots`" + `, ` + "`cycles`" + `, ` + "`would_create_cycle`" + `
- Comments / churn: ` + "`todos`" + `, ` + "`stale_code`" + `, ` + "`ownership`" + `
- Coverage / releases: ` + "`coverage`" + `, ` + "`coverage_gaps`" + `, ` + "`coverage_summary`" + `, ` + "`releases`" + `, ` + "`blame`" + `
- Schema: ` + "`orphan_tables`" + `, ` + "`unreferenced_tables`" + `
- Flags / interop: ` + "`stale_flags`" + `, ` + "`cgo_users`" + `, ` + "`wasm_users`" + `
- Edge-driven: ` + "`channel_ops`" + `, ` + "`goroutine_spawns`" + `, ` + "`field_writers`" + `, ` + "`annotation_users`" + `, ` + "`config_readers`" + `, ` + "`event_emitters`" + `, ` + "`error_surface`" + `, ` + "`external_calls`" + `
- Framework layer: ` + "`routes`" + ` (handler ↔ HTTP/gRPC/WS/GraphQL/topic), ` + "`models`" + ` (ORM class ↔ DB table), ` + "`components`" + ` (parent → child JSX)

The ` + "`gortex enrich blame|coverage|releases|all`" + ` CLI hydrates the graph with the metadata that the ` + "`stale_*`" + `, ` + "`coverage*`" + `, ` + "`ownership`" + `, and ` + "`releases`" + ` analyzers need.

### Token Economy

For list-shaped responses (` + "`search_symbols`" + `, ` + "`find_usages`" + `, ` + "`analyze`" + `, ` + "`batch_symbols`" + `, ` + "`get_callers`" + `, ` + "`get_call_chain`" + `, ` + "`get_dependencies`" + `, ` + "`get_dependents`" + `, ` + "`find_implementations`" + `, ` + "`get_file_summary`" + `, ` + "`get_editing_context`" + `, ` + "`smart_context`" + `, ` + "`contracts`" + `), pass ` + "`format: \"gcx\"`" + ` to get GCX1 compact wire format — round-trippable, ~27% fewer tokens. Decode with ` + "`@gortex/wire`" + ` (npm) or ` + "`github.com/gortexhq/gcx-go`" + ` (Go).

### Session Start

The SessionStart hook injects daemon status (tracked repos, cwd coverage, ready/warmup state). If you see "daemon is not running" — run ` + "`gortex daemon start --detach`" + ` and re-run the task. If you see "cwd is not covered by any tracked repo" — graph tools won't be available for that directory.
`

// InstructionsBody is the shared rule block every adapter writes to
// its agent's instructions file. Tool names in the tables (Read, Grep)
// are Claude-Code-specific flavour; models outside Claude Code read
// them as "any file-reading tool" — the principle stays the same so
// we keep one body rather than branch by agent.
const InstructionsBody = `## MANDATORY: Use Gortex MCP tools instead of Read/Grep

Gortex is running as an MCP server. You MUST use graph queries instead of file reads whenever possible. This saves thousands of tokens per task.

### Navigation and Reading

| Instead of...                         | You MUST use...                          |
|---------------------------------------|------------------------------------------|
| ` + "`Read`" + ` a whole file for one function  | ` + "`get_symbol_source`" + ` with ` + "`id: \"path/to/file.go::SymbolName\"`" + ` (80% fewer tokens) — use ` + "`get_file_summary`" + ` first if you don't know the symbol name |
| ` + "`Read`" + ` to find a function             | ` + "`get_symbol`" + ` or ` + "`get_editing_context`" + `    |
| Multiple ` + "`get_symbol`" + ` calls           | ` + "`batch_symbols`" + ` (one call for N symbols) |
| ` + "`Grep`" + ` for references                 | ` + "`find_usages`" + ` (zero false positives)     |
| ` + "`Grep`" + ` to find a symbol by name       | ` + "`search_symbols`" + ` (BM25 + camelCase-aware)|
| Filtering ` + "`search_symbols`" + ` by hand    | ` + "`winnow_symbols`" + ` — structured constraint chain (kind, language, community, path_prefix, min_fan_in, min_churn) with per-axis score contributions |
| ` + "`Read`" + ` to understand a file           | ` + "`get_file_summary`" + ` or ` + "`get_editing_context`" + ` |
| ` + "`Read`" + ` multiple files to trace calls  | ` + "`get_call_chain`" + ` / ` + "`get_callers`" + `         |
| Guessing an import path               | ` + "`find_import_path`" + `                       |
| ` + "`Read`" + ` to check a function signature  | ` + "`get_symbol`" + ` (signature is in ` + "`meta.signature`" + `) |
| 5-10 calls to explore for a task      | ` + "`smart_context`" + ` (one call)               |

### Impact Analysis and Safety

| Instead of...                         | You MUST use...                          |
|---------------------------------------|------------------------------------------|
| Reading files to assess change scope  | ` + "`explain_change_impact`" + ` (includes cross-community warnings) |
| Guessing which tests to run           | ` + "`get_test_targets`" + `                       |
| Manual dependency ordering            | ` + "`get_edit_plan`" + `                          |
| Hoping signature changes are safe     | ` + "`verify_change`" + ` — checks callers and interface implementors |
| Manually checking team conventions    | ` + "`check_guards`" + ` — evaluates guard rules from .gortex.yaml |
| Wondering if a new dep creates a cycle| ` + "`analyze`" + ` with ` + "`kind: \"would_create_cycle\"`" + ` — checks before you add it |

### Code Quality and Analysis

The ` + "`analyze`" + ` MCP tool is a unified dispatcher. Supported ` + "`kind`" + ` values:

| Instead of...                         | You MUST use...                          |
|---------------------------------------|------------------------------------------|
| Manually hunting unused code          | ` + "`analyze`" + ` with ` + "`kind: \"dead_code\"`" + ` — zero incoming edges (excludes entry points, tests, exports) |
| Guessing which symbols are over-coupled| ` + "`analyze`" + ` with ` + "`kind: \"hotspots\"`" + ` — ranks by fan-in, fan-out, community crossings |
| Manually scanning for circular deps   | ` + "`analyze`" + ` with ` + "`kind: \"cycles\"`" + ` — Tarjan's SCC with severity classification |
| Wondering if a new dep creates a cycle| ` + "`analyze`" + ` with ` + "`kind: \"would_create_cycle\"`" + ` — checks before you add it |
| Grepping for TODO / FIXME             | ` + "`analyze`" + ` with ` + "`kind: \"todos\"`" + ` — KindTodo nodes, filter by tag/assignee/ticket |
| Walking blame by hand                 | ` + "`analyze`" + ` with ` + "`kind: \"blame\"`" + ` — stamps meta.last_authored from git blame |
| Reading a cover.out profile manually  | ` + "`analyze`" + ` with ` + "`kind: \"coverage\"`" + ` — stamps meta.coverage_pct on executable symbols |
| Hunting symbols nobody touches        | ` + "`analyze`" + ` with ` + "`kind: \"stale_code\"`" + ` — symbols older than ` + "`older_than`" + ` days |
| Asking who owns a package             | ` + "`analyze`" + ` with ` + "`kind: \"ownership\"`" + ` — author rollup with symbol/file counts |
| Finding undertested symbols           | ` + "`analyze`" + ` with ` + "`kind: \"coverage_gaps\"`" + ` — symbols inside [min_pct, max_pct) |
| Per-package coverage rollup           | ` + "`analyze`" + ` with ` + "`kind: \"coverage_summary\"`" + ` — directory-level avg/covered/partial/uncovered |
| Stale feature flags                   | ` + "`analyze`" + ` with ` + "`kind: \"stale_flags\"`" + ` — flag callers untouched for ` + "`older_than`" + ` days |
| Walking git tags by hand              | ` + "`analyze`" + ` with ` + "`kind: \"releases\"`" + ` — stamps meta.added_in on file nodes |
| Surveying cgo / wasm boundaries       | ` + "`analyze`" + ` with ` + "`kind: \"cgo_users\"`" + ` or ` + "`kind: \"wasm_users\"`" + ` — files crossing the FFI boundary |
| Finding tables without migrations     | ` + "`analyze`" + ` with ` + "`kind: \"orphan_tables\"`" + ` — queried tables missing EdgeProvides |
| Finding migrations without users      | ` + "`analyze`" + ` with ` + "`kind: \"unreferenced_tables\"`" + ` — provided tables with zero EdgeQueries |
| Spotting channel send/recv mismatches | ` + "`analyze`" + ` with ` + "`kind: \"channel_ops\"`" + ` — channels grouped by sends/recvs |
| Finding goroutine spawn hotspots      | ` + "`analyze`" + ` with ` + "`kind: \"goroutine_spawns\"`" + ` — EdgeSpawns grouped by target + mode |
| Finding mutability hotspots           | ` + "`analyze`" + ` with ` + "`kind: \"field_writers\"`" + ` — fields ranked by EdgeWrites; pass ` + "`id`" + ` for one field |
| Listing every @Deprecated use         | ` + "`analyze`" + ` with ` + "`kind: \"annotation_users\"`" + ` — pass ` + "`id`" + ` or ` + "`name`" + ` for one annotation |
| Tracing config-key readers            | ` + "`analyze`" + ` with ` + "`kind: \"config_readers\"`" + ` — config_key nodes grouped by EdgeReadsConfig |
| Tracing event/log emitters            | ` + "`analyze`" + ` with ` + "`kind: \"event_emitters\"`" + ` — events grouped by EdgeEmits, ` + "`level`" + ` filter optional |
| Mapping the error surface             | ` + "`analyze`" + ` with ` + "`kind: \"error_surface\"`" + ` — function/method nodes with their EdgeThrows targets |
| Surveying stdlib / module-cache reach | ` + "`analyze`" + ` with ` + "`kind: \"external_calls\"`" + ` — KindModule nodes grouped by call/symbol counts; pass ` + "`id`" + ` for per-symbol detail, ` + "`module_kind`" + ` for stdlib/module_cache filter |
| Listing every HTTP/gRPC/WS route      | ` + "`analyze`" + ` with ` + "`kind: \"routes\"`" + ` — handler→route pairs from the EdgeHandlesRoute graph layer; ` + "`method`" + `, ` + "`path`" + `, ` + "`type`" + ` filters (` + "`type`" + ` ∈ http/grpc/ws/graphql/topic) |
| Mapping ORM models to tables          | ` + "`analyze`" + ` with ` + "`kind: \"models\"`" + ` — class→table edges from EdgeModelsTable across gorm / SQLAlchemy / Django / ActiveRecord / JPA / TypeORM; ` + "`orm`" + `, ` + "`table`" + `, ` + "`model`" + ` filters |
| Walking the JSX component tree        | ` + "`analyze`" + ` with ` + "`kind: \"components\"`" + ` — parent↔child fan-in/out from EdgeRendersChild; pass ` + "`id`" + ` for per-component child list |
| Checking if the index is stale        | ` + "`index_health`" + ` — health score, parse failures, stale files |
| Wondering what changed this session   | ` + "`get_symbol_history`" + ` — modification counts, flags churning (3+ edits) |
| Hydrating blame / coverage / releases | ` + "`gortex enrich blame|coverage|releases|all`" + ` (CLI) — bulk-stamps the graph for the ` + "`stale_*`" + `, ` + "`coverage_*`" + `, ` + "`ownership`" + `, and ` + "`releases`" + ` analyzers |

### Code Generation and Editing

| Instead of...                         | You MUST use...                          |
|---------------------------------------|------------------------------------------|
| Reading files to learn a pattern      | ` + "`suggest_pattern`" + `                        |
| Manually scaffolding from a pattern   | ` + "`scaffold`" + ` — generates code, wiring, and test stubs from an example |
| Read→Edit roundtrip for one symbol    | ` + "`edit_symbol`" + ` — edit source by ID, no Read needed |
| Read→Edit roundtrip for any file      | ` + "`edit_file`" + ` — string-replace any file by absolute or repo-relative path; atomic write, auto-reindex; pass ` + "`dry_run`" + ` to preview |
| Read→Write roundtrip for new files    | ` + "`write_file`" + ` — create or overwrite any file with given content; creates parent dirs; pass ` + "`dry_run`" + ` to preview |
| Manual find-and-replace for renames   | ` + "`rename_symbol`" + ` — coordinated rename across all references |
| Sequencing multi-file edits yourself  | ` + "`batch_edit`" + ` — applies edits in dependency order, re-indexes between steps |
| Reading a diff without graph context  | ` + "`diff_context`" + ` — enriches git diff with callers, callees, community, risk |
| Guessing what context you need next   | ` + "`prefetch_context`" + ` — predicts needed symbols from task + recent activity |

### API Contracts

| Instead of...                         | You MUST use...                          |
|---------------------------------------|------------------------------------------|
| Manually tracking API routes/services | ` + "`contracts`" + ` (default ` + "`action: \"list\"`" + `) — lists HTTP, gRPC, GraphQL, topic, WebSocket, env, OpenAPI; filter by ` + "`repo`" + `, ` + "`project`" + `, or ` + "`ref`" + ` |
| Guessing if APIs match across repos   | ` + "`contracts`" + ` with ` + "`action: \"check\"`" + ` — detects orphan providers/consumers and mismatches; scope with ` + "`repo`" + ` / ` + "`project`" + ` / ` + "`ref`" + ` |

### CPG-lite Dataflow

The ` + "`flow_between`" + ` and ` + "`taint_paths`" + ` MCP tools answer **"where does this value flow?"** by walking three new edge kinds — ` + "`value_flow`" + ` (intra-procedural), ` + "`arg_of`" + ` (caller arg → callee param), and ` + "`returns_to`" + ` (callee → assignment).

| Instead of...                         | You MUST use...                          |
|---------------------------------------|------------------------------------------|
| Hand-tracing a value through helper functions | ` + "`flow_between`" + ` — ranked dataflow paths between two symbol IDs; pass ` + "`max_depth`" + ` (default 8) and ` + "`max_paths`" + ` (default 10); supports ` + "`format: \"gcx\"`" + ` |
| Grepping for sources / sinks         | ` + "`taint_paths`" + ` — pattern-driven sweep returning every flow from a matching source to a matching sink. Pattern syntax: bare token = case-insensitive substring on name; ` + "`exact:Foo`" + ` = exact match; ` + "`path:dir/`" + ` = file-path prefix; ` + "`kind:method`" + ` = node-kind filter; combine clauses with spaces (AND). Sinks expand functions to their params automatically. |
| Reading callers to verify a refactor | ` + "`flow_between`" + ` from the changed return symbol to a downstream consumer's param to find every consumer site, including those reached through helper functions. |

### Multi-Repo Management

| Instead of...                         | You MUST use...                          |
|---------------------------------------|------------------------------------------|
| Manually adding a repo to config      | ` + "`track_repository`" + ` — indexes immediately, persists to config |
| Manually removing a repo from config  | ` + "`untrack_repository`" + ` — evicts nodes/edges, persists to config |
| Wondering which project is active     | ` + "`get_active_project`" + ` — returns project name and repo list |
| Switching project context             | ` + "`set_active_project`" + ` — re-scopes all subsequent queries |
| Scoping a query to one repo           | Pass ` + "`repo`" + ` param to ` + "`search_symbols`" + `, ` + "`find_usages`" + `, etc. |
| Scoping a query to a project          | Pass ` + "`project`" + ` param to any query tool |
| Filtering by reference tag            | Pass ` + "`ref`" + ` param to any query tool |

## Session start (Gortex)

1. Call ` + "`graph_stats`" + ` to confirm Gortex is running and get repo orientation.
2. If ` + "`total_nodes`" + ` is 0, call ` + "`index_repository`" + ` with path ` + "`\".\"`" + `.
3. In multi-repo mode, call ` + "`get_active_project`" + ` to check scope. Use ` + "`set_active_project`" + ` to switch if needed.
4. For a new task, call ` + "`smart_context`" + ` with the task description.
5. For every file you are about to edit, call ` + "`get_editing_context`" + ` first.
6. Before changing a function signature, call ` + "`verify_change`" + ` to catch contract violations — checks callers across all repos.
7. Before any refactor, call ` + "`get_edit_plan`" + ` for dependency-ordered file list. Use ` + "`batch_edit`" + ` to apply atomically.
8. After editing, call ` + "`check_guards`" + ` to verify team conventions, then ` + "`get_test_targets`" + ` for tests to run (includes cross-repo test files).
9. Before committing, call ` + "`detect_changes`" + ` to verify scope. Use ` + "`diff_context`" + ` for graph-enriched review.

## Graph Schema (Gortex)

**Node kinds** (filter ` + "`search_symbols`" + ` with ` + "`kind`" + `):
- Code structure: ` + "`file`" + `, ` + "`package`" + `, ` + "`function`" + `, ` + "`method`" + `, ` + "`type`" + `, ` + "`interface`" + `, ` + "`field`" + `, ` + "`variable`" + `, ` + "`constant`" + `, ` + "`import`" + `, ` + "`contract`" + `, ` + "`param`" + `, ` + "`closure`" + `, ` + "`enum_member`" + `, ` + "`generic_param`" + `
- Coverage extensions: ` + "`module`" + ` (ecosystem deps), ` + "`table`" + `/` + "`column`" + ` (db schema), ` + "`config_key`" + ` (env/viper), ` + "`flag`" + ` (feature flags), ` + "`event`" + ` (logs/metrics/spans), ` + "`migration`" + `, ` + "`fixture`" + ` (test data), ` + "`todo`" + ` (TODO/FIXME comments), ` + "`team`" + ` (CODEOWNERS), ` + "`license`" + `, ` + "`release`" + ` (tag boundaries)

**Edge kinds** (used internally; pass kind name to ` + "`analyze`" + ` to query):
- Calls / structure: ` + "`calls`" + `, ` + "`imports`" + `, ` + "`defines`" + `, ` + "`implements`" + `, ` + "`extends`" + `, ` + "`references`" + `, ` + "`member_of`" + `, ` + "`instantiates`" + `, ` + "`provides`" + `, ` + "`consumes`" + `, ` + "`composes`" + `, ` + "`aliases`" + `, ` + "`typed_as`" + `, ` + "`returns`" + `, ` + "`captures`" + `, ` + "`param_of`" + `
- Concurrency: ` + "`spawns`" + ` (goroutine/async), ` + "`sends`" + ` / ` + "`recvs`" + ` (channels)
- Mutation: ` + "`reads`" + ` / ` + "`writes`" + ` (fields), ` + "`reads_config`" + ` / ` + "`writes_config`" + `
- Dataflow (CPG-lite, ` + "`flow_between`" + ` / ` + "`taint_paths`" + `): ` + "`value_flow`" + ` (intra-procedural assignment / return / range), ` + "`arg_of`" + ` (caller arg → callee param), ` + "`returns_to`" + ` (callee → assignment LHS)
- Metadata: ` + "`annotated`" + ` (decorators), ` + "`emits`" + ` (events), ` + "`throws`" + ` (errors), ` + "`queries`" + ` (SQL), ` + "`reads_col`" + ` / ` + "`writes_col`" + `, ` + "`toggles_flag`" + `, ` + "`depends_on_module`" + `, ` + "`matches`" + ` (fixtures), ` + "`generated_by`" + `, ` + "`tests`" + ` (test → tested symbol), ` + "`covered_by`" + `, ` + "`owns`" + ` (CODEOWNERS), ` + "`authored`" + `, ` + "`licensed_as`" + `
`

// AppendInstructions appends body to path, creating the file if
// missing. Idempotent: when `sentinel` is already present anywhere in
// the file we skip with ActionSkip and log the reason. Callers pass
// the adapter's ApplyOpts through so --dry-run / --global / --force
// all flow to the right FileAction status.
//
// Not atomic. Rules files are plaintext a human edits, matching the
// historical CLAUDE.md append behaviour — a concurrent external writer
// during init is extraordinarily unlikely and atomic rename of a file
// a human is editing would fight their editor.
func AppendInstructions(w io.Writer, path, body, sentinel string, opts ApplyOpts) (FileAction, error) {
	existing, readErr := os.ReadFile(path)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return FileAction{}, fmt.Errorf("read %s: %w", path, readErr)
	}
	existed := readErr == nil
	if existed && strings.Contains(string(existing), sentinel) {
		if w != nil {
			_, _ = fmt.Fprintf(w, "[gortex init] skip %s (Gortex block already present)\n", path)
		}
		return FileAction{Path: path, Action: ActionSkip, Reason: "block-present"}, nil
	}

	if opts.DryRun {
		action := ActionWouldMerge
		if !existed {
			action = ActionWouldCreate
		}
		return FileAction{Path: path, Action: action, Keys: []string{"gortex-block"}}, nil
	}

	// Two blank lines between existing content and the block so the
	// appended section reads as a separate document and doesn't glue
	// onto the last paragraph the user wrote.
	prefix := ""
	if existed && len(existing) > 0 {
		prefix = "\n\n"
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return FileAction{}, err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return FileAction{}, err
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(prefix + body); err != nil {
		return FileAction{}, err
	}
	if w != nil {
		_, _ = fmt.Fprintf(w, "[gortex init] appended Gortex block to %s\n", path)
	}
	action := ActionMerge
	if !existed {
		action = ActionCreate
	}
	return FileAction{Path: path, Action: action, Keys: []string{"gortex-block"}}, nil
}

// CursorMDCFrontmatter wraps the instructions body in the YAML
// frontmatter Cursor expects for MDC rules files. Cursor reads
// `alwaysApply: true` rules on every chat turn — which is what we
// want for the MANDATORY-prefer-Gortex block.
//
// Kept separate from AppendInstructions because MDC files are
// one-rule-per-file (Cursor owns the filename, not the content), so
// they use WriteIfNotExists semantics, not append.
func CursorMDCFrontmatter(body string) string {
	return `---
description: Gortex code intelligence — prefer graph tools over file reads
alwaysApply: true
---

` + body
}

// UpsertMarkedBlock writes `body` into `path` between `startMarker`
// and `endMarker`. Unlike AppendInstructions, this is idempotent AND
// regeneratable: if the markers already exist the block between them
// is replaced; otherwise the block is appended with a blank-line gap
// to existing content. If `body` is empty and the markers exist, the
// block is removed (migration use case). Creates the file if missing.
//
// Designed for the per-repo community-routing block which regenerates
// on every `gortex init` run as the graph evolves.
func UpsertMarkedBlock(w io.Writer, path, body, startMarker, endMarker string, opts ApplyOpts) (FileAction, error) {
	existing, readErr := os.ReadFile(path)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return FileAction{}, fmt.Errorf("read %s: %w", path, readErr)
	}
	existed := readErr == nil
	text := ""
	if existed {
		text = string(existing)
	}

	hasBlock := existed && strings.Contains(text, startMarker) && strings.Contains(text, endMarker)
	empty := strings.TrimSpace(body) == ""

	// Nothing to do: empty body and no existing block.
	if empty && !hasBlock {
		return FileAction{Path: path, Action: ActionSkip, Reason: "no-communities"}, nil
	}

	fenced := startMarker + "\n" + body + "\n" + endMarker + "\n"

	var next string
	switch {
	case hasBlock:
		start := strings.Index(text, startMarker)
		end := strings.Index(text, endMarker) + len(endMarker)
		// Trim trailing newline after the end marker so we don't
		// accumulate blank lines on repeated re-runs.
		if end < len(text) && text[end] == '\n' {
			end++
		}
		if empty {
			next = text[:start] + text[end:]
		} else {
			next = text[:start] + fenced + text[end:]
		}
	case !existed:
		next = fenced
	default:
		prefix := ""
		if len(text) > 0 {
			if !strings.HasSuffix(text, "\n") {
				prefix = "\n\n"
			} else if !strings.HasSuffix(text, "\n\n") {
				prefix = "\n"
			}
		}
		next = text + prefix + fenced
	}

	// Skip when the file would end up byte-identical to what's
	// already there — important for AssertIdempotent semantics and
	// for avoiding spurious mtime bumps on `gortex init` re-runs
	// when the graph hasn't changed.
	if existed && next == text {
		return FileAction{Path: path, Action: ActionSkip, Reason: "unchanged"}, nil
	}

	if opts.DryRun {
		switch {
		case !existed:
			return FileAction{Path: path, Action: ActionWouldCreate, Keys: []string{"communities-block"}}, nil
		case hasBlock:
			return FileAction{Path: path, Action: ActionWouldMerge, Keys: []string{"communities-block"}}, nil
		default:
			return FileAction{Path: path, Action: ActionWouldMerge, Keys: []string{"communities-block"}}, nil
		}
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return FileAction{}, err
	}
	if err := os.WriteFile(path, []byte(next), 0o644); err != nil {
		return FileAction{}, err
	}
	if w != nil {
		verb := "updated"
		if !existed {
			verb = "wrote"
		}
		_, _ = fmt.Fprintf(w, "[gortex init] %s %s (communities block)\n", verb, path)
	}
	action := ActionMerge
	if !existed {
		action = ActionCreate
	}
	return FileAction{Path: path, Action: action, Keys: []string{"communities-block"}}, nil
}
