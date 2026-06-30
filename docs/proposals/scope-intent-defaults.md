# Proposal: intent-based default scoping for multi-repo workspaces

*A discussion starter - not a finished decision. I think the workspace/project/repo model is the right foundation; this is about the **default breadth** of a query and the **uniformity** of the scope overrides. Corrections welcome, especially where I've misread the intent.*

## Summary

In a workspace that indexes many repos, the query tools default to a broader scope than a developer usually wants while working inside one repo. Two changes, separable:

- **Layer A - uniform scope overrides.** Let every scoped tool accept `repo` / `project` / `workspace` / `scope`, resolved the same way and clamped to the session workspace. Pure capability; **no behavior change**.
- **Layer B - intent-based defaults.** Default *locate* tools (symbol search) to the **current repo** and *reach* tools (usages/callers/contracts) to the **workspace**, with `project` as an explicit-only middle rung. **Behavior change; config-gated via `scope.intent_defaults` / `GORTEX_SCOPE_INTENT_DEFAULTS`. Shipped default on.**

Layer A stands on its own. Layer B is config-gated and ships **default on** (opt out to restore today's "search everything indexed" behavior).

## How scoping works today (my reading - please correct)

- Three nesting levels: **repo** (`Node.RepoPrefix`) ⊂ **project** (`Node.ProjectID`, soft) ⊂ **workspace** (`Node.WorkspaceID`, hard isolation boundary).
- A daemon session resolves its scope from cwd - `ScopeForCWD` returns `(workspace, project, repo)` - and caches the home repo, but uses it **only for relevance ranking**, never for filtering.
- There are **two** scope paths with **two different defaults**:
  - *Locate / traversal* (`search_symbols`, `find_usages`, `get_callers`) → `resolveQueryScope` → `QueryOptions{WorkspaceID, ProjectID}`. Defaults to the session **project**.
  - *Analyze / by-id / review* (`get_symbol`, `analyze`, `review`, sast) → `resolveRepoFilter` → a repo allow-set. Defaults to the whole **workspace**.
- `QueryOptions` has no repo dimension, so the locate path can't filter below project even though the session already knows the exact repo.

## Problem

1. **Default is too broad.** Working inside one repo, `search_symbols` returns hits from every repo in the project (or workspace). The noise scales with workspace size.
2. **The two paths are inconsistent.** They default differently (project vs workspace) and expose different overrides - `find_usages` can't take a `repo:`, and symbol search can't take a saved `scope:`.
3. **No single granularity is a good default for everything:**
   - *Locate* ("where is X defined / find a symbol") wants the **repo**.
   - *Reach* ("who consumes X / trace it") wants the **workspace** - anything narrower misses cross-project consumers of shared libraries.
   - *Project* is wrong as a default for **both**, but valuable as a deliberate selector.

That third point is the crux: a uniform repo-default would break reach queries on shared code, and a uniform workspace-default is the current noise. The breadth should follow the *intent of the tool*.

## Proposal

### Layer A - uniform override surface

Collapse the two scope paths into one resolver. Every scoped tool accepts `repo` / `project` / `workspace`(self only) / `scope`(saved), all intersected with the session workspace so they can only narrow, never escape isolation. Defaults stay exactly as today. This alone fixes the inconsistency (e.g. `repo:` on `find_usages`, `scope:` on search) and is non-breaking.

> **v1 status (analyze).** `analyze` now genuinely accepts the uniform `repo` / `project` / `workspace` / `scope` overrides across most of its kinds. The resolver (`resolveScope`) produces a `RepoAllow` folded into the scoped-node accessors (`scopedNodes` / `scopedNodesByKinds` / `scopedNodeSlice`) for the **AUTO** kinds. The three flagship kinds that read `s.graph` directly (`dead_code`, `hotspots`, `cycles`) and the broader **category-(a)** edge-walk / graph-algorithm / framework kinds (`channel_ops`, `pubsub`, `routes`, `models`, `pagerank`, `kcore`, `wcc`, `scc`, `edge_audit`, `tests_as_edges`, `doc_staleness`, `temporal_orphans`, …) apply an explicit per-row visibility filter (`analyzeNodeVisible`) — pruning rows / re-tallying counts against the workspace ceiling + optional repo allow-set. The **category-(c)** file/AST scans (`sast`, `hygiene`, `review`, `domain`, `named`, `unsafe_patterns`) reach `resolveRepoFilter` / `buildASTTargets`, which already enforce the same narrowing. Because `analyzeNodeVisible` gates on the session workspace before the repo allow-set, categories (a) and (c) are now **both repo-narrowed and workspace-isolated** — the latent cross-workspace leak is **closed** for them, not merely disclosed. The two kind-specific collisions are handled by stripping them from the resolution view: `kind=cross_repo` keeps `repo` as its boundary filter and `kind=cycles` keeps `scope` as a path/package prefix. **Caveat:** the remaining long-tail kinds — community detection (`clusters`, `concepts`, `suggest_boundaries`), git/disk-mining (`blame`, `coverage`, `fixes_history`, `retrieval_log`, `temporal_verify`), per-id (`would_create_cycle`, `def_use`), `synthesizers` / `resolution_outcomes`, and `sql_rebuild` — are workspace-bound but not repo-narrowed in v1 — a `scope_note` on the response discloses this when a narrowing arg is passed to such a kind. (Those category-(b)/(d) kinds also do not filter their own rows by the workspace ceiling, so the pre-existing cross-workspace leak remains open for them — still a documented follow-up.)

### Layer B - intent-based defaults (config-gated, shipped default on)

> **Shipped status (Layer B).** This landed gated by config `scope.intent_defaults` (env `GORTEX_SCOPE_INTENT_DEFAULTS`) and ships with the default **on** — an intentional departure from the "default off" framing in the original draft above. The repo owner chose to make the narrowed defaults the out-of-the-box behavior rather than an opt-in. **⚠ Behavior change vs. prior gortex behavior:** a query that takes no explicit `repo` / `project` / `workspace` / `scope` arg now defaults to a *narrowed* scope (locate → current repo, reach / analyze → workspace) instead of searching the whole index. The defaults are clamped to the session workspace and narrow-only — an explicit scope arg always overrides. To restore the pre-upgrade "search everything indexed" behavior, set `scope.intent_defaults: false` in `.gortex.yaml` or `GORTEX_SCOPE_INTENT_DEFAULTS=0`.

Classify each tool by intent and pick the default breadth from it:

| Intent | Tools | Default |
|---|---|---|
| **Locate** | `search_symbols`, `search_text`, `find_files` | current **repo** |
| **Reach** | `find_usages`, `get_callers`, `get_call_chain`, `contracts` | **workspace** |
| **Analyze** | `analyze`, `review`, sast | **workspace** |

`project` is never a default - it's reached explicitly. The whole default-change sits behind one config flag (`scope.intent_defaults`); as shipped it defaults **on**, so the narrowed defaults are the out-of-the-box behavior. Setting the flag off (`scope.intent_defaults: false` / `GORTEX_SCOPE_INTENT_DEFAULTS=0`) restores today's "search everything indexed" behavior.

## Why I think this fits the existing design

- The **workspace stays the hard boundary** - `nodeInSessionScope` / `scopedNodes` are untouched. This only changes default breadth *within* the ceiling; every explicit arg is still clamped to it.
- It reads like a **completion of the existing tool classification**: `search_symbols` is already declared a repo-scoped tool (`ScopeRepo` in the scope map), and `ResolveToolScope` already exists but is currently wired only for the workspace-level tools. Layer B brings the runtime in line with that declared intent. (Is that the direction you intended? That assumption is doing a lot of work here.)
- Layer A is capability-only. Layer B is config-gated — as shipped it defaults **on** (an intentional behavior change), and opting out (`scope.intent_defaults: false`) restores the old breadth.

## Abstract example

> Workspace `acme` indexes two service groups and a shared layer:
> - project `payments`: `payments-api`, `payments-worker`
> - project `storefront`: `storefront-web`, `storefront-bff`
> - project `platform` (shared libs used by **both**): `platform-auth`, `platform-logging`
>
> - **Locate** - in `payments-api`, `search_symbols "RetryPolicy"` should default to `payments-api`, not all six repos.
> - **Reach** - `find_usages` of `platform-auth`'s `VerifyToken` must default to the **whole workspace**; a project default would hide every `storefront` consumer. This is precisely why reach can't default to project.
> - **Project as explicit override** - `find_usages VerifyToken project:payments` to deliberately scope to one group.
>
> The shared `platform` repos are also why `acme` must be a single workspace: a hard boundary can't be shared by two groups.

## Alternatives considered

- **Per-repo projects (config only).** Giving each repo a unique `project:` narrows symbol search to one repo - but it collapses the project group, so `find_usages` of a shared-lib symbol shrinks to that lib's own repo with no group left to widen to. Wrong for shared-library consumer discovery.
- **Split into smaller workspaces.** A library used by two groups can live in only one hard-boundary workspace, which breaks cross-group reach entirely.
- **Instruction-only** (tell agents to always pass `repo:`). Fragile - the default stays broad, and one forgotten arg is back to the noise.

These are why the fix wants to be *per-tool defaults in code* rather than a config or convention workaround.

## Backward compatibility / rollout

- **Layer A**: no behavior change; can merge on its own.
- **Layer B**: one config flag, `scope.intent_defaults` (env `GORTEX_SCOPE_INTENT_DEFAULTS`). **Shipped default on** — an intentional behavior change: queries now default to a narrowed scope (locate → repo, reach / analyze → workspace) instead of the whole index. Opt out per user with `scope.intent_defaults: false` / `GORTEX_SCOPE_INTENT_DEFAULTS=0` to restore the prior "search everything indexed" behavior.

## Open questions for you

1. Is **repo-default-for-locate** a direction you'd want, or do you prefer scope to remain an explicit choice everywhere?
2. Config flag name, and default on vs off. *(Resolved: `scope.intent_defaults` / `GORTEX_SCOPE_INTENT_DEFAULTS`, shipped default on — see "Shipped status (Layer B)" above.)*
3. Land as one PR (two commits: A then B) or two PRs?
4. Is my read of `ScopeRepo` / `ResolveToolScope` as "the intended-but-not-yet-wired direction" correct, or were those meant for something else?

A reference implementation is ready to accompany this if the direction looks right.
