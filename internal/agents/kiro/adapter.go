package kiro

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/zzet/gortex/internal/agents"
)

const Name = "kiro"
const DocsURL = "https://kiro.dev/docs/mcp"

type Adapter struct{}

func New() *Adapter        { return &Adapter{} }
func (a *Adapter) Name() string    { return Name }
func (a *Adapter) DocsURL() string { return DocsURL }

// Detect reports true when any of: the project already has .kiro/,
// the user has ~/.kiro, or "kiro" is on PATH. A single hit is
// enough — we'd rather over-provision than silently skip a user who
// happens to open the repo in Kiro later.
func (a *Adapter) Detect(env agents.Env) (bool, error) {
	if _, err := os.Stat(filepath.Join(env.Root, ".kiro")); err == nil {
		return true, nil
	}
	if env.Home != "" {
		if _, err := os.Stat(filepath.Join(env.Home, ".kiro")); err == nil {
			return true, nil
		}
	}
	if p, err := exec.LookPath("kiro"); err == nil && p != "" {
		return true, nil
	}
	return false, nil
}

func (a *Adapter) Plan(env agents.Env) (*agents.Plan, error) {
	p := &agents.Plan{}
	p.Files = append(p.Files, agents.FileAction{
		Path:   mcpConfigPath(env),
		Action: agents.ActionWouldMerge,
		Keys:   []string{"mcpServers"},
	})
	// Steering docs and hooks only make sense per-project — Kiro's
	// agent-hook engine only fires in the workspace that owns them.
	if env.Mode == agents.ModeGlobal {
		return p, nil
	}
	for name := range SteeringFiles {
		p.Files = append(p.Files, agents.FileAction{
			Path:   filepath.Join(env.Root, ".kiro", "steering", name),
			Action: agents.ActionWouldCreate,
		})
	}
	for name := range HookFiles {
		p.Files = append(p.Files, agents.FileAction{
			Path:   filepath.Join(env.Root, ".kiro", "hooks", name),
			Action: agents.ActionWouldCreate,
		})
	}
	return p, nil
}

func (a *Adapter) Apply(env agents.Env, opts agents.ApplyOpts) (*agents.Result, error) {
	res := &agents.Result{Name: Name, DocsURL: DocsURL}
	detected, _ := a.Detect(env)
	res.Detected = detected
	if !detected {
		logf(env.Stderr, "[gortex init] skip Kiro setup (Kiro not detected)")
		return res, nil
	}
	logf(env.Stderr, "[gortex init] setting up Kiro IDE integration...")

	// 1. MCP config with Kiro-specific extras (autoApprove list and
	//    an explicit disabled:false flag Kiro's UI expects). Kiro
	//    supports both workspace (.kiro/settings/mcp.json) and user
	//    (~/.kiro/settings/mcp.json) paths — the global mode writes
	//    to the user-level one so every project the user opens
	//    picks up Gortex automatically.
	mcpPath := mcpConfigPath(env)
	action, err := agents.MergeJSON(env.Stderr, mcpPath, func(root map[string]any, _ bool) (bool, error) {
		entry := agents.DefaultGortexMCPEntry()
		entry["disabled"] = false
		entry["autoApprove"] = AutoApproveTools
		return agents.UpsertMCPServer(root, "gortex", entry, opts), nil
	}, opts)
	if err != nil {
		return res, fmt.Errorf("kiro mcp.json: %w", err)
	}
	res.Files = append(res.Files, action)

	if env.Mode == agents.ModeGlobal {
		// Steering docs and agent hooks are project-scoped and
		// irrelevant at the user level. Stop here.
		res.Configured = true
		return res, nil
	}

	// 2. Steering docs — static, created if absent.
	for name, content := range SteeringFiles {
		action, err := agents.WriteIfNotExists(env.Stderr, filepath.Join(env.Root, ".kiro", "steering", name), content, opts)
		if err != nil {
			return res, err
		}
		res.Files = append(res.Files, action)
	}

	// 3. Agent hooks — static JSON, created if absent.
	for name, content := range HookFiles {
		action, err := agents.WriteIfNotExists(env.Stderr, filepath.Join(env.Root, ".kiro", "hooks", name), content, opts)
		if err != nil {
			return res, err
		}
		res.Files = append(res.Files, action)
	}

	res.Configured = true
	return res, nil
}

// mcpConfigPath returns the mcp.json path for the given Env's
// mode. Workspace mode writes .kiro/settings/mcp.json; global mode
// writes ~/.kiro/settings/mcp.json. Kiro merges the two with
// workspace taking precedence.
func mcpConfigPath(env agents.Env) string {
	if env.Mode == agents.ModeGlobal && env.Home != "" {
		return filepath.Join(env.Home, ".kiro", "settings", "mcp.json")
	}
	return filepath.Join(env.Root, ".kiro", "settings", "mcp.json")
}

func logf(w io.Writer, format string, args ...any) {
	if w == nil {
		return
	}
	_, _ = fmt.Fprintf(w, format+"\n", args...)
}
