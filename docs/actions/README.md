# Pull-request actions

The daemon exposes a small set of data-only MCP tools over your repository's
pull requests:

- **`list_prs`** — list a repo's PRs with a one-shot review-state
  classification (DRAFT / BASE_MISMATCH / CHANGES_REQUESTED / APPROVED /
  STALE / READY), a normalized CI rollup, and per-PR merge blockers.
- **`get_pr_impact`** — map a PR's changed files to the symbols they define,
  score PR-level risk across five axes, and group the affected surface by
  community and by caller/test file. Set `receipt: true` for a small,
  privacy-safe review receipt.
- **`triage_prs`** — rank a repo's open PRs by graph-derived review priority
  (highest risk first, deterministic).

These tools are **read-only** — none of them edits code or posts to GitHub.

## Providing a GitHub token to the daemon

The daemon **self-serves** PR data: it pairs a GitHub token with the repo
identity it already indexed, so there is no CLI-versus-daemon auth split and
no dependency on a `gh` CLI login. All it needs is a token in **the daemon's
own environment**.

The token resolves from, in order:

1. `GH_TOKEN`
2. `GITHUB_TOKEN`

Set one of these in the environment the daemon process runs under — not in
your interactive shell, unless the daemon inherits it. For example, when
starting the daemon manually:

```bash
GH_TOKEN=ghp_xxxxxxxxxxxxxxxxxxxx gortex daemon start --detach
```

For a long-running daemon managed by a service supervisor, set the variable in
that unit's environment (e.g. a systemd `Environment=` line, a launchd
`EnvironmentVariables` entry, or your process manager's env file) so it is
present every time the daemon starts.

GitHub Enterprise: when `GITHUB_API_URL` or `GH_HOST` names a non-`github.com`
host, the forge client targets that Enterprise API base automatically. The
same `GH_TOKEN` / `GITHUB_TOKEN` resolution applies.

In CI, a per-PR Action's `GITHUB_TOKEN` is picked up automatically — no extra
configuration is needed.

## When no token is available

If no token is resolvable **and** you did not supply already-fetched data,
each tool degrades gracefully instead of failing:

```json
{ "error": "forge unavailable",
  "hint": "set GH_TOKEN (or GITHUB_TOKEN) in the daemon environment" }
```

A GitHub rate-limit is surfaced as a typed degradation carrying the
Retry-After hint:

```json
{ "error": "rate limited", "retry_after_s": 42 }
```

## Skipping the network with caller-supplied data

Every tool accepts an optional caller-supplied data path so an agent (or a CLI
front-end) that already fetched the PR data can avoid a refetch:

- `list_prs` accepts `prs` — a JSON array of already-fetched PR objects.
- `get_pr_impact` accepts `files` — a JSON array of changed file paths.
- `triage_prs` accepts `prs` and/or `files` (a JSON object mapping a PR
  number to its changed file paths).

When supplied data is present, the tool classifies / scores it directly and
makes no network call. Triage additionally caches each fetched PR for a short
window so a re-run within the window does not refetch the same PR.
