// Package kimi implements the Gortex init/install integration for Kimi Code
// CLI. Kimi keeps MCP server declarations in mcp.json and user-level lifecycle
// hooks in config.toml:
//
//   - project MCP: .kimi-code/mcp.json
//   - user MCP:    $KIMI_CODE_HOME/mcp.json, defaulting to ~/.kimi-code/mcp.json
//   - user hooks:  $KIMI_CODE_HOME/config.toml, defaulting to ~/.kimi-code/config.toml
//
// Kimi's hooks are user-level today, so project-mode `gortex init` only writes
// the repo-local MCP file. Machine-wide `gortex install` writes user MCP and,
// when hooks are enabled, UserPromptSubmit / PreToolUse / Stop / SubagentStart
// hooks that shell `gortex hook --agent=kimi` for pre-turn context injection,
// graph-aware tool redirects, post-turn self-correction, and subagent briefing.
//
// Docs: https://www.kimi.com/code/docs/en/kimi-code-cli/customization/hooks.html
package kimi

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/agents/internalutil"
)

const Name = "kimi"
const DocsURL = "https://www.kimi.com/code/docs/en/kimi-code-cli/customization/hooks.html"

// Per-event hook timeouts (Kimi allows 1–600s). UserPromptSubmit and
// PreToolUse sit on the turn's critical path, so they stay snappy; the Stop and
// SubagentStart briefings fan out to several graph tools and run off the hot
// path, so they get more headroom before Kimi's fail-open kill.
const (
	kimiHookTimeoutSeconds         = 5
	kimiStopHookTimeoutSeconds     = 30
	kimiSubagentHookTimeoutSeconds = 15
)

type Adapter struct{}

func New() *Adapter                { return &Adapter{} }
func (a *Adapter) Name() string    { return Name }
func (a *Adapter) DocsURL() string { return DocsURL }

// Detect checks for the kimi CLI on PATH or an existing Kimi Code home.
func (a *Adapter) Detect(env agents.Env) (bool, error) {
	if p, err := exec.LookPath("kimi"); err == nil && p != "" {
		return true, nil
	}
	if home := kimiConfigRoot(env); home != "" {
		if _, err := os.Stat(home); err == nil {
			return true, nil
		}
	}
	return false, nil
}

func (a *Adapter) Plan(env agents.Env) (*agents.Plan, error) {
	p := &agents.Plan{}
	if env.Mode == agents.ModeGlobal {
		home := kimiConfigRoot(env)
		if home == "" {
			return p, nil
		}
		p.Files = append(p.Files, agents.FileAction{
			Path:   filepath.Join(home, "mcp.json"),
			Action: agents.ActionWouldMerge,
			Keys:   []string{"mcpServers"},
		})
		if env.InstallHooks {
			p.Files = append(p.Files, agents.FileAction{
				Path:   filepath.Join(home, "config.toml"),
				Action: agents.ActionWouldMerge,
				Keys:   []string{"hooks"},
			})
		}
		return p, nil
	}

	p.Files = append(p.Files, agents.FileAction{
		Path:   projectMCPPath(env),
		Action: agents.ActionWouldMerge,
		Keys:   []string{"mcpServers"},
	})
	return p, nil
}

func (a *Adapter) Apply(env agents.Env, opts agents.ApplyOpts) (*agents.Result, error) {
	res := &agents.Result{Name: Name, DocsURL: DocsURL}
	detected, _ := a.Detect(env)
	res.Detected = detected
	if !detected && !opts.ForceDetect {
		internalutil.Logf(env.Stderr, "[gortex init] skip Kimi Code CLI setup (kimi not detected)")
		return res, nil
	}

	if env.Mode == agents.ModeGlobal {
		return a.applyGlobal(env, opts, res)
	}
	return a.applyProject(env, opts, res)
}

func (a *Adapter) applyProject(env agents.Env, opts agents.ApplyOpts, res *agents.Result) (*agents.Result, error) {
	if env.Root == "" {
		return res, fmt.Errorf("kimi: requires a repository root")
	}
	internalutil.Logf(env.Stderr, "[gortex init] setting up Kimi Code CLI project MCP integration...")

	action, err := agents.MergeJSON(env.Stderr, projectMCPPath(env), func(root map[string]any, _ bool) (bool, error) {
		return agents.UpsertMCPServer(root, "gortex", agents.DefaultGortexMCPEntry(), opts), nil
	}, opts)
	if err != nil {
		return res, err
	}
	res.Files = append(res.Files, action)
	res.Configured = true
	return res, nil
}

func (a *Adapter) applyGlobal(env agents.Env, opts agents.ApplyOpts, res *agents.Result) (*agents.Result, error) {
	home := kimiConfigRoot(env)
	if home == "" {
		return res, fmt.Errorf("kimi: requires KIMI_CODE_HOME or a resolved home directory")
	}
	internalutil.Logf(env.Stderr, "[gortex install] setting up Kimi Code CLI user integration...")

	mcpAction, err := agents.MergeJSON(env.Stderr, filepath.Join(home, "mcp.json"), func(root map[string]any, _ bool) (bool, error) {
		return agents.UpsertMCPServer(root, "gortex", agents.DefaultGortexMCPEntry(), opts), nil
	}, opts)
	if err != nil {
		return res, err
	}
	res.Files = append(res.Files, mcpAction)

	if env.InstallHooks {
		hookAction, err := agents.MergeTOML(env.Stderr, filepath.Join(home, "config.toml"), func(root map[string]any, _ bool) (bool, error) {
			return upsertKimiHooks(root, env, opts), nil
		}, opts)
		if err != nil {
			return res, err
		}
		if hookAction.Action != agents.ActionSkip {
			hookAction.Keys = []string{"hooks"}
		}
		res.Files = append(res.Files, hookAction)
	}

	res.Configured = true
	return res, nil
}

