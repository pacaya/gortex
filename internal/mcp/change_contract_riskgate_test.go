package mcp

import (
	"context"
	"encoding/json"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

func contractEnvelope(t *testing.T, srv *Server, args map[string]any) changeEnvelope {
	t.Helper()
	req := mcplib.CallToolRequest{}
	req.Params.Arguments = args
	res, err := srv.handleChangeContract(context.Background(), req)
	require.NoError(t, err)
	require.False(t, res.IsError, "change_contract errored: %s", toolResultText(res))
	var env changeEnvelope
	require.NoError(t, json.Unmarshal([]byte(res.Content[0].(mcplib.TextContent).Text), &env))
	return env
}

func hasErrorRiskGate(env changeEnvelope) bool {
	for _, r := range env.Reasons {
		if r.Family == "risk_gate" && r.Severity == "error" {
			return true
		}
	}
	return false
}

func TestRiskGateAckLifecycle(t *testing.T) {
	t.Setenv("GORTEX_RISK_GATE_MIN_CALLERS", "1") // Foo has 1 caller (Bar) -> gated
	srv, g := setupAPIServer(t)
	srv.InitMemories("", "") // in-memory ack ledger

	var fooID string
	for _, n := range g.AllNodes() {
		if n.Name == "Foo" && n.Kind == graph.KindFunction {
			fooID = n.ID
		}
	}
	require.NotEmpty(t, fooID)

	// 1. Gated, no ack yet -> refuse.
	env := contractEnvelope(t, srv, map[string]any{"source": "symbols", "symbols": fooID, "risk_gate": true})
	require.True(t, hasErrorRiskGate(env), "ungated symbol should produce a risk_gate error, got %+v", env.Reasons)
	require.Equal(t, verdictRefuse, env.Verdict)

	// 2. Record an ack.
	req := mcplib.CallToolRequest{}
	req.Params.Arguments = map[string]any{"source": "symbols", "symbols": fooID, "ack": true}
	res, err := srv.handleChangeContract(context.Background(), req)
	require.NoError(t, err)
	require.False(t, res.IsError, "ack errored: %s", toolResultText(res))
	var ackResp struct {
		Acked []string `json:"acked"`
	}
	require.NoError(t, json.Unmarshal([]byte(res.Content[0].(mcplib.TextContent).Text), &ackResp))
	require.Contains(t, ackResp.Acked, fooID)

	// 3. Gated run now clears — no risk_gate error.
	env = contractEnvelope(t, srv, map[string]any{"source": "symbols", "symbols": fooID, "risk_gate": true})
	require.False(t, hasErrorRiskGate(env), "a fresh ack should clear the gate, got %+v", env.Reasons)
}

func TestRiskGateOffByDefault(t *testing.T) {
	t.Setenv("GORTEX_RISK_GATE_MIN_CALLERS", "1")
	srv, g := setupAPIServer(t)
	srv.InitMemories("", "")

	var fooID string
	for _, n := range g.AllNodes() {
		if n.Name == "Foo" && n.Kind == graph.KindFunction {
			fooID = n.ID
		}
	}
	// Without risk_gate the gate never engages.
	env := contractEnvelope(t, srv, map[string]any{"source": "symbols", "symbols": fooID})
	require.False(t, hasErrorRiskGate(env), "risk gate must be opt-in")
}
