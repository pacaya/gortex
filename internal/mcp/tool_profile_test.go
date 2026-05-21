package mcp

import (
	"testing"
)

func TestIsToolEnabled(t *testing.T) {
	srv := newFullTestServer(t)
	// search_symbols is a hot eager tool — always live.
	if !srv.IsToolEnabled("search_symbols") {
		t.Error("search_symbols should be enabled")
	}
	// A deferred tool is still reachable (behind tools_search).
	if !srv.IsToolEnabled("store_memory") {
		t.Error("store_memory should be enabled (deferred is still reachable)")
	}
	if srv.IsToolEnabled("definitely_not_a_tool_xyz") {
		t.Error("an unregistered name must report not enabled")
	}
	if srv.IsToolEnabled("") {
		t.Error("empty tool name must report not enabled")
	}
}

func TestToolProfile_FullProfile(t *testing.T) {
	srv := newFullTestServer(t)
	res := callHandler(t, srv.handleToolProfile, map[string]any{})
	out := unmarshalResult(t, res)

	total, _ := out["total"].(float64)
	if total < 50 {
		t.Errorf("total tools = %v, want a substantial surface (>=50)", total)
	}
	live, ok := out["live"].([]any)
	if !ok || len(live) == 0 {
		t.Fatalf("live list missing or empty: %#v", out["live"])
	}
	liveCount, _ := out["live_count"].(float64)
	if int(liveCount) != len(live) {
		t.Errorf("live_count %v disagrees with live list length %d", liveCount, len(live))
	}
	if _, ok := out["scopes"].(map[string]any); !ok {
		t.Errorf("scopes map missing: %#v", out["scopes"])
	}
}

func TestToolProfile_LazyDisabledMakesEverythingLive(t *testing.T) {
	t.Setenv("GORTEX_LAZY_TOOLS", "0")
	srv := newFullTestServer(t)
	res := callHandler(t, srv.handleToolProfile, map[string]any{})
	out := unmarshalResult(t, res)

	if le, _ := out["lazy_enabled"].(bool); le {
		t.Error("lazy_enabled should be false with GORTEX_LAZY_TOOLS=0")
	}
	if dc, _ := out["deferred_count"].(float64); dc != 0 {
		t.Errorf("deferred_count = %v, want 0 when lazy is disabled", dc)
	}
}

func TestToolProfile_PerTool(t *testing.T) {
	srv := newFullTestServer(t)

	res := callHandler(t, srv.handleToolProfile, map[string]any{"tool": "search_symbols"})
	out := unmarshalResult(t, res)
	if en, _ := out["enabled"].(bool); !en {
		t.Error("search_symbols should report enabled")
	}
	if out["status"] != "live" {
		t.Errorf("search_symbols status = %v, want live", out["status"])
	}

	res = callHandler(t, srv.handleToolProfile, map[string]any{"tool": "no_such_tool_xyz"})
	out = unmarshalResult(t, res)
	if en, _ := out["enabled"].(bool); en {
		t.Error("no_such_tool_xyz should report not enabled")
	}
	if out["status"] != "absent" {
		t.Errorf("unknown tool status = %v, want absent", out["status"])
	}
}
