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
// when hooks are enabled, a UserPromptSubmit hook that shells `gortex hook
// --agent=kimi`.
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

const kimiHookTimeoutSeconds = 5

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
			return upsertUserPromptSubmitHook(root, env, opts), nil
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

func upsertUserPromptSubmitHook(root map[string]any, env agents.Env, opts agents.ApplyOpts) bool {
	existing, ok := kimiHookList(root["hooks"])
	if !ok {
		return false
	}

	found := false
	kept := make([]any, 0, len(existing)+1)
	for _, entry := range existing {
		if kimiHookEntryInvokesKimi(entry) {
			found = true
			if opts.Force {
				continue
			}
		}
		kept = append(kept, entry)
	}
	if found && !opts.Force {
		return false
	}

	root["hooks"] = append(kept, kimiUserPromptSubmitHook(env))
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

func kimiUserPromptSubmitHook(env agents.Env) map[string]any {
	return map[string]any{
		"event":   "UserPromptSubmit",
		"command": kimiHookCommand(env),
		"timeout": kimiHookTimeoutSeconds,
	}
}

func kimiHookCommand(env agents.Env) string {
	base := strings.TrimSpace(env.HookCommand)
	if base == "" {
		base = "gortex hook"
	}
	return base + " --agent=kimi"
}

func kimiHookEntryInvokesKimi(entry any) bool {
	m, ok := entry.(map[string]any)
	if !ok {
		return false
	}
	event, _ := m["event"].(string)
	if event != "UserPromptSubmit" {
		return false
	}
	cmd, _ := m["command"].(string)
	return kimiCommandInvokesKimiHook(cmd)
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
