// Package codex implements the Gortex init integration for the
// OpenAI Codex CLI. Codex stores MCP server definitions in a TOML
// file — ~/.codex/config.toml for the default scope — under the
// [mcp_servers.<name>] table:
//
//	[mcp_servers.gortex]
//	command = "gortex"
//	args = ["serve", "--index", ".", "--watch"]
//	[mcp_servers.gortex.env]
//	GORTEX_INDEX_WORKERS = "8"
//
// Docs: https://github.com/openai/codex/blob/main/docs/config.md
package codex

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/agents/internalutil"
)

const Name = "codex"
const DocsURL = "https://developers.openai.com/codex/mcp"

type Adapter struct{}

func New() *Adapter                { return &Adapter{} }
func (a *Adapter) Name() string    { return Name }
func (a *Adapter) DocsURL() string { return DocsURL }

// Detect checks for the codex CLI on PATH or ~/.codex/.
func (a *Adapter) Detect(env agents.Env) (bool, error) {
	if p, err := exec.LookPath("codex"); err == nil && p != "" {
		return true, nil
	}
	if env.Home == "" {
		return false, nil
	}
	if _, err := os.Stat(filepath.Join(env.Home, ".codex")); err == nil {
		return true, nil
	}
	return false, nil
}

func (a *Adapter) Plan(env agents.Env) (*agents.Plan, error) {
	if env.Home == "" {
		return &agents.Plan{}, nil
	}
	return &agents.Plan{Files: []agents.FileAction{{
		Path:   filepath.Join(env.Home, ".codex", "config.toml"),
		Action: agents.ActionWouldMerge,
		Keys:   []string{"mcp_servers"},
	}}}, nil
}

func (a *Adapter) Apply(env agents.Env, opts agents.ApplyOpts) (*agents.Result, error) {
	res := &agents.Result{Name: Name, DocsURL: DocsURL}
	detected, _ := a.Detect(env)
	res.Detected = detected
	if !detected {
		internalutil.Logf(env.Stderr, "[gortex init] skip Codex setup (codex not detected)")
		return res, nil
	}
	if env.Home == "" {
		return res, fmt.Errorf("codex: requires a resolved home directory")
	}
	internalutil.Logf(env.Stderr, "[gortex init] setting up OpenAI Codex CLI integration...")

	path := filepath.Join(env.Home, ".codex", "config.toml")
	action, err := agents.MergeTOML(env.Stderr, path, func(root map[string]any, _ bool) (bool, error) {
		servers, ok := root["mcp_servers"].(map[string]any)
		if !ok {
			servers = make(map[string]any)
		}
		if _, exists := servers["gortex"]; exists && !opts.Force {
			return false, nil
		}
		servers["gortex"] = map[string]any{
			"command": "gortex",
			"args":    []string{"serve", "--index", ".", "--watch"},
			"env": map[string]any{
				"GORTEX_INDEX_WORKERS": "8",
			},
		}
		root["mcp_servers"] = servers
		return true, nil
	}, opts)
	if err != nil {
		return res, err
	}
	res.Files = append(res.Files, action)
	res.Configured = true
	return res, nil
}
