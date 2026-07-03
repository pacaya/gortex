package hooks

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestRunKimiUserPromptSubmitPlainStdout(t *testing.T) {
	restore := stubUserPromptProbe(t, []grepSymbolHit{
		{Name: "ValidateToken", Kind: "function", FilePath: "internal/auth/token.go", Line: 42},
	}, nil)
	defer restore()

	out := captureStdout(t, func() {
		runKimi([]byte(`{"hook_event_name":"UserPromptSubmit","prompt":"fix auth token validation"}`))
	})
	if out == "" {
		t.Fatal("expected Kimi prompt context, got empty output")
	}
	if strings.Contains(out, "hookSpecificOutput") || strings.Contains(out, "additionalContext") {
		t.Fatalf("Kimi dispatcher should emit plain stdout, got JSON-shaped output:\n%s", out)
	}
	if !strings.Contains(out, "ValidateToken") {
		t.Fatalf("missing symbol context:\n%s", out)
	}
}

func TestRunKimiUserPromptSubmitContentParts(t *testing.T) {
	restore := stubUserPromptProbe(t, []grepSymbolHit{
		{Name: "AuthMiddleware", Kind: "type", FilePath: "internal/auth/mw.go"},
	}, nil)
	defer restore()

	out := captureStdout(t, func() {
		runKimi([]byte(`{"hook_event_name":"UserPromptSubmit","prompt":[{"type":"text","text":"trace auth middleware"},{"type":"image","source":"ignored"}]}`))
	})
	if !strings.Contains(out, "AuthMiddleware") {
		t.Fatalf("content-parts prompt did not produce context:\n%s", out)
	}
}

func TestRunKimiNoopShapes(t *testing.T) {
	restore := stubUserPromptProbe(t, nil, errors.New("should not be called"))
	defer restore()

	cases := [][]byte{
		[]byte(`{`),
		[]byte(`{"hook_event_name":"PreToolUse","prompt":"fix auth"}`),
		[]byte(`{"hook_event_name":"UserPromptSubmit","prompt":"/clear"}`),
	}
	for _, tc := range cases {
		out := captureStdout(t, func() { runKimi(tc) })
		if out != "" {
			t.Fatalf("expected no output for %s, got %q", tc, out)
		}
	}
}

func stubUserPromptProbe(t *testing.T, hits []grepSymbolHit, err error) func() {
	t.Helper()
	old := userPromptProbe
	userPromptProbe = func(string, time.Duration) ([]grepSymbolHit, error) {
		return hits, err
	}
	return func() { userPromptProbe = old }
}
