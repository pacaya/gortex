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

func TestRunCodexPostToolUseBashReadSourceAdditionalContext(t *testing.T) {
	tests := []struct {
		name    string
		command string
	}{
		{
			name:    "cat",
			command: "cat internal/a.go",
		},
		{
			name:    "head",
			command: "head -20 internal/a.go",
		},
		{
			name:    "tail",
			command: "tail -n 50 internal/a.go",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			port := stubBridge(t, map[string]int{"internal/a.go": 3}, nil, map[string]int{"internal/a.go": 2})

			data := codexPostBashPayload(tt.command, "package internal\n")
			out := captureStdout(t, func() { runCodex(data, port) })
			if out == "" {
				t.Fatal("expected Codex Bash PostToolUse Read graph context, got empty output")
			}
			hso := decodeHookOutput(t, out).HookSpecificOutput
			if hso == nil {
				t.Fatalf("missing hookSpecificOutput: %s", out)
			}
			if hso.HookEventName != "PostToolUse" {
				t.Fatalf("hookEventName=%q want PostToolUse", hso.HookEventName)
			}
			if !strings.Contains(hso.AdditionalContext, "Graph footprint for internal/a.go") {
				t.Fatalf("additionalContext missing file footprint: %q", hso.AdditionalContext)
			}
			if !strings.Contains(hso.AdditionalContext, "3 indexed symbol(s)") {
				t.Fatalf("additionalContext missing symbol count: %q", hso.AdditionalContext)
			}
			if !strings.Contains(hso.AdditionalContext, "2 unique external caller(s)") {
				t.Fatalf("additionalContext missing caller count: %q", hso.AdditionalContext)
			}
			if hso.PermissionDecision != "" || hso.PermissionDecisionReason != "" {
				t.Fatalf("Codex PostToolUse enrichment must not deny: %#v", hso)
			}
		})
	}
}

func TestRunCodexPostToolUseBashFindNameFileListAdditionalContext(t *testing.T) {
	tests := []struct {
		name     string
		command  string
		response string
		indexed  map[string]int
		want     []string
	}{
		{
			name:     "find name",
			command:  `find . -name "*.go"`,
			response: "internal/hooks/codex.go\ninternal/hooks/codex_test.go\nREADME.md\n",
			indexed: map[string]int{
				"internal/hooks/codex.go":      4,
				"internal/hooks/codex_test.go": 9,
			},
			want: []string{
				"Indexed 2/3 Glob match(es)",
				"internal/hooks/codex_test.go",
				"9 symbol(s)",
				"internal/hooks/codex.go",
				"4 symbol(s)",
			},
		},
		{
			name:     "find iname",
			command:  `find internal -iname "*hook*.go"`,
			response: "internal/hooks/codex.go\ninternal/hooks/posttooluse.go\n",
			indexed: map[string]int{
				"internal/hooks/codex.go":       4,
				"internal/hooks/posttooluse.go": 7,
			},
			want: []string{
				"Indexed 2/2 Glob match(es)",
				"internal/hooks/posttooluse.go",
				"7 symbol(s)",
				"internal/hooks/codex.go",
				"4 symbol(s)",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			port := stubBridge(t, tt.indexed, nil, nil)

			data := codexPostBashPayload(tt.command, tt.response)
			out := captureStdout(t, func() { runCodex(data, port) })
			if out == "" {
				t.Fatal("expected Codex Bash PostToolUse Glob graph context, got empty output")
			}
			hso := decodeHookOutput(t, out).HookSpecificOutput
			if hso == nil {
				t.Fatalf("missing hookSpecificOutput: %s", out)
			}
			if hso.HookEventName != "PostToolUse" {
				t.Fatalf("hookEventName=%q want PostToolUse", hso.HookEventName)
			}
			for _, want := range tt.want {
				if !strings.Contains(hso.AdditionalContext, want) {
					t.Fatalf("additionalContext missing %q: %q", want, hso.AdditionalContext)
				}
			}
			if hso.PermissionDecision != "" || hso.PermissionDecisionReason != "" {
				t.Fatalf("Codex PostToolUse enrichment must not deny: %#v", hso)
			}
		})
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
			name:     "unindexed find name list stays quiet",
			command:  `find . -name "Handler*"`,
			response: "unindexed/handler.go\n",
		},
		{
			name:     "unindexed cat source output stays quiet",
			command:  "cat /repo/handler.go",
			response: "internal/a.go:7:type MyType struct{}\n",
		},
		{
			name:     "unsupported sed range read stays quiet",
			command:  `sed -n '1,80p' /repo/handler.go`,
			response: "internal/a.go:7:type MyType struct{}\n",
		},
		{
			name:     "sed source read stays quiet",
			command:  `sed -n '1,20p' internal/a.go`,
			response: "package hooks\n",
		},
		{
			name:     "unsupported awk source scan stays quiet",
			command:  `awk 'NR>=1 && NR<=80 {print}' /repo/handler.go`,
			response: "internal/a.go:7:type MyType struct{}\n",
		},
		{
			name:     "awk source read stays quiet",
			command:  `awk '{print}' internal/a.go`,
			response: "package hooks\n",
		},
		{
			name:     "ls stays quiet",
			command:  "ls /repo",
			response: "internal/a.go\n",
		},
		{
			name:     "fd stays quiet",
			command:  `fd '\.go$' internal`,
			response: "internal/a.go\n",
		},
		{
			name:     "tree stays quiet",
			command:  "tree internal",
			response: "internal/a.go\n",
		},
		{
			name:     "git ls-files stays quiet",
			command:  "git ls-files '*.go'",
			response: "internal/a.go\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			port := stubBridge(t, map[string]int{"internal/a.go": 3},
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
