// Package continuedev implements the Gortex init integration for
// Continue.dev. Today we write .continue/mcpServers/gortex.json
// (the legacy split-file layout); the Step 3 audit revisits whether
// Continue's current canonical form is an inline block inside
// config.yaml.
package continuedev

import (
	"os"
	"path/filepath"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/agents/internalutil"
)

const Name = "continue"
const DocsURL = "https://docs.continue.dev/customize/deep-dives/mcp"

type Adapter struct{}

func New() *Adapter                { return &Adapter{} }
func (a *Adapter) Name() string    { return Name }
func (a *Adapter) DocsURL() string { return DocsURL }

func (a *Adapter) Detect(env agents.Env) (bool, error) {
	if _, err := os.Stat(filepath.Join(env.Root, ".continue")); err == nil {
		return true, nil
	}
	if env.Home != "" {
		if _, err := os.Stat(filepath.Join(env.Home, ".continue")); err == nil {
			return true, nil
		}
	}
	return false, nil
}

func (a *Adapter) Plan(env agents.Env) (*agents.Plan, error) {
	return &agents.Plan{Files: []agents.FileAction{{
		Path:   filepath.Join(env.Root, ".continue", "mcpServers", "gortex.json"),
		Action: agents.ActionWouldMerge,
		Keys:   []string{"mcpServers"},
	}}}, nil
}

func (a *Adapter) Apply(env agents.Env, opts agents.ApplyOpts) (*agents.Result, error) {
	res := &agents.Result{Name: Name, DocsURL: DocsURL}
	// Global mode: Continue.dev's MCP config is project-scoped only;
	// nothing to install user-wide today.
	if env.Mode == agents.ModeGlobal {
		return res, nil
	}
	detected, _ := a.Detect(env)
	res.Detected = detected
	if !detected {
		internalutil.Logf(env.Stderr, "[gortex init] skip Continue.dev setup (Continue not detected)")
		return res, nil
	}
	internalutil.Logf(env.Stderr, "[gortex init] setting up Continue.dev integration...")

	path := filepath.Join(env.Root, ".continue", "mcpServers", "gortex.json")
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
