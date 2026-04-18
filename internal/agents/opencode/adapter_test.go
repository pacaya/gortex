package opencode

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/agents/agentstest"
)

// TestOpenCodeUsesMCPSectionKey verifies we write under "mcp",
// not "mcpServers" — OpenCode's schema differs from the canonical
// Claude / Cursor shape.
func TestOpenCodeUsesMCPSectionKey(t *testing.T) {
	env, _ := agentstest.NewEnv(t)
	if err := os.MkdirAll(filepath.Join(env.Root, ".opencode"), 0o755); err != nil {
		t.Fatal(err)
	}
	a := New()

	res, err := a.Apply(env, agents.ApplyOpts{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	agentstest.AssertCountsByAction(t, res, map[agents.ActionKind]int{agents.ActionCreate: 1})

	cfg := agentstest.ReadJSON(t, filepath.Join(env.Root, ".opencode", "config.json"))
	if _, ok := cfg["mcpServers"]; ok {
		t.Fatalf("should not write mcpServers (that's Claude/Cursor shape): %v", cfg)
	}
	mcp, ok := cfg["mcp"].(map[string]any)
	if !ok {
		t.Fatalf("missing 'mcp' section: %v", cfg)
	}
	gortex, ok := mcp["gortex"].(map[string]any)
	if !ok {
		t.Fatalf("missing 'mcp.gortex': %v", mcp)
	}
	// command must be an array (OpenCode-specific)
	if _, ok := gortex["command"].([]any); !ok {
		t.Fatalf("command should be an array: %v", gortex)
	}
	if cfg["$schema"] != SchemaURL {
		t.Fatalf("expected $schema=%q, got %v", SchemaURL, cfg["$schema"])
	}

	agentstest.AssertIdempotent(t, a, env)
}
