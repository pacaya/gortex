# Multi-repo workspaces

Gortex can index multiple repositories into a single shared graph, enabling cross-repo symbol resolution, impact analysis, and navigation.

## Workspace boundary

Every node and contract is keyed on a **workspace slug**, which is the hard graph boundary for cross-repo work. Two repos that should pair their contracts (an HTTP server and the client that calls it, a Kafka producer and its consumer, etc.) must declare the same `workspace:` in their `.gortex.yaml` — otherwise contract matching stops at the boundary and they look like orphans.

Slug resolution precedence (first match wins):

1. `RepoEntry.workspace` in `~/.gortex/config.yaml` — overrides everything, ideal for OSS / read-only repos where you don't want to leave an artifact in the tree
2. `workspace:` in the repo's own `.gortex.yaml` — the default for first-party repos
3. The repo prefix — fallback when neither is set, so each unconfigured repo gets its own isolated workspace

The same chain applies to the optional `project:` slug (a sub-bucket inside a workspace). The daemon loads every tracked repo into one shared graph; you scope a query to a single workspace or project at request time rather than at startup. Over the HTTP surface (`gortex daemon start --http-addr ...`) the `/v1/graph` route accepts `?project=` and `?repo=` to narrow the dump, so a typo'd value returns an empty result for that request instead of bringing the whole index up empty.

## Configuration

Two-tier config hierarchy:

- **Global config** (`~/.gortex/config.yaml`) — projects, repo lists, active project, reference tags
- **Workspace config** (`.gortex.yaml` per repo) — guards, excludes, local overrides

Excludes are layered — builtin → repo's own `.gitignore` → global → per-repo entry → workspace — with gitignore semantics. The repo's `.gitignore` is respected by default so you don't have to re-declare entries already curated for git; opt out per-workspace with `respect_gitignore: false` in `.gortex.yaml`. Use `!pattern` in a later layer to re-include something an earlier layer excluded. Beyond `.gitignore`, the index walk also honors per-directory `.gortexignore` files (Gortex's own ignore file, a sibling to `.gitignore`) and ripgrep's `.ignore` / `.rgignore` — each scoped to the directory that contains it.

```yaml
# ~/.gortex/config.yaml
active_project: my-saas

exclude:                            # Applies to every tracked repo
  - "**/*.generated.*"
  - "node_modules/"                 # Already in the builtin baseline

repos:
  - path: /home/user/projects/gortex
    name: gortex
    exclude:                        # Extra patterns just for this repo
      - "results/**"

projects:
  my-saas:
    repos:
      - path: /home/user/projects/frontend
        name: frontend
        ref: work
      - path: /home/user/projects/backend
        name: backend
        ref: work
      - path: /home/user/projects/shared-lib
        name: shared-lib
        ref: opensource
```

`synthesize_external_calls: true` (opt-in, default off — set in `.gortex.yaml` or the global config) makes the resolver synthesize placeholder nodes for calls into un-indexed external packages or sibling services, so call-chains keep the external hop instead of terminating at the indexed boundary.

## Daemon tuning (optional)

The daemon's defaults handle typical workflows without configuration. These knobs exist for monorepos, branch-heavy workflows, or filesystems without fsnotify support.

```yaml
# ~/.gortex/config.yaml (or per-repo .gortex.yaml)
watch:
  debounce_ms: 150            # per-file patch debounce (default 150)

  # Storm mode — when more than N events land within the window,
  # switch from per-file debounced patching to a batched reconcile
  # that defers cross-file resolver + search work until a quiet
  # period has passed. Amortises the cost of bulk operations
  # (rsync, npm install, branch checkout, bulk format-on-save,
  # find-and-replace). Zero = disabled (default).
  storm_threshold: 0          # 0 disables; try 50 on monorepos
  storm_window_ms: 500
  storm_quiet_period_ms: 500
```

Environment variables:

- `GORTEX_RECONCILE_INTERVAL` — janitor tick that walks every tracked repo and runs `IncrementalReindex` against disk. Insurance against fsnotify gaps on NFS/SMB mounts, inotify watch-limit exhaustion, or daemon downtime where edits happened offline. Default `1h`; `"0"` or `"off"` disables; otherwise any Go duration string (e.g., `15m`).
- The daemon also watches each tracked repo's `.git/HEAD`, so branch switches and rebases reconcile incrementally (via `git diff --name-status`) rather than by re-indexing every changed file individually — no configuration needed.

## CLI

```bash
gortex track /path/to/repo          # Add a repo to the workspace
gortex untrack /path/to/repo        # Remove a repo from the workspace
gortex mcp --track /path/to/repo    # Track additional repos on startup
gortex mcp --project my-saas        # Set active project scope
gortex index repo-a/ repo-b/        # Index multiple repos
gortex status                       # Per-repo and per-project stats
gortex repos                        # List tracked repos — head-commit SHA, last-indexed time, staleness flag
gortex repos --json                 # Same, machine-readable (for scripts / CI)

# Stamp workspace / project slugs across tracked repos (migration helper)
gortex workspace list                                       # Show what each tracked repo currently declares
gortex workspace list --json                                # Same, machine-readable
gortex workspace set backend api                            # Write workspace=api to backend's .gortex.yaml
gortex workspace set upstream-lib api --global              # OSS-friendly: pin to api in ~/.gortex/config.yaml
gortex workspace set-all api --root ~/projects/work --yes   # Bulk: stamp every tracked repo under a prefix

# Manage the effective ignore list used by indexing + watching
gortex config exclude list                          # Show all layers (builtin, global, repo entry, workspace)
gortex config exclude add pkg/generated             # Default target: workspace .gortex.yaml
gortex config exclude add '**/*.bak' --global       # Write to ~/.gortex/config.yaml
gortex config exclude add testdata/ --repo backend  # Write to a RepoEntry
gortex config exclude remove pkg/generated          # Remove from the same target
```

## MCP tools

Agents can manage repos at runtime without CLI access:

| Tool | Description |
|------|-------------|
| `track_repository` | Add a repo, index immediately, persist to config |
| `untrack_repository` | Remove a repo, evict nodes/edges, persist to config |
| `set_active_project` | Switch project scope for all subsequent queries |
| `get_active_project` | Return current project name and repo list |

All query tools (`search_symbols`, `get_symbol`, `find_usages`, `get_file_summary`, `get_call_chain`, `smart_context`) accept optional `repo`, `project`, and `ref` parameters for scoping. When an active project is set, it applies as the default scope.

## How it works

- **Qualified node IDs** — in multi-repo mode, IDs become `<repo_prefix>/<path>::<Symbol>` (e.g., `frontend/src/app.ts::App`). Single-repo mode keeps the existing `<path>::<Symbol>` format.
- **Cross-repo edges** — the resolver links symbols across repo boundaries with same-repo preference. Cross-repo edges carry a `cross_repo: true` flag.
- **Impact analysis** — `explain_change_impact`, `verify_change`, and `get_test_targets` follow cross-repo edges automatically, grouping results by repository.
- **Shared repos** — the same repo can appear in multiple projects with different reference tags. It's indexed once and shared across projects.
- **Auto-detection** — set `workspace.auto_detect: true` in `.gortex.yaml` to auto-discover Git repos in a parent directory.
