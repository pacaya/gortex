// Package cursor implements the Gortex init integration for
// Cursor. Writes .cursor/mcp.json (project-level) and, when --global
// is effective, ~/.cursor/mcp.json (user-level).
//
// Schema: standard {"mcpServers": {<name>: {command, args, env}}}.
package cursor

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/agents/internalutil"
)

const Name = "cursor"
const DocsURL = "https://docs.cursor.com/en/context/mcp"

type Adapter struct{}

func New() *Adapter                { return &Adapter{} }
func (a *Adapter) Name() string    { return Name }
func (a *Adapter) DocsURL() string { return DocsURL }

// cursorUserDataDir is the OS-default directory where Cursor stores
// settings when the CLI has never created ~/.cursor/. Detecting it
// avoids false negatives for users who use the app daily but have no
// `cursor` shell helper on PATH yet.
func cursorUserDataDir(home string) string {
	if home == "" {
		return ""
	}
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "Cursor")
	case "windows":
		return filepath.Join(home, "AppData", "Roaming", "Cursor")
	default:
		// Linux and other Unix targets Cursor ships for.
		return filepath.Join(home, ".config", "Cursor")
	}
}

// Detect succeeds when any of: project has .cursor/, user has
// ~/.cursor/, Cursor's application data directory exists, or the
// `cursor` CLI is on PATH.
func (a *Adapter) Detect(env agents.Env) (bool, error) {
	if _, err := os.Stat(filepath.Join(env.Root, ".cursor")); err == nil {
		return true, nil
	}
	if env.Home != "" {
		if _, err := os.Stat(filepath.Join(env.Home, ".cursor")); err == nil {
			return true, nil
		}
		if dir := cursorUserDataDir(env.Home); dir != "" {
			if _, err := os.Stat(dir); err == nil {
				return true, nil
			}
		}
	}
	if p, err := exec.LookPath("cursor"); err == nil && p != "" {
		return true, nil
	}
	return false, nil
}

func (a *Adapter) Plan(env agents.Env) (*agents.Plan, error) {
	p := &agents.Plan{Files: []agents.FileAction{
		{Path: mcpConfigPath(env), Action: agents.ActionWouldMerge, Keys: []string{"mcpServers"}},
	}}
	if env.Mode != agents.ModeGlobal {
		p.Files = append(p.Files, agents.FileAction{
			Path: workflowRulePath(env), Action: agents.ActionWouldCreate,
			Keys: []string{"workflow-rule"},
		})
	}
	if env.Mode != agents.ModeGlobal && env.SkillsRouting != "" {
		p.Files = append(p.Files, agents.FileAction{
			Path: communitiesRulePath(env), Action: agents.ActionWouldCreate,
			Keys: []string{"communities-rule"},
		})
	}
	return p, nil
}

// mcpConfigPath returns the mcp.json path for the given mode.
// Project mode: .cursor/mcp.json; global mode: ~/.cursor/mcp.json.
// Cursor reads both and prefers project when a key is defined in
// both.
func mcpConfigPath(env agents.Env) string {
	if env.Mode == agents.ModeGlobal && env.Home != "" {
		return filepath.Join(env.Home, ".cursor", "mcp.json")
	}
	return filepath.Join(env.Root, ".cursor", "mcp.json")
}

// communitiesRulePath returns the project-scoped MDC file carrying
// the regenerated community-routing block. Gortex owns this file
// end-to-end; each `gortex init` overwrites it.
//
// Cursor does not support user-level MDC rules (those live in the
// app's Settings UI), so this is always project-scoped.
func communitiesRulePath(env agents.Env) string {
	return filepath.Join(env.Root, ".cursor", "rules", "gortex-communities.mdc")
}

// workflowRulePath is the stable Cursor rule that steers the agent
// toward Gortex MCP tools. Regenerated on every `gortex init` so
// wording stays aligned with shipped analyzers.
func workflowRulePath(env agents.Env) string {
	return filepath.Join(env.Root, ".cursor", "rules", "gortex-workflow.mdc")
}

