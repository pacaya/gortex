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

// TestCoreDefaultDefer_E2EOverMCPDispatch exercises the SHIPPED DEFAULT
// surface — the `core` preset in `defer` mode, with no GORTEX_LAZY_TOOLS
// override — through the real MCPServer.HandleMessage path: the cold
// tools/list carries the curated core, a specialised tool is absent
// until tools_search fetches its schema, and the promotion lands it in
// a subsequent tools/list (callable thereafter).
func TestCoreDefaultDefer_E2EOverMCPDispatch(t *testing.T) {
	srv := setupPresetServer(t, ToolPolicyConfig{Preset: "core", Mode: "defer"})
	ctx := context.Background()

	listFrame := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)
	parseNames := func(reply any) map[string]bool {
		out, _ := json.Marshal(reply)
		var parsed struct {
			Result struct {
				Tools []struct {
					Name string `json:"name"`
				} `json:"tools"`
			} `json:"result"`
		}
		require.NoError(t, json.Unmarshal(out, &parsed))
		names := make(map[string]bool, len(parsed.Result.Tools))
		for _, e := range parsed.Result.Tools {
			names[e.Name] = true
		}
		return names
	}

	// 1. Cold tools/list — curated core + tools_search visible; a
	//    specialised analysis tool is deferred (absent).
	names := parseNames(srv.MCPServer().HandleMessage(ctx, listFrame))
	require.True(t, names["smart_context"], "core tool smart_context must be eagerly visible")
	require.True(t, names["edit_file"], "core tool edit_file must be eagerly visible")
	require.True(t, names[LazyToolsSearchName], "tools_search must be eagerly visible")
	require.False(t, names["get_architecture"], "deferred get_architecture must not appear before discovery")

	// 2. tools_search promotes get_architecture and returns its schema.
	callFrame, _ := json.Marshal(struct {
		JSONRPC string `json:"jsonrpc"`
		ID      int    `json:"id"`
		Method  string `json:"method"`
		Params  any    `json:"params"`
	}{
		JSONRPC: "2.0", ID: 2, Method: "tools/call",
		Params: mcplib.CallToolParams{
			Name:      LazyToolsSearchName,
			Arguments: map[string]any{"query": "select:get_architecture"},
		},
	})
	out2, _ := json.Marshal(srv.MCPServer().HandleMessage(ctx, callFrame))
	require.True(t, strings.Contains(string(out2), "get_architecture"),
		"tools_search response must carry get_architecture schema:\n%s", string(out2))

	// 3. tools/list again — the promoted tool is now live.
	names2 := parseNames(srv.MCPServer().HandleMessage(ctx, listFrame))
	require.True(t, names2["get_architecture"],
		"promoted get_architecture must be in tools/list after discovery")
}
