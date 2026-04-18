// Package vscode implements the Gortex init integration for
// Visual Studio Code's native MCP runtime (1.102+). VS Code's
// schema differs from the Claude / Cursor canonical shape in two
// ways:
//
//   - top-level key is "servers" (not "mcpServers")
//   - stdio is the inferred default; no "type" field is required
//
// The VS Code docs document both project-level (.vscode/mcp.json)
// and user-level configurations; the user-level file lives under
// the platform-specific VS Code profile directory and is accessed
// via the "MCP: Open User Configuration" command. We write the
// project-level file today and leave user-level to a follow-up once
// the official per-OS paths are confirmed.
//
// Docs: https://code.visualstudio.com/docs/copilot/chat/mcp-servers
package vscode

import (
	"os"
	"os/exec"
	"path/filepath"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/agents/internalutil"
)

const Name = "vscode"
const DocsURL = "https://code.visualstudio.com/docs/copilot/chat/mcp-servers"

type Adapter struct{}

func New() *Adapter                { return &Adapter{} }
func (a *Adapter) Name() string    { return Name }
func (a *Adapter) DocsURL() string { return DocsURL }

func (a *Adapter) Detect(env agents.Env) (bool, error) {
	if _, err := os.Stat(filepath.Join(env.Root, ".vscode")); err == nil {
		return true, nil
	}
	if p, err := exec.LookPath("code"); err == nil && p != "" {
		return true, nil
	}
	if env.Home != "" {
		candidates := []string{
			filepath.Join(env.Home, "Library", "Application Support", "Code"),
			filepath.Join(env.Home, ".config", "Code"),
			filepath.Join(env.Home, ".vscode"),
		}
		for _, dir := range candidates {
			if _, err := os.Stat(dir); err == nil {
				return true, nil
			}
		}
	}
	return false, nil
}

func (a *Adapter) Plan(env agents.Env) (*agents.Plan, error) {
	return &agents.Plan{Files: []agents.FileAction{{
		Path:   filepath.Join(env.Root, ".vscode", "mcp.json"),
		Action: agents.ActionWouldMerge,
		Keys:   []string{"servers"},
	}}}, nil
}

func (a *Adapter) Apply(env agents.Env, opts agents.ApplyOpts) (*agents.Result, error) {
	res := &agents.Result{Name: Name, DocsURL: DocsURL}
	// Global mode skips this adapter — user-level VS Code MCP
	// config lives at a platform-specific path (via the "MCP: Open
	// User Configuration" command) which we don't resolve today.
	if env.Mode == agents.ModeGlobal {
		return res, nil
	}
	detected, _ := a.Detect(env)
	res.Detected = detected
	if !detected {
		internalutil.Logf(env.Stderr, "[gortex init] skip VS Code / Copilot setup (VS Code not detected)")
		return res, nil
	}
	internalutil.Logf(env.Stderr, "[gortex init] setting up VS Code / GitHub Copilot integration...")

	path := filepath.Join(env.Root, ".vscode", "mcp.json")
	action, err := agents.MergeJSON(env.Stderr, path, func(root map[string]any, _ bool) (bool, error) {
		servers, ok := root["servers"].(map[string]any)
		if !ok {
			servers = make(map[string]any)
		}
		if _, exists := servers["gortex"]; exists && !opts.Force {
			return false, nil
		}
		// VS Code's native MCP runtime (1.102+) infers stdio from
		// command/args presence, so no "type" field is needed. Env
		// is optional — we set it for parity with other adapters.
		servers["gortex"] = map[string]any{
			"command": "gortex",
			"args":    []string{"serve", "--index", ".", "--watch"},
			"env":     map[string]string{"GORTEX_INDEX_WORKERS": "8"},
		}
		root["servers"] = servers
		return true, nil
	}, opts)
	if err != nil {
		return res, err
	}
	res.Files = append(res.Files, action)
	res.Configured = true
	return res, nil
}
