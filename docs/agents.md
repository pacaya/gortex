# Agent Integrations

`gortex init` auto-configures Gortex for every AI coding assistant it
detects on your machine. Fifteen adapters ship today.

Run `gortex init doctor` to see what's currently configured. Run
`gortex init --agents=<csv>` to constrain setup to a specific subset,
or `--agents-skip=<csv>` to exclude one.

## Adapter matrix

| Name            | What gets written                                            | Mode       | Docs link                                                           |
| --------------- | ------------------------------------------------------------ | ---------- | ------------------------------------------------------------------- |
| `claude-code`   | `.mcp.json`, `.claude/*`, `CLAUDE.md`, `~/.claude/skills/*`  | both       | https://docs.claude.com/en/docs/claude-code/overview                |
| `aider`         | `.aiderignore` block                                         | project    | https://aider.chat/docs/config/aider_conf.html                      |
| `antigravity`   | `~/.gemini/antigravity/mcp_config.json` + Knowledge Item     | user       | https://antigravity.google/docs/mcp                                 |
| `cline`         | `cline_mcp_settings.json` (per VS Code / Cursor globalStorage) | user     | https://docs.cline.bot/mcp/mcp-overview                             |
| `codex`         | `~/.codex/config.toml` (`[mcp_servers.gortex]`)              | user       | https://developers.openai.com/codex/mcp                             |
| `continue`      | `.continue/mcpServers/gortex.json`                           | project    | https://docs.continue.dev/customize/deep-dives/mcp                  |
| `cursor`        | `.cursor/mcp.json` (project) or `~/.cursor/mcp.json`         | both       | https://docs.cursor.com/en/context/mcp                              |
| `gemini`        | `.gemini/settings.json` or `~/.gemini/settings.json`         | both       | https://geminicli.com/docs/tools/mcp-server/                        |
| `kilocode`      | `mcp_settings.json` + `.kilocode/mcp.json`                   | both       | https://kilo.ai/docs/features/mcp/using-mcp-in-kilo-code            |
| `kiro`          | `.kiro/settings/mcp.json` + steering/hooks or user-level     | both       | https://kiro.dev/docs/mcp/configuration                             |
| `opencode`      | `.opencode/config.json`                                      | project    | https://opencode.ai/docs/mcp                                        |
| `openclaw`      | `~/.openclaw/openclaw.json` (`mcp.servers.gortex`)           | user       | https://docs.openclaw.ai/cli/mcp                                    |
| `vscode`        | `.vscode/mcp.json` (`servers` key, 1.102+ schema)            | project    | https://code.visualstudio.com/docs/copilot/chat/mcp-servers         |
| `windsurf`      | `~/.codeium/mcp_config.json`                                 | user       | https://docs.windsurf.com/plugins/cascade/mcp                       |
| `zed`           | OS-specific `settings.json` (`context_servers`)              | user       | https://zed.dev/docs/ai/mcp                                         |

Mode legend: **project** writes inside the repo; **user** writes
under `$HOME`; **both** respects `--global` to choose.

## Common CLI flags

```
gortex init                          # interactive, default mode
gortex init --yes                    # skip wizard, use defaults
gortex init --agents=claude-code,cursor     # allow-list
gortex init --agents-skip=antigravity       # block-list
gortex init --dry-run --json         # plan, emit JSON report
gortex init --force                  # overwrite merge-preserved keys
gortex init --hooks-only             # refresh Claude Code hooks only
gortex init --global                 # user-wide install + daemon wiring
gortex init doctor                   # observe-only report
gortex init doctor --json            # machine-readable report
```

## Adapter contract

Every adapter under `internal/agents/<name>/` implements the
`agents.Adapter` interface:

- `Name()` — stable identifier used by `--agents`
- `DocsURL()` — upstream docs link (for `--json` reports)
- `Detect(env)` — cheap filesystem/`PATH` probe; never writes
- `Plan(env)` — returns the set of files Apply *would* touch,
  without writing
- `Apply(env, opts)` — performs the writes, respecting
  `opts.DryRun` and `opts.Force`

Every write funnels through `agents.WriteIfNotExists`,
`agents.MergeJSON`, or `agents.MergeTOML`. Those helpers provide:

- Atomic temp-file-plus-rename — a partial failure can't leave a
  half-written config
- Uniform dry-run handling — no adapter has its own bool
- Structured `FileAction` results — `--json` and doctor speak the
  same vocabulary
- Malformed-file backup — a user with broken JSON gets a `.bak`
  sibling instead of silent data loss

## Per-agent notes

### claude-code

