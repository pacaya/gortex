package antigravity

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/agents/agentstest"
)

// TestAntigravityRegistersMCPAndWritesKI is the acceptance test for
// the 2026 audit fix: we must now write *both* the native MCP
// config (new) and the Knowledge Item (existing). A regression to
// KI-only would silently remove the runtime tool access.
func TestAntigravityRegistersMCPAndWritesKI(t *testing.T) {
	env, _ := agentstest.NewEnv(t)
	a := New()

	res, err := a.Apply(env, agents.ApplyOpts{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !res.Configured {
		t.Fatal("expected Configured=true")
	}

	// 1. Native MCP stanza under ~/.gemini/antigravity/mcp_config.json.
	mcpPath := filepath.Join(env.Home, ".gemini", "antigravity", "mcp_config.json")
	if _, err := os.Stat(mcpPath); err != nil {
		t.Fatalf("mcp_config.json missing: %v", err)
	}
	cfg := agentstest.ReadJSON(t, mcpPath)
	servers, ok := cfg["mcpServers"].(map[string]any)
	if !ok {
		t.Fatalf("mcpServers missing: %v", cfg)
	}
	if _, ok := servers["gortex"]; !ok {
		t.Fatalf("gortex server missing from mcpServers: %v", servers)
	}

	// 2. Knowledge Item artifacts.
	kiBase := filepath.Join(env.Home, ".gemini", "antigravity", "knowledge", "gortex-workflow")
	for _, p := range []string{
		filepath.Join(kiBase, "metadata.json"),
		filepath.Join(kiBase, "artifacts", "gortex-instructions.md"),
	} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("KI artifact missing: %s (%v)", p, err)
		}
	}

	agentstest.AssertIdempotent(t, a, env)
}
