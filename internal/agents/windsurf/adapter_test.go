package windsurf

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/agents/agentstest"
)

// TestWindsurfUsesCurrentPath verifies we write to
// ~/.codeium/mcp_config.json (the 2026+ canonical path) rather than
// the legacy ~/.codeium/windsurf/mcp_config.json.
func TestWindsurfUsesCurrentPath(t *testing.T) {
	env, _ := agentstest.NewEnv(t)
	// Detection sentinel: ~/.codeium exists.
	if err := os.MkdirAll(filepath.Join(env.Home, ".codeium"), 0o755); err != nil {
		t.Fatal(err)
	}
	a := New()

	res, err := a.Apply(env, agents.ApplyOpts{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	agentstest.AssertCountsByAction(t, res, map[agents.ActionKind]int{agents.ActionCreate: 1})

	if _, err := os.Stat(filepath.Join(env.Home, ".codeium", "mcp_config.json")); err != nil {
		t.Fatalf("current config path not written: %v", err)
	}
	if _, err := os.Stat(filepath.Join(env.Home, ".codeium", "windsurf", "mcp_config.json")); err == nil {
		t.Fatal("legacy path should not be written by default")
	}

	agentstest.AssertIdempotent(t, a, env)
}

// TestWindsurfForceRemovesLegacyFile confirms --force removes the
// stale pre-2026 config file so users don't get confused state.
func TestWindsurfForceRemovesLegacyFile(t *testing.T) {
	env, _ := agentstest.NewEnv(t)
	legacy := filepath.Join(env.Home, ".codeium", "windsurf", "mcp_config.json")
	agentstest.WriteJSON(t, legacy, map[string]any{"mcpServers": map[string]any{}})

	res, err := New().Apply(env, agents.ApplyOpts{Force: true})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !res.Configured {
		t.Fatal("expected Configured=true")
	}
	if _, err := os.Stat(legacy); err == nil {
		t.Fatal("--force should have removed legacy file")
	}
}