func projectMCPPath(env agents.Env) string {
	return filepath.Join(env.Root, ".kimi-code", "mcp.json")
}

func kimiConfigRoot(env agents.Env) string {
	if home := strings.TrimSpace(os.Getenv("KIMI_CODE_HOME")); home != "" {
		return home
	}
	if env.Home == "" {
		return ""
	}
	return filepath.Join(env.Home, ".kimi-code")
}

func upsertKimiHooks(root map[string]any, env agents.Env, opts agents.ApplyOpts) bool {
	existing, ok := kimiHookList(root["hooks"])
	if !ok {
		return false
	}

	found := make(map[string]bool)
	kept := make([]any, 0, len(existing)+2)
	for _, entry := range existing {
		if event, ok := kimiHookEntryInvokesKimi(entry); ok {
			found[event] = true
			if opts.Force {
				continue
			}
		}
		kept = append(kept, entry)
	}

	changed := false
	for _, hook := range kimiHooks(env) {
		event, _ := hook["event"].(string)
		if found[event] && !opts.Force {
			continue
		}
		kept = append(kept, hook)
		changed = true
	}
	if !changed {
		return false
	}

	root["hooks"] = kept
	return true
}

func kimiHookList(v any) ([]any, bool) {
	if v == nil {
		return nil, true
	}
	switch list := v.(type) {
	case []any:
		return append([]any(nil), list...), true
	case []map[string]any:
		out := make([]any, 0, len(list))
		for _, entry := range list {
			out = append(out, entry)
		}
		return out, true
	default:
		return nil, false
	}
}

func kimiHooks(env agents.Env) []map[string]any {
	return []map[string]any{
		kimiUserPromptSubmitHook(env),
		kimiPreToolUseHook(env),
		kimiStopHook(env),
		kimiSubagentStartHook(env),
	}
}

func kimiUserPromptSubmitHook(env agents.Env) map[string]any {
	return map[string]any{
		"event":   "UserPromptSubmit",
		"command": kimiHookCommand(env),
		"timeout": kimiHookTimeoutSeconds,
	}
}

func kimiPreToolUseHook(env agents.Env) map[string]any {
	// Do not set matcher here: Kimi currently exposes MCP tool names as the
	// underlying tool name without a documented server namespace, and the
	// native Read/Grep/Glob redirects want to see every tool call. The
	// dispatcher does the filtering and silently ignores anything it doesn't
	// enrich.
	return map[string]any{
		"event":   "PreToolUse",
		"command": kimiHookCommand(env),
		"timeout": kimiHookTimeoutSeconds,
	}
}

// kimiStopHook runs the post-turn diagnostics (changed symbols → test targets,
// guards, dead code, coverage, contracts) and feeds them back so the agent can
// self-correct before handoff.
func kimiStopHook(env agents.Env) map[string]any {
	return map[string]any{
		"event":   "Stop",
		"command": kimiHookCommand(env),
		"timeout": kimiStopHookTimeoutSeconds,
	}
}

// kimiSubagentStartHook briefs a spawned subagent with task-scoped graph
// context and the tool-swap table so it doesn't default to raw Read/Grep.
func kimiSubagentStartHook(env agents.Env) map[string]any {
	return map[string]any{
		"event":   "SubagentStart",
		"command": kimiHookCommand(env),
		"timeout": kimiSubagentHookTimeoutSeconds,
	}
}

func kimiHookCommand(env agents.Env) string {
	base := strings.TrimSpace(env.HookCommand)
	if base == "" {
		base = "gortex hook"
	}
	return base + " --agent=kimi"
}

func kimiHookEntryInvokesKimi(entry any) (string, bool) {
	m, ok := entry.(map[string]any)
	if !ok {
		return "", false
	}
	event, _ := m["event"].(string)
	switch event {
	case "UserPromptSubmit", "PreToolUse", "Stop", "SubagentStart":
	default:
		return "", false
	}
	cmd, _ := m["command"].(string)
	return event, kimiCommandInvokesKimiHook(cmd)
}

func kimiCommandInvokesKimiHook(cmd string) bool {
	fields := strings.Fields(cmd)
	if len(fields) < 3 {
		return false
	}
	if !strings.Contains(strings.ToLower(fields[0]), "gortex") {
		return false
	}
	hasHook := false
	hasAgent := false
	for i, f := range fields[1:] {
		if f == "hook" {
			hasHook = true
		}
		if f == "--agent=kimi" {
			hasAgent = true
		}
		if f == "--agent" && i+2 < len(fields) && fields[i+2] == "kimi" {
			hasAgent = true
		}
	}
	return hasHook && hasAgent
}
