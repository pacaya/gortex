package cursor

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/agents/agentstest"
)

// TestCursorCreatesMergesAndSkips covers the three behavioural
// phases every adapter must honour:
//   - initial create on an empty project (via .cursor/ sentinel)
//   - re-run is a no-op (idempotent)
//   - merge into an existing mcp.json preserves user keys
func TestCursorCreatesMergesAndSkips(t *testing.T) {
	env, _ := agentstest.NewEnv(t)

	// Sentinel: create .cursor/ so Detect reports true. Without
	// this the adapter would skip with "not detected".
	if err := os.MkdirAll(filepath.Join(env.Root, ".cursor"), 0o755); err != nil {
		t.Fatal(err)
	}

	a := New()

	// Phase 1 — create.
	res, err := a.Apply(env, agents.ApplyOpts{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !res.Detected {
		t.Fatal("expected Detected=true after creating .cursor/")
	}
	if !res.Configured {
		t.Fatal("expected Configured=true after first apply")
	}
	agentstest.AssertCountsByAction(t, res, map[agents.ActionKind]int{agents.ActionCreate: 1})
	mcp := agentstest.ReadJSON(t, filepath.Join(env.Root, ".cursor", "mcp.json"))
	servers := mcp["mcpServers"].(map[string]any)
	if _, ok := servers["gortex"]; !ok {
		t.Fatalf("gortex server missing: %v", servers)
	}

	// Phase 2 — idempotent re-run.
	agentstest.AssertIdempotent(t, a, env)
}

// TestCursorMergeIntoExistingPreservesUserKeys confirms that the
// adapter writes alongside — not over — a user's pre-existing MCP
// server entry. This is the load-bearing property of MergeJSON.
func TestCursorMergeIntoExistingPreservesUserKeys(t *testing.T) {
	env, _ := agentstest.NewEnv(t)
	if err := os.MkdirAll(filepath.Join(env.Root, ".cursor"), 0o755); err != nil {
		t.Fatal(err)
	}
	mcpPath := filepath.Join(env.Root, ".cursor", "mcp.json")
	agentstest.WriteJSON(t, mcpPath, map[string]any{
		"mcpServers": map[string]any{
			"user-server": map[string]any{"command": "user-tool"},
		},
	})

	res, err := New().Apply(env, agents.ApplyOpts{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	agentstest.AssertCountsByAction(t, res, map[agents.ActionKind]int{agents.ActionMerge: 1})
	after := agentstest.ReadJSON(t, mcpPath)
	servers := after["mcpServers"].(map[string]any)
	if _, ok := servers["user-server"]; !ok {
		t.Fatalf("user-server was clobbered: %v", servers)
	}
	if _, ok := servers["gortex"]; !ok {
		t.Fatalf("gortex missing after merge: %v", servers)
	}
}
