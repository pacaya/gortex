package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestMaybeToolInvocationHint(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		wantHint bool
		// substrings that must appear in the emitted hint when wantHint is true
		wantContains []string
	}{
		{
			name:         "read_file tool name",
			args:         []string{"read_file", "foo.go"},
			wantHint:     true,
			wantContains: []string{"gortex call read_file --arg path=<file>", "not a gortex command"},
		},
		{
			name:         "get_symbol_source tool name",
			args:         []string{"get_symbol_source"},
			wantHint:     true,
			wantContains: []string{"gortex call get_symbol_source --arg"},
		},
		{
			name:         "tool name behind leading global flag",
			args:         []string{"--config", "x.yaml", "read_file", "foo.go"},
			wantHint:     true,
			wantContains: []string{"gortex call read_file --arg path=<file>"},
		},
		{
			name:     "real cobra command is not intercepted",
			args:     []string{"daemon", "status"},
			wantHint: false,
		},
		{
			name:     "call itself is a real command",
			args:     []string{"call", "read_file"},
			wantHint: false,
		},
		{
			name:     "genuinely unknown non-tool falls through to cobra",
			args:     []string{"florpwidget"},
			wantHint: false,
		},
		{
			name:     "no args",
			args:     nil,
			wantHint: false,
		},
		{
			name:     "help flag is not a tool",
			args:     []string{"--help"},
			wantHint: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			got := maybeToolInvocationHint(&buf, tc.args)
			if got != tc.wantHint {
				t.Fatalf("maybeToolInvocationHint(%v) = %v, want %v (output: %q)", tc.args, got, tc.wantHint, buf.String())
			}
			if !tc.wantHint {
				if buf.Len() != 0 {
					t.Errorf("expected no output when no hint fires, got %q", buf.String())
				}
				return
			}
			out := buf.String()
			// Must never emit the invalid bare `gortex <verb>` shape (only the
			// `gortex call <verb>` fallback is valid).
			verb := firstPositionalArg(tc.args)
			if strings.Contains(out, "gortex "+verb+" ") || strings.HasSuffix(strings.TrimSpace(out), "gortex "+verb) {
				t.Errorf("hint emitted the invalid bare shape `gortex %s`:\n%s", verb, out)
			}
			for _, want := range tc.wantContains {
				if !strings.Contains(out, want) {
					t.Errorf("hint missing %q:\n%s", want, out)
				}
			}
			if !strings.Contains(out, "gortex call ") {
				t.Errorf("hint must teach the `gortex call` shape:\n%s", out)
			}
		})
	}
}
