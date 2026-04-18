package gemini

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/agents/agentstest"
)

func TestGeminiGlobalModeWritesUserSettings(t *testing.T) {
	env, _ := agentstest.NewEnv(t)
	env.Mode = agents.ModeGlobal
	if err := os.MkdirAll(filepath.Join(env.Home, ".gemini"), 0o755); err != nil {
		t.Fatal(err)
	}
	if f, err := os.Create(filepath.Join(env.Home, ".gemini", "settings.json")); err != nil {
		t.Fatal(err)
	} else {
		_ = f.Close()
	}

	a := New()
	res, err := a.Apply(env, agents.ApplyOpts{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !res.Configured {
		t.Fatal("expected Configured=true")
	}
	cfg := agentstest.ReadJSON(t, filepath.Join(env.Home, ".gemini", "settings.json"))
	servers, ok := cfg["mcpServers"].(map[string]any)
	if !ok {
		t.Fatalf("missing mcpServers: %v", cfg)
	}
	if _, ok := servers["gortex"]; !ok {
		t.Fatalf("gortex missing: %v", servers)
	}
}
