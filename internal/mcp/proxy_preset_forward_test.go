package mcp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// listToolNamesForSession drives a real tools/list through
// MCPServer.HandleMessage for the given session ID — the same path the
// daemon's per-session dispatcher uses — and returns the visible set.
func listToolNamesForSession(t *testing.T, srv *Server, sessionID string) map[string]bool {
	t.Helper()
	ctx := WithSessionID(context.Background(), sessionID)
	listFrame := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)
	reply := srv.MCPServer().HandleMessage(ctx, listFrame)
	require.NotNil(t, reply)
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

// TestSessionPresetForward_ProxyPath is the W1 regression: a tool preset
// forwarded by a `gortex mcp` proxy (GORTEX_TOOLS / --tools relayed through
// the daemon handshake and recorded via NoteSessionToolPolicy) is honoured
// authoritatively at tools/list — the daemon serves the shared graph, but
// each session sees exactly the surface it asked for, both narrower AND
// wider than the server's global `core`/defer default. Before this, a
// client preset that widened the surface was silently a no-op because the
// proxy's byte-pump filter can only subtract from the daemon's list.
func TestSessionPresetForward_ProxyPath(t *testing.T) {
	srv := setupPresetServer(t, ToolPolicyConfig{Preset: "core", Mode: "defer"})

	// Baseline: a session that forwards nothing gets the server's global
	// core default — a specialised tool stays deferred (absent).
	base := listToolNamesForSession(t, srv, "sess_default")
	require.True(t, base["smart_context"], "core tool must be visible by default")
	require.False(t, base["find_overrides"], "non-core find_overrides is deferred by default")
	require.False(t, base["get_architecture"], "non-core get_architecture is deferred by default")

	// A session forwards `nav`: the surface NARROWS to read-only navigation
	// (edit tools drop) AND WIDENS to include nav-preset tools the daemon
	// held deferred under core (find_overrides). This is the case the proxy
	// filter alone could never satisfy.
	srv.NoteSessionToolPolicy("sess_nav", "nav", "")
	nav := listToolNamesForSession(t, srv, "sess_nav")
	require.True(t, nav["search_symbols"], "nav keeps navigation tools")
	require.True(t, nav["find_overrides"],
		"forwarded nav must WIDEN the surface to a tool the daemon deferred under core")
	require.False(t, nav["edit_file"], "forwarded nav must NARROW out editing tools")

	// A session forwards `full`: the whole catalogue is materialised for it,
	// including tools deferred under the server's core default.
	srv.NoteSessionToolPolicy("sess_full", "full", "")
	full := listToolNamesForSession(t, srv, "sess_full")
	require.True(t, full["get_architecture"],
		"forwarded full must widen to the deferred catalogue")
	require.True(t, full["edit_file"], "full includes editing tools")
	require.Greater(t, len(full), len(base),
		"full surface must be strictly larger than the core default")

	// Sessions are isolated: the default session's surface is unchanged by
	// another session forwarding a wider preset.
	baseAfter := listToolNamesForSession(t, srv, "sess_default")
	require.False(t, baseAfter["get_architecture"],
		"one session forwarding full must not leak into another session's surface")
}

// TestSessionPresetForward_ExplicitList checks an explicit allow-list spec
// (no preset — exactly these tools) forwarded by a client scopes the
// session down to precisely that set plus the always-kept discovery tools.
func TestSessionPresetForward_ExplicitList(t *testing.T) {
	srv := setupPresetServer(t, ToolPolicyConfig{Preset: "core", Mode: "defer"})
	srv.NoteSessionToolPolicy("sess_x", "search_symbols,edit_file", "")
	names := listToolNamesForSession(t, srv, "sess_x")
	require.True(t, names["search_symbols"])
	require.True(t, names["edit_file"])
	require.True(t, names[LazyToolsSearchName], "discovery tool is always kept")
	require.False(t, names["smart_context"], "a tool outside the explicit list is hidden")
	require.False(t, names["analyze"], "a tool outside the explicit list is hidden")
}
