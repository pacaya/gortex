package main

import (
	"encoding/json"
	"errors"
	"testing"
)

// TestBuildToolCallFrame_PinsJSON asserts every relayed tools/call forces
// format:"json" so the per-tool decode is stable regardless of the
// daemon's client-based wire-format auto-selection.
func TestBuildToolCallFrame_PinsJSON(t *testing.T) {
	frame, err := buildToolCallFrame(1, "search_symbols", map[string]any{"query": "Foo"})
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Method string `json:"method"`
		Params struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		} `json:"params"`
	}
	if err := json.Unmarshal(frame, &m); err != nil {
		t.Fatal(err)
	}
	if m.Method != "tools/call" || m.Params.Name != "search_symbols" {
		t.Fatalf("unexpected frame: %s", frame)
	}
	if m.Params.Arguments["format"] != "json" {
		t.Fatalf("format must be pinned to json, got %v", m.Params.Arguments["format"])
	}
	if m.Params.Arguments["query"] != "Foo" {
		t.Fatalf("caller args must be preserved, got %v", m.Params.Arguments)
	}
}

// TestBuildToolCallFrame_NilArgs asserts a nil args map still pins json.
func TestBuildToolCallFrame_NilArgs(t *testing.T) {
	frame, err := buildToolCallFrame(2, "graph_stats", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(frame, `"format":"json"`) {
		t.Fatalf("nil args must still pin json: %s", frame)
	}
}

func contains(b []byte, sub string) bool {
	return len(b) >= len(sub) && (string(b) == sub || indexOf(string(b), sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// TestExtractToolResult_RepoNotTracked maps the daemon's typed refusal to
// the sentinel so the CLI falls back instead of erroring.
func TestExtractToolResult_RepoNotTracked(t *testing.T) {
	resp := []byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"repository not tracked","data":{"error_code":"repo_not_tracked"}}}`)
	_, err := extractToolResult(resp)
	if !errors.Is(err, ErrRepoNotTracked) {
		t.Fatalf("want ErrRepoNotTracked, got %v", err)
	}
}

// TestExtractToolResult_RealErrorSurfaced asserts a genuine tool error is
// surfaced verbatim (not swallowed as "not tracked").
func TestExtractToolResult_RealErrorSurfaced(t *testing.T) {
	resp := []byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32602,"message":"bad symbol id"}}`)
	_, err := extractToolResult(resp)
	if err == nil || errors.Is(err, ErrRepoNotTracked) {
		t.Fatalf("a real error must surface, got %v", err)
	}
	if err.Error() != "bad symbol id" {
		t.Fatalf("error message should be verbatim, got %q", err.Error())
	}
}

// TestExtractToolResult_Success returns the tool JSON payload from the
// result content block.
func TestExtractToolResult_Success(t *testing.T) {
	resp := []byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"{\"total\":3}"}]}}`)
	out, err := extractToolResult(resp)
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Total int `json:"total"`
	}
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("payload should be the tool json: %v (%s)", err, out)
	}
	if payload.Total != 3 {
		t.Fatalf("want total 3, got %d", payload.Total)
	}
}
