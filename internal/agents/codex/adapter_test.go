package codex

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/agents/agentstest"
)

// TestCodexWritesMcpServersTOMLTable verifies we produce the
// documented [mcp_servers.gortex] table — not a legacy
// [mcp.gortex] or [mcpServers.gortex].
func TestCodexWritesMcpServersTOMLTable(t *testing.T) {
	env, _ := agentstest.NewEnv(t)
	// Detection sentinel: ~/.codex/ exists.
	if err := os.MkdirAll(filepath.Join(env.Home, ".codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	a := New()

	res, err := a.Apply(env, agents.ApplyOpts{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	agentstest.AssertCountsByAction(t, res, map[agents.ActionKind]int{agents.ActionCreate: 1})

	data, err := os.ReadFile(filepath.Join(env.Home, ".codex", "config.toml"))
	if err != nil {
		t.Fatalf("read config.toml: %v", err)
	}
	got := string(data)
	if !strings.Contains(got, "mcp_servers") {
		t.Fatalf("expected mcp_servers table: %s", got)
	}
	if !strings.Contains(got, "gortex") {
		t.Fatalf("expected gortex entry: %s", got)
	}

	agentstest.AssertIdempotent(t, a, env)
}