The primary integration. Writes six artifacts in project mode
(`.mcp.json`, `.claude/commands/gortex-*.md`, `.claude/settings.json`,
`.claude/settings.local.json`, `CLAUDE.md`, `~/.claude/skills/gortex-*`).
Hooks installed today: **PreToolUse**, **PreCompact**, **Stop**,
**SessionStart** — the SessionStart handler fires on new or resumed
sessions to prime the first turn with graph orientation, complementing
PreCompact which fires on summary boundaries.

Global mode (`--global`) writes `~/.claude.json` (user-level MCP) and
`~/.claude/settings.local.json` (user-level hooks) instead, so every
project Claude Code opens uses the shared daemon.

### aider

Aider has no native MCP client today. We install an `.aiderignore`
block telling Aider to skip Gortex's cache dirs so it doesn't waste
tokens ingesting them.

### antigravity

Two artifacts: a native MCP registration at
`~/.gemini/antigravity/mcp_config.json` (new in 2026) plus a
Knowledge Item at `~/.gemini/antigravity/knowledge/gortex-workflow/`
that documents how to use Gortex via `run_command`. The KI stays
because it gives workflow intent the raw MCP registration doesn't.

### cline

Extension ID `saoudrizwan.claude-dev`. We write
`cline_mcp_settings.json` to each VS Code and Cursor globalStorage
directory that exists. Auto-approval field is `alwaysAllow` (not
`autoApprove`, which is a different field in the schema).

### codex

OpenAI Codex CLI stores config in `~/.codex/config.toml`. We
upsert a `[mcp_servers.gortex]` table there.

### continue

Continue.dev still accepts JSON block files under
`.continue/mcpServers/` even though its native format is YAML with
metadata headers. We write the JSON form today for zero-dependency
simplicity; upgrading to the YAML+metadata form is tracked.

### cursor

Project-level `.cursor/mcp.json` by default; `~/.cursor/mcp.json`
when `--global` is set. Env key is `env` (not `environment`).

### gemini

Gemini CLI reads `.gemini/settings.json` (project) and
`~/.gemini/settings.json` (user). Distinct from the antigravity
adapter despite the shared `~/.gemini/` prefix.

### kilocode

Kilo Code is a Cline fork with its own globalStorage key
(`kilocode.kilo`). We write to every candidate globalStorage path
(VS Code + Cursor + Insiders variants) plus `.kilocode/mcp.json`
when a project-level directory exists.

### kiro

Workspace `.kiro/settings/mcp.json` + steering/hooks in project
mode; `~/.kiro/settings/mcp.json` only in global mode (steering
and hooks are project-scoped in Kiro's runtime). The MCP entry
carries `autoApprove` and explicit `disabled: false` keys Kiro's
UI expects.

### opencode

OpenCode's schema differs from the canonical form: top-level
`mcp.<name>` (not `mcpServers`), `command` is an array, env key
is `environment`, plus a `$schema` pointer at
`https://opencode.ai/config.json`.

### openclaw

Config lives at `~/.openclaw/openclaw.json`. OpenClaw advertises
JSON5 but accepts strict JSON, which is what we emit. Servers go
under `mcp.servers.<name>`.

### vscode

**Schema changed in 2026.** VS Code's native MCP runtime (1.102+)
uses `{"servers": {...}}`, not the Copilot-Chat legacy
`{"mcpServers": {...}}`. `type` is inferred from `command`
presence, so stdio servers don't need a type field.

### windsurf

**Path changed in 2026.** Current canonical path is
`~/.codeium/mcp_config.json`. The legacy
`~/.codeium/windsurf/mcp_config.json` is left in place unless
`--force` is passed, which removes it as part of the migration.

### zed

Zed calls its MCP registry `context_servers`, not `mcpServers`.
Settings file is platform-specific:

- macOS: `~/Library/Application Support/Zed/settings.json`
- Linux: `~/.config/zed/settings.json`
- Windows: `%APPDATA%\Zed\settings.json`

Each entry takes `source: "custom"` alongside the usual
`command/args/env`.

## Troubleshooting

- **Config file malformed**: If an adapter finds invalid JSON/TOML
  it writes a `.bak` sibling before replacing the file with the
  merged result. Check alongside the original.
- **Hook command points at `/tmp/…`**: `gortex init` heals stale
  ephemeral paths automatically on re-run.
- **"Already configured" but tools missing**: re-run with
  `--force` to overwrite our entries; or delete the `gortex`
  stanza from the config and re-run without `--force`.
- **CI / scripted install**: pass `--yes --json` and parse the
  report.
