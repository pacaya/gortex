package zed

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/agents/agentstest"
)

// TestZedUsesContextServersKey verifies Zed's idiosyncratic schema:
// top-level key is "context_servers" (not mcpServers or servers).
// A regression here would silently break the integration.
func TestZedUsesContextServersKey(t *testing.T) {
	env, _ := agentstest.NewEnv(t)
	// Detection: create the per-OS settings dir so Detect() succeeds.
	p := userSettingsPath(env.Home)
	if p == "" {
		t.Skip("unsupported OS for zed test")
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}

	a := New()
	res, err := a.Apply(env, agents.ApplyOpts{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	agentstest.AssertCountsByAction(t, res, map[agents.ActionKind]int{agents.ActionCreate: 1})

	cfg := agentstest.ReadJSON(t, p)
	if _, ok := cfg["mcpServers"]; ok {
		t.Fatalf("should not write mcpServers — Zed uses context_servers: %v", cfg)
	}
	servers, ok := cfg["context_servers"].(map[string]any)
	if !ok {
		t.Fatalf("missing context_servers key: %v", cfg)
	}
	gortex, ok := servers["gortex"].(map[string]any)
	if !ok {
		t.Fatalf("gortex entry missing: %v", servers)
	}
	if gortex["source"] != "custom" {
		t.Fatalf("expected source=custom, got %v", gortex["source"])
	}
}
