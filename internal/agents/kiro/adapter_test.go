package kiro

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/agents/agentstest"
)

func TestKiroCreatesAllArtifactsAndIsIdempotent(t *testing.T) {
	env, _ := agentstest.NewEnv(t)
	// Sentinel: create .kiro/ so Detect returns true.
	if err := os.MkdirAll(filepath.Join(env.Root, ".kiro"), 0o755); err != nil {
		t.Fatal(err)
	}
	a := New()

	res, err := a.Apply(env, agents.ApplyOpts{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	// 1 mcp.json + len(SteeringFiles) + len(HookFiles).
	want := 1 + len(SteeringFiles) + len(HookFiles)
	got := 0
	for _, f := range res.Files {
		if f.Action == agents.ActionCreate {
			got++
		}
	}
	if got != want {
		t.Fatalf("expected %d creates, got %d (%v)", want, got, res.Files)
	}

	// autoApprove list is baked into the mcp.json entry — verify.
	mcp := agentstest.ReadJSON(t, filepath.Join(env.Root, ".kiro", "settings", "mcp.json"))
	servers := mcp["mcpServers"].(map[string]any)
	gortex := servers["gortex"].(map[string]any)
	approvals, ok := gortex["autoApprove"].([]any)
	if !ok || len(approvals) == 0 {
		t.Fatalf("autoApprove missing or empty: %v", gortex)
	}
	if gortex["disabled"] != false {
		t.Fatalf("disabled should be false: %v", gortex)
	}

	agentstest.AssertIdempotent(t, a, env)
}
