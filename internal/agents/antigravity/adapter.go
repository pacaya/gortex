package antigravity

import (
	"fmt"
	"path/filepath"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/agents/internalutil"
)

// Name identifies the Antigravity adapter for --agents=<name>.
const Name = "antigravity"

// DocsURL is the current public docs entry point for MCP in
// Antigravity. As of 2026 Antigravity supports both MCP server
// registration (at ~/.gemini/antigravity/mcp_config.json) and the
// older Knowledge Item mechanism. We write both today: MCP for
// runtime tool access, KI for the in-editor instructions panel.
const DocsURL = "https://antigravity.google/docs/mcp"

type Adapter struct{}

func New() *Adapter                { return &Adapter{} }
func (a *Adapter) Name() string    { return Name }
func (a *Adapter) DocsURL() string { return DocsURL }

// Detect returns true whenever a Home directory is resolvable.
// Both artifacts are cheap to install and harmless on machines
// without Antigravity installed — a user who installs later picks
// up the config automatically. The Step 4 audit will tighten this
// to a proper install check once Antigravity ships a PATH-visible
// CLI.
func (a *Adapter) Detect(env agents.Env) (bool, error) {
	return env.Home != "", nil
}

func (a *Adapter) Plan(env agents.Env) (*agents.Plan, error) {
	if env.Home == "" {
		return &agents.Plan{}, nil
	}
	kiDir := filepath.Join(env.Home, ".gemini", "antigravity", "knowledge", "gortex-workflow")
	return &agents.Plan{Files: []agents.FileAction{
		{Path: filepath.Join(env.Home, ".gemini", "antigravity", "mcp_config.json"), Action: agents.ActionWouldMerge, Keys: []string{"mcpServers"}},
		{Path: filepath.Join(kiDir, "metadata.json"), Action: agents.ActionWouldCreate},
		{Path: filepath.Join(kiDir, "artifacts", "gortex-instructions.md"), Action: agents.ActionWouldCreate},
	}}, nil
}

func (a *Adapter) Apply(env agents.Env, opts agents.ApplyOpts) (*agents.Result, error) {
	res := &agents.Result{Name: Name, DocsURL: DocsURL}
	detected, _ := a.Detect(env)
	res.Detected = detected
	if !detected {
		return res, nil
	}
	if env.Home == "" {
		return res, fmt.Errorf("antigravity: requires a resolved home directory")
	}
	internalutil.Logf(env.Stderr, "[gortex init] setting up Antigravity integration...")

	// 1. Native MCP registration — new in 2026.
	mcpPath := filepath.Join(env.Home, ".gemini", "antigravity", "mcp_config.json")
	mcpAction, err := agents.MergeJSON(env.Stderr, mcpPath, func(root map[string]any, _ bool) (bool, error) {
		return agents.UpsertMCPServer(root, "gortex", agents.DefaultGortexMCPEntry(), opts), nil
	}, opts)
	if err != nil {
		return res, err
	}
	res.Files = append(res.Files, mcpAction)

	// 2. Knowledge Item — kept as a secondary artifact. Teaches
	//    Antigravity *how to use* Gortex via run_command, which is
	//    useful even when the MCP server is registered (the KI
	//    gives the model intent / workflow guidance that a plain
	//    tool registration doesn't).
	kiDir := filepath.Join(env.Home, ".gemini", "antigravity", "knowledge", "gortex-workflow")

	metaAction, err := agents.WriteIfNotExists(env.Stderr, filepath.Join(kiDir, "metadata.json"), Metadata, opts)
	if err != nil {
		return res, err
	}
	res.Files = append(res.Files, metaAction)

	instrAction, err := agents.WriteIfNotExists(env.Stderr, filepath.Join(kiDir, "artifacts", "gortex-instructions.md"), Instructions, opts)
	if err != nil {
		return res, err
	}
	res.Files = append(res.Files, instrAction)

	res.Configured = true
	return res, nil
}
