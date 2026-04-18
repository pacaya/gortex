// Package zed implements the Gortex init integration for the Zed
// editor. Zed calls its MCP registry `context_servers` (not
// `mcpServers`) and stores it in a platform-specific settings.json:
//
//	macOS:   ~/Library/Application Support/Zed/settings.json
//	Linux:   ~/.config/zed/settings.json
//	Windows: %APPDATA%\Zed\settings.json
//
// The server entry shape:
//
//	{
//	  "context_servers": {
//	    "gortex": {
//	      "source": "custom",
//	      "command": "gortex",
//	      "args": ["serve", "--index", ".", "--watch"],
//	      "env": {"GORTEX_INDEX_WORKERS": "8"}
//	    }
//	  }
//	}
//
// Docs: https://zed.dev/docs/ai/mcp
package zed

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/agents/internalutil"
)

const Name = "zed"
const DocsURL = "https://zed.dev/docs/ai/mcp"

type Adapter struct{}

func New() *Adapter                { return &Adapter{} }
func (a *Adapter) Name() string    { return Name }
func (a *Adapter) DocsURL() string { return DocsURL }

// userSettingsPath returns the platform-specific Zed settings file.
// Returns "" when Home is unset or the OS is unsupported.
func userSettingsPath(home string) string {
	if home == "" {
		return ""
	}
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "Zed", "settings.json")
	case "linux":
		return filepath.Join(home, ".config", "zed", "settings.json")
	case "windows":
		// %APPDATA% is usually ~\AppData\Roaming on Windows.
		return filepath.Join(home, "AppData", "Roaming", "Zed", "settings.json")
	default:
		return ""
	}
}

// Detect checks for the zed CLI on PATH or the platform-specific
// settings.json directory.
func (a *Adapter) Detect(env agents.Env) (bool, error) {
	if p, err := exec.LookPath("zed"); err == nil && p != "" {
		return true, nil
	}
	path := userSettingsPath(env.Home)
	if path == "" {
		return false, nil
	}
	if _, err := os.Stat(filepath.Dir(path)); err == nil {
		return true, nil
	}
	return false, nil
}

func (a *Adapter) Plan(env agents.Env) (*agents.Plan, error) {
	path := userSettingsPath(env.Home)
	if path == "" {
		return &agents.Plan{}, nil
	}
	return &agents.Plan{Files: []agents.FileAction{{
		Path:   path,
		Action: agents.ActionWouldMerge,
		Keys:   []string{"context_servers"},
	}}}, nil
}

func (a *Adapter) Apply(env agents.Env, opts agents.ApplyOpts) (*agents.Result, error) {
	res := &agents.Result{Name: Name, DocsURL: DocsURL}
	detected, _ := a.Detect(env)
	res.Detected = detected
	if !detected {
		internalutil.Logf(env.Stderr, "[gortex init] skip Zed setup (zed not detected)")
		return res, nil
	}
	path := userSettingsPath(env.Home)
	if path == "" {
		return res, fmt.Errorf("zed: no user settings path known for %s", runtime.GOOS)
	}
	internalutil.Logf(env.Stderr, "[gortex init] setting up Zed integration...")

	action, err := agents.MergeJSON(env.Stderr, path, func(root map[string]any, _ bool) (bool, error) {
		servers, ok := root["context_servers"].(map[string]any)
		if !ok {
			servers = make(map[string]any)
		}
		if _, exists := servers["gortex"]; exists && !opts.Force {
			return false, nil
		}
		servers["gortex"] = map[string]any{
			"source":  "custom",
			"command": "gortex",
			"args":    []string{"serve", "--index", ".", "--watch"},
			"env":     map[string]string{"GORTEX_INDEX_WORKERS": "8"},
		}
		root["context_servers"] = servers
		return true, nil
	}, opts)
	if err != nil {
		return res, err
	}
	res.Files = append(res.Files, action)
	res.Configured = true
	return res, nil
}
