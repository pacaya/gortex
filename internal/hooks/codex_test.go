package hooks

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestRunCodexMalformedJSONNoop(t *testing.T) {
	out := captureStdout(t, func() { runCodex([]byte(`{`), 0) })
	if out != "" {
		t.Fatalf("malformed JSON should be silent, got %q", out)
	}
}

func TestRunCodexPostToolUseWithoutParseableOutputSilent(t *testing.T) {
	data := []byte(`{"hook_event_name":"PostToolUse","tool_name":"Bash","tool_input":{"command":"rg Foo"}}`)
	out := captureStdout(t, func() { runCodex(data, 0) })
	if out != "" {
		t.Fatalf("PostToolUse without parseable output should be silent, got %q", out)
	}
}

func TestRunCodexIgnoresNonBash(t *testing.T) {
	data := []byte(`{"hook_event_name":"PreToolUse","tool_name":"Read","tool_input":{"file_path":"internal/x.go"}}`)
	out := captureStdout(t, func() { runCodex(data, 0) })
	if out != "" {
		t.Fatalf("non-Bash PreToolUse should be silent, got %q", out)
	}
}

func TestRunCodexPreToolUseBashSoftAdditionalContext(t *testing.T) {
	oldProbe := grepProbe
	grepProbe = func(string, time.Duration) ([]grepSymbolHit, error) {
		return nil, errDaemonUnreachable
	}
	t.Cleanup(func() { grepProbe = oldProbe })

	data := codexBashPayload("rg Foo")
	out := captureStdout(t, func() {
		withStdin(t, data, func() { RunCodex(0) })
	})
	if out == "" {
		t.Fatal("expected Codex Bash PreToolUse guidance, got empty output")
	}
	dec := decodeHookOutput(t, out)
	if dec.HookSpecificOutput == nil {
		t.Fatalf("missing hookSpecificOutput: %s", out)
	}
	hso := dec.HookSpecificOutput
	if hso.HookEventName != "PreToolUse" {
		t.Fatalf("hookEventName=%q want PreToolUse", hso.HookEventName)
	}
	if !strings.Contains(hso.AdditionalContext, "PREFER graph tools over Grep") {
		t.Fatalf("additionalContext missing graph guidance: %q", hso.AdditionalContext)
	}
	if hso.PermissionDecision != "" || hso.PermissionDecisionReason != "" {
		t.Fatalf("Codex soft nudge must not deny: %#v", hso)
	}
}

func TestRunCodexPostToolUseBashGrepOutputAdditionalContext(t *testing.T) {
	port := stubBridge(t, nil,
		map[string]struct{ ID, Name, Kind string }{
			"internal/a.go:7": {ID: "internal/a.go::MyType", Name: "MyType", Kind: "type"},
		}, nil)

	data := codexPostBashPayload("rg -n MyType", "internal/a.go:7:type MyType struct{}\n")
	out := captureStdout(t, func() { runCodex(data, port) })
	if out == "" {
		t.Fatal("expected Codex Bash PostToolUse graph context, got empty output")
	}
	hso := decodeHookOutput(t, out).HookSpecificOutput
	if hso == nil {
		t.Fatalf("missing hookSpecificOutput: %s", out)
	}
	if hso.HookEventName != "PostToolUse" {
		t.Fatalf("hookEventName=%q want PostToolUse", hso.HookEventName)
	}
	if !strings.Contains(hso.AdditionalContext, "type MyType") {
		t.Fatalf("additionalContext missing enclosing symbol: %q", hso.AdditionalContext)
	}
	if hso.PermissionDecision != "" || hso.PermissionDecisionReason != "" {
		t.Fatalf("Codex PostToolUse enrichment must not deny: %#v", hso)
	}
}

func TestRunCodexPostToolUseBashCommandShapes(t *testing.T) {
	tests := []struct {
		name     string
		command  string
		response string
	}{
		{
			name:     "grep with no path line output stays quiet",
			command:  "grep -rn handleFoo .",
			response: "no matches\n",
		},
		{
			name:     "piped grep is filter not search",
			command:  "go test ./... | grep FAIL",
			response: "pkg/x_test.go:12: FAIL\n",
		},
		{
			name:     "find name is left to PreToolUse only",
			command:  `find . -name "Handler*"`,
			response: "internal/handler.go\n",
		},
		{
			name:     "cat source output stays quiet",
			command:  "cat /repo/handler.go",
			response: "internal/a.go:7:type MyType struct{}\n",
		},
		{
			name:     "unsupported sed range read stays quiet",
			command:  `sed -n '1,80p' /repo/handler.go`,
			response: "internal/a.go:7:type MyType struct{}\n",
		},
		{
			name:     "unsupported awk source scan stays quiet",
			command:  `awk 'NR>=1 && NR<=80 {print}' /repo/handler.go`,
			response: "internal/a.go:7:type MyType struct{}\n",
		},
		{
			name:     "ls stays quiet",
			command:  "ls /repo",
			response: "internal/a.go\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			port := stubBridge(t, nil,
				map[string]struct{ ID, Name, Kind string }{
					"internal/a.go:7": {ID: "internal/a.go::MyType", Name: "MyType", Kind: "type"},
				}, nil)

			data := codexPostBashPayload(tt.command, tt.response)
			out := captureStdout(t, func() { runCodex(data, port) })
			if out != "" {
				t.Fatalf("expected silent no-op, got %q", out)
			}
		})
	}
}

func TestRunCodexPostToolUseIgnoresNonBash(t *testing.T) {
	data := []byte(`{"hook_event_name":"PostToolUse","tool_name":"Read","tool_input":{"file_path":"internal/a.go"},"tool_response":"internal/a.go:7:type MyType struct{}\n"}`)
	out := captureStdout(t, func() { runCodex(data, 0) })
	if out != "" {
		t.Fatalf("non-Bash PostToolUse should be silent, got %q", out)
	}
}

func TestRunCodexPostToolUseMalformedJSONNoop(t *testing.T) {
	out := captureStdout(t, func() { runCodexPostToolUse([]byte(`{`), 0) })
	if out != "" {
		t.Fatalf("malformed JSON should be silent, got %q", out)
	}
}

func codexBashPayload(command string) []byte {
	return []byte(`{"hook_event_name":"PreToolUse","tool_name":"Bash","session_id":"codex-shape","tool_input":{"command":` + strconv.Quote(command) + `}}`)
}

func codexPostBashPayload(command string, response string) []byte {
	return []byte(`{"hook_event_name":"PostToolUse","tool_name":"Bash","session_id":"codex-shape","tool_input":{"command":` + strconv.Quote(command) + `},"tool_response":` + strconv.Quote(response) + `}`)
}
