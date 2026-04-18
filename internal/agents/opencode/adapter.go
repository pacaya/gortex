// Package opencode implements the Gortex init integration for
// OpenCode. OpenCode uses a different MCP schema than the canonical
// Claude / Cursor shape:
//
//	{
//	  "$schema": "https://opencode.ai/config.json",
//	  "mcp": {
//	    "gortex": {
//	      "type": "local",
//	      "command": ["gortex", "serve", ...],
//	      "environment": {...},
//	      "enabled": true
//	    }
//	  }
//	}
//
// We preserve any existing $schema reference and add one if absent.
package opencode

import (
	"os"
	"os/exec"
	"path/filepath"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/agents/internalutil"
)

const Name = "opencode"
const DocsURL = "https://opencode.ai/docs/mcp"
const SchemaURL = "https://opencode.ai/config.json"

type Adapter struct{}

func New() *Adapter                { return &Adapter{} }
func (a *Adapter) Name() string    { return Name }
func (a *Adapter) DocsURL() string { return DocsURL }

func (a *Adapter) Detect(env agents.Env) (bool, error) {
	if _, err := os.Stat(filepath.Join(env.Root, ".opencode")); err == nil {
		return true, nil
	}
	if p, err := exec.LookPath("opencode"); err == nil && p != "" {
		return true, nil
	}
	if env.Home != "" {
		if _, err := os.Stat(filepath.Join(env.Home, ".config", "opencode")); err == nil {
			return true, nil
		}
	}
	return false, nil
}

func (a *Adapter) Plan(env agents.Env) (*agents.Plan, error) {
	return &agents.Plan{Files: []agents.FileAction{{
		Path:   filepath.Join(env.Root, ".opencode", "config.json"),
		Action: agents.ActionWouldMerge,
		Keys:   []string{"mcp"},
	}}}, nil
}

func (a *Adapter) Apply(env agents.Env, opts agents.ApplyOpts) (*agents.Result, error) {
	res := &agents.Result{Name: Name, DocsURL: DocsURL}
	if env.Mode == agents.ModeGlobal {
		return res, nil
	}
	detected, _ := a.Detect(env)
	res.Detected = detected
	if !detected {
		internalutil.Logf(env.Stderr, "[gortex init] skip OpenCode setup (OpenCode not detected)")
		return res, nil
	}
	internalutil.Logf(env.Stderr, "[gortex init] setting up OpenCode integration...")

	path := filepath.Join(env.Root, ".opencode", "config.json")
	action, err := agents.MergeJSON(env.Stderr, path, func(root map[string]any, _ bool) (bool, error) {
		mcpSection, ok := root["mcp"].(map[string]any)
		if !ok {
			mcpSection = make(map[string]any)
		}
		if _, exists := mcpSection["gortex"]; exists && !opts.Force {
			return false, nil
		}
		mcpSection["gortex"] = map[string]any{
			"type":    "local",
			"command": []string{"gortex", "serve", "--index", ".", "--watch"},
			"environment": map[string]string{
				"GORTEX_INDEX_WORKERS": "8",
			},
			"enabled": true,
		}
		root["mcp"] = mcpSection
		if _, hasSchema := root["$schema"]; !hasSchema {
			root["$schema"] = SchemaURL
		}
		return true, nil
	}, opts)
	if err != nil {
		return res, err
	}
	res.Files = append(res.Files, action)
	res.Configured = true
	return res, nil
}
