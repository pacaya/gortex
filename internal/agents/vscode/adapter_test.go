package vscode

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/agents/agentstest"
)

// TestVSCodeUsesServersKeyNotMcpServers locks in the VS Code
// 1.102+ schema: top-level key is "servers", not the legacy
// "mcpServers". A regression here would be silently invisible to
// users — VS Code would just not load the server.
func TestVSCodeUsesServersKeyNotMcpServers(t *testing.T) {
	env, _ := agentstest.NewEnv(t)
	if err := os.MkdirAll(filepath.Join(env.Root, ".vscode"), 0o755); err != nil {
		t.Fatal(err)
	}
	a := New()

	res, err := a.Apply(env, agents.ApplyOpts{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	agentstest.AssertCountsByAction(t, res, map[agents.ActionKind]int{agents.ActionCreate: 1})

	cfg := agentstest.ReadJSON(t, filepath.Join(env.Root, ".vscode", "mcp.json"))
	if _, ok := cfg["mcpServers"]; ok {
		t.Fatalf("wrote legacy 'mcpServers' key; VS Code 1.102+ expects 'servers': %v", cfg)
	}
	servers, ok := cfg["servers"].(map[string]any)
	if !ok {
		t.Fatalf("missing 'servers' key: %v", cfg)
	}
	if _, ok := servers["gortex"]; !ok {
		t.Fatalf("servers.gortex missing: %v", servers)
	}

	agentstest.AssertIdempotent(t, a, env)
}

// TestVSCodeMergePreservesUserServers confirms we don't clobber a
// user's existing "servers" entry during the merge.
func TestVSCodeMergePreservesUserServers(t *testing.T) {
	env, _ := agentstest.NewEnv(t)
	if err := os.MkdirAll(filepath.Join(env.Root, ".vscode"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(env.Root, ".vscode", "mcp.json")
	agentstest.WriteJSON(t, path, map[string]any{
		"servers": map[string]any{
			"playwright": map[string]any{"command": "npx"},
		},
	})

	res, err := New().Apply(env, agents.ApplyOpts{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	agentstest.AssertCountsByAction(t, res, map[agents.ActionKind]int{agents.ActionMerge: 1})

	cfg := agentstest.ReadJSON(t, path)
	servers := cfg["servers"].(map[string]any)
	if _, ok := servers["playwright"]; !ok {
		t.Fatalf("playwright was clobbered: %v", servers)
	}
	if _, ok := servers["gortex"]; !ok {
		t.Fatalf("gortex missing: %v", servers)
	}
}
