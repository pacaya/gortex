package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
)

// TestLazyRegistration_E2EOverMCPDispatch exercises the
// discovery → promotion → tools/list flow through the real
// MCPServer.HandleMessage entry point — the same path the
// daemon's per-session dispatcher uses. Without this, a unit
// failure in handleToolsSearch could still pass while the
// JSON-RPC wiring quietly regresses.
func TestLazyRegistration_E2EOverMCPDispatch(t *testing.T) {
	t.Setenv("GORTEX_LAZY_TOOLS", "1")
	srv, _ := setupTestServer(t)
	ctx := context.Background()

	// 1. tools/list — only the hot set + tools_search visible.
	listFrame := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)
	reply := srv.MCPServer().HandleMessage(ctx, listFrame)
	require.NotNil(t, reply)
	out, _ := json.Marshal(reply)
	var listParsed struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	require.NoError(t, json.Unmarshal(out, &listParsed))
	require.LessOrEqual(t, len(listParsed.Result.Tools), 40,
		"cold tools/list should be tight: got %d", len(listParsed.Result.Tools))

	var sawSearch, sawStoreMemory bool
	for _, e := range listParsed.Result.Tools {
		if e.Name == LazyToolsSearchName {
			sawSearch = true
		}
		if e.Name == "store_memory" {
			sawStoreMemory = true
		}
	}
	require.True(t, sawSearch, "tools_search must be eagerly visible")
	require.False(t, sawStoreMemory, "deferred store_memory must NOT appear before discovery")

	// 2. tools/call tools_search → fetch + promote memories tools.
	callFrame, _ := json.Marshal(struct {
		JSONRPC string `json:"jsonrpc"`
		ID      int    `json:"id"`
		Method  string `json:"method"`
		Params  any    `json:"params"`
	}{
		JSONRPC: "2.0", ID: 2, Method: "tools/call",
		Params: mcplib.CallToolParams{
			Name:      LazyToolsSearchName,
			Arguments: map[string]any{"query": "select:store_memory"},
		},
	})
	reply2 := srv.MCPServer().HandleMessage(ctx, callFrame)
	require.NotNil(t, reply2)
	out2, _ := json.Marshal(reply2)
	require.True(t, strings.Contains(string(out2), "store_memory"),
		"tools_search response must carry store_memory schema:\n%s", string(out2))

	// 3. tools/list again — promoted tool now visible.
	reply3 := srv.MCPServer().HandleMessage(ctx, listFrame)
	out3, _ := json.Marshal(reply3)
	var listParsed2 struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	require.NoError(t, json.Unmarshal(out3, &listParsed2))
	var promoted bool
	for _, e := range listParsed2.Result.Tools {
		if e.Name == "store_memory" {
			promoted = true
			break
		}
	}
	require.True(t, promoted, "promoted store_memory must be in tools/list after discovery")
}
