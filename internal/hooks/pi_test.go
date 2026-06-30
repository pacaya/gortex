package hooks

import (
	"testing"

	"github.com/zzet/gortex/internal/daemon"
)

func TestHandlePi_SessionStartReturnsOrientation(t *testing.T) {
	withFakeStatus(t, func() (*daemon.StatusResponse, error) {
		return &daemon.StatusResponse{
			Version: "1.0.0", Ready: true,
			TrackedRepos: []daemon.TrackedRepoStatus{{Name: "repo", Path: "/tmp/repo", Workspace: "repo", Nodes: 10}},
			Workspaces:   []daemon.WorkspaceSummary{{Slug: "repo"}},
		}, nil
	})
	d := handlePi([]byte(`{"event":"session_start","cwd":"/tmp/repo"}`), 0, ModeDeny)
	if d.Orientation == "" {
		t.Fatal("expected a non-empty orientation on session_start")
	}
	if d.Block {
		t.Error("session_start must never block")
	}
}

func TestHandlePi_ToolCallBlocksGreedyGlob(t *testing.T) {
	// Greedy source glob denies only when the daemon is reachable.
	prev := daemonReachableFn
	daemonReachableFn = func() bool { return true }
	t.Cleanup(func() { daemonReachableFn = prev })

	d := handlePi([]byte(`{"event":"tool_call","tool_name":"Glob","tool_input":{"pattern":"**/*.go"},"cwd":"/tmp/repo"}`), 0, ModeDeny)
	if !d.Block {
		t.Fatalf("expected block on greedy source glob, got %+v", d)
	}
	if d.Reason == "" {
		t.Error("a block must carry a reason")
	}
}

func TestHandlePi_EnrichModeDowngradesBlockToContext(t *testing.T) {
	prev := daemonReachableFn
	daemonReachableFn = func() bool { return true }
	t.Cleanup(func() { daemonReachableFn = prev })

	d := handlePi([]byte(`{"event":"tool_call","tool_name":"Glob","tool_input":{"pattern":"**/*.go"},"cwd":"/tmp/repo"}`), 0, ModeEnrich)
	if d.Block {
		t.Errorf("enrich mode must not block, got block=true")
	}
	if d.AdditionalContext == "" {
		t.Error("enrich mode should surface the guidance as additional_context")
	}
}

func TestHandlePi_GortexToolNeverBlocks(t *testing.T) {
	prev := daemonReachableFn
	daemonReachableFn = func() bool { return true }
	t.Cleanup(func() { daemonReachableFn = prev })

	// Even a name that would otherwise classify as a fallback is allowed
	// when flagged as a Gortex graph tool.
	d := handlePi([]byte(`{"event":"tool_call","tool_name":"Glob","tool_input":{"pattern":"**/*.go"},"is_gortex_tool":true,"cwd":"/tmp/repo"}`), 0, ModeDeny)
	if d.Block {
		t.Errorf("a Gortex graph tool call must never be blocked, got %+v", d)
	}
}

func TestHandlePi_UnknownEventIsNoOp(t *testing.T) {
	d := handlePi([]byte(`{"event":"turn_end"}`), 0, ModeDeny)
	if d.Block || d.Reason != "" || d.AdditionalContext != "" || d.Orientation != "" {
		t.Errorf("unknown event must be a no-op, got %+v", d)
	}
}

func TestHandlePi_GarbledInputIsNoOp(t *testing.T) {
	d := handlePi([]byte(`{not json`), 0, ModeDeny)
	if d.Block || d.Orientation != "" {
		t.Errorf("garbled input must be a no-op, got %+v", d)
	}
}
