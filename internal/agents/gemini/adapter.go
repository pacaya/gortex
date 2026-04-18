// Package gemini implements the Gortex init integration for the
// Gemini CLI. Gemini reads ~/.gemini/settings.json (user-level) and
// .gemini/settings.json (project-level); both accept the standard
// {"mcpServers": {...}} shape.
//
// Note: this adapter is distinct from the antigravity adapter —
// Gemini CLI and Google Antigravity are different products that
// happen to share the ~/.gemini/ root directory.
//
// Docs: https://geminicli.com/docs/tools/mcp-server/
package gemini

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/agents/internalutil"
)

const Name = "gemini"
const DocsURL = "https://geminicli.com/docs/tools/mcp-server/"

type Adapter struct{}

func New() *Adapter                { return &Adapter{} }
func (a *Adapter) Name() string    { return Name }
func (a *Adapter) DocsURL() string { return DocsURL }

// Detect checks for the gemini CLI on PATH or an existing user-level
// settings.json. We avoid colliding with the antigravity adapter's
// detection by looking at ~/.gemini/settings.json specifically
// rather than the whole ~/.gemini/ tree.
func (a *Adapter) Detect(env agents.Env) (bool, error) {
	if p, err := exec.LookPath("gemini"); err == nil && p != "" {
		return true, nil
	}
	if env.Home == "" {
		return false, nil
	}
	if _, err := os.Stat(filepath.Join(env.Home, ".gemini", "settings.json")); err == nil {
		return true, nil
	}
	return false, nil
}

// configPath returns the right settings.json for the current Env.
// Project mode writes .gemini/settings.json; global mode writes
// ~/.gemini/settings.json.
func configPath(env agents.Env) string {
	if env.Mode == agents.ModeGlobal && env.Home != "" {
		return filepath.Join(env.Home, ".gemini", "settings.json")
	}
	return filepath.Join(env.Root, ".gemini", "settings.json")
}

func (a *Adapter) Plan(env agents.Env) (*agents.Plan, error) {
	return &agents.Plan{Files: []agents.FileAction{{
		Path:   configPath(env),
		Action: agents.ActionWouldMerge,
		Keys:   []string{"mcpServers"},
	}}}, nil
}

func (a *Adapter) Apply(env agents.Env, opts agents.ApplyOpts) (*agents.Result, error) {
	res := &agents.Result{Name: Name, DocsURL: DocsURL}
	detected, _ := a.Detect(env)
	res.Detected = detected
	if !detected {
		internalutil.Logf(env.Stderr, "[gortex init] skip Gemini CLI setup (gemini not detected)")
		return res, nil
	}
	if env.Home == "" {
		return res, fmt.Errorf("gemini: requires a resolved home directory")
	}
	internalutil.Logf(env.Stderr, "[gortex init] setting up Gemini CLI integration...")

	action, err := agents.MergeJSON(env.Stderr, configPath(env), func(root map[string]any, _ bool) (bool, error) {
		return agents.UpsertMCPServer(root, "gortex", agents.DefaultGortexMCPEntry(), opts), nil
	}, opts)
	if err != nil {
		return res, err
	}
	res.Files = append(res.Files, action)
	res.Configured = true
	return res, nil
}
