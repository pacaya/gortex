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

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/agents/internalutil"
)

const Name = "cursor"
const DocsURL = "https://docs.cursor.com/en/context/mcp"

type Adapter struct{}

func New() *Adapter                { return &Adapter{} }
func (a *Adapter) Name() string    { return Name }
func (a *Adapter) DocsURL() string { return DocsURL }

// Detect succeeds when any of: project has .cursor/, user has
// ~/.cursor/, or "cursor" is on PATH.
func (a *Adapter) Detect(env agents.Env) (bool, error) {
	if _, err := os.Stat(filepath.Join(env.Root, ".cursor")); err == nil {
		return true, nil
	}
	if env.Home != "" {
		if _, err := os.Stat(filepath.Join(env.Home, ".cursor")); err == nil {
			return true, nil
		}
	}
	if p, err := exec.LookPath("cursor"); err == nil && p != "" {
		return true, nil
	}
	return false, nil
}

func (a *Adapter) Plan(env agents.Env) (*agents.Plan, error) {
	return &agents.Plan{Files: []agents.FileAction{{
		Path:   mcpConfigPath(env),
		Action: agents.ActionWouldMerge,
		Keys:   []string{"mcpServers"},
	}}}, nil
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

func (a *Adapter) Apply(env agents.Env, opts agents.ApplyOpts) (*agents.Result, error) {
	res := &agents.Result{Name: Name, DocsURL: DocsURL}
	detected, _ := a.Detect(env)
	res.Detected = detected
	if !detected {
		internalutil.Logf(env.Stderr, "[gortex init] skip Cursor setup (Cursor not detected)")
		return res, nil
	}
	internalutil.Logf(env.Stderr, "[gortex init] setting up Cursor IDE integration...")

	path := mcpConfigPath(env)
	action, err := agents.MergeJSON(env.Stderr, path, func(root map[string]any, _ bool) (bool, error) {
		return agents.UpsertMCPServer(root, "gortex", agents.DefaultGortexMCPEntry(), opts), nil
	}, opts)
	if err != nil {
		return res, err
	}
	res.Files = append(res.Files, action)
	res.Configured = true
	return res, nil
}