// workflowRuleBody is intentionally concise: Cursor injects this on
// every turn (alwaysApply) and we want high signal without drowning
// project-specific rules.
const workflowRuleBody = `## Gortex in Cursor

This repository wires the **gortex** MCP server via .cursor/mcp.json (merge-managed by Gortex).

**MANDATORY: use graph tools, not blind file reads**

You **MUST** prefer Gortex graph queries over text search and whole-file opens on every task. These are not suggestions.

- **Start** a new chat with **index_health** to confirm the daemon/index (cheap); use **graph_stats** only when you need node/edge counts or multi-repo orientation.
- **Use** **search_symbols**, **get_symbol_source**, **get_file_summary**, **get_call_chain**, **find_usages**, and **smart_context** instead of opening whole files or guessing with text search.
- Before any signature or API change, **run** **verify_change**; for test selection **run** **get_test_targets**.

**MANDATORY: session memory**

- **At session start**, call **distill_session** to recover decisions, pinned notes, and recent excerpts saved in prior sessions in this workspace.
- **At every decision point** (picking an approach, rejecting an alternative, spotting a non-obvious constraint), call **save_note** with ` + "`tags:\"decision\"`" + ` and mention affected symbol IDs in the body — they auto-link.
- **Before editing a symbol you've touched before**, call **query_notes** with ` + "`symbol_id:\"<id>\"`" + ` to surface prior warnings and decisions.

**MANDATORY: development memories (cross-session)**

- **Immediately after smart_context** on every task, call **surface_memories** with ` + "`task:\"<task>\"`" + ` and ` + "`symbol_ids:\"<top hits>\"`" + ` — returns memories ranked by anchor overlap, importance, pinning, recency.
- **When you find a durable invariant, gotcha, or decision worth teaching the team**, call **store_memory** with ` + "`kind:\"<invariant|gotcha|convention|decision>\"`" + `, ` + "`symbol_ids:\"<id>\"`" + `, ` + "`importance:5`" + `. Pin load-bearing memories. Use ` + "`supersedes:\"<old-id>\"`" + ` when newer knowledge replaces older.
- Memories are workspace-wide and outlive sessions, agents, and teammates — every future agent inherits them.
`

func (a *Adapter) Apply(env agents.Env, opts agents.ApplyOpts) (*agents.Result, error) {
	res := &agents.Result{Name: Name, DocsURL: DocsURL}
	detected, _ := a.Detect(env)
	res.Detected = detected
	if !detected && !opts.ForceDetect {
		internalutil.Logf(env.Stderr, "[gortex init] skip Cursor setup (Cursor not detected)")
		return res, nil
	}
	internalutil.Logf(env.Stderr, "[gortex init] setting up Cursor IDE integration...")

	path := mcpConfigPath(env)
	action, err := agents.MergeJSON(env.Stderr, path, func(root map[string]any, _ bool) (bool, error) {
		// Global ~/.cursor/mcp.json is read by Cursor across every
		// project, so cwd at MCP-launch time is unrelated to the open
		// repo (typically $HOME). Use the daemon-proxy entry; never
		// the legacy `--index .` shape that fails the entry-point
		// handshake on home. See gortexhq/gortex#19.
		if env.Mode == agents.ModeGlobal {
			return agents.UpsertMCPServerWithMigration(root, "gortex", agents.GlobalGortexMCPEntry(), opts), nil
		}
		return agents.UpsertMCPServer(root, "gortex", agents.DefaultGortexMCPEntry(), opts), nil
	}, opts)
	if err != nil {
		return res, err
	}
	res.Files = append(res.Files, action)

	// Workflow MDC — project-local only (Cursor has no user-level MDC
	// on disk). Tells the agent to reach for MCP graph tools first.
	if env.Mode != agents.ModeGlobal {
		wfBody := agents.CursorMDCFrontmatter(workflowRuleBody)
		wfAction, err := agents.WriteOwnedFile(env.Stderr, workflowRulePath(env), wfBody, opts)
		if err != nil {
			return res, err
		}
		res.Files = append(res.Files, wfAction)
	}

	// Community-routing MDC file — always written fresh on init
	// so the routing tracks the current graph. Skipped in global
	// mode (file is per-repo) and when --no-skills / no
	// communities qualify.
	if env.Mode != agents.ModeGlobal && env.SkillsRouting != "" {
		body := agents.CursorMDCFrontmatter(
			agents.CommunitiesStartMarker + "\n" + env.SkillsRouting + "\n" + agents.CommunitiesEndMarker + "\n")
		ruleAction, err := agents.WriteOwnedFile(env.Stderr, communitiesRulePath(env), body, opts)
		if err != nil {
			return res, err
		}
		res.Files = append(res.Files, ruleAction)
	}

	res.Configured = true
	return res, nil
}
