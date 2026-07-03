package lsp

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// TestOmniSharpSpec_HasCSharpLsAlternative pins the registry wiring: the C#
// server spec still serves csharp and now offers csharp-ls (a Roslyn stdio
// LSP that is far more commonly installed than OmniSharp) as an alternative
// command.
func TestOmniSharpSpec_HasCSharpLsAlternative(t *testing.T) {
	spec := SpecByName("omnisharp")
	require.NotNil(t, spec, "omnisharp spec must exist")

	servesCS := false
	for _, l := range spec.Languages {
		if l == "csharp" {
			servesCS = true
		}
	}
	require.True(t, servesCS, "omnisharp spec must still list csharp")

	hasAlt := false
	for _, alt := range spec.AlternativeCommands {
		if alt.Command == "csharp-ls" {
			hasAlt = true
		}
	}
	require.True(t, hasAlt, "omnisharp spec must offer csharp-ls as an alternative")
}

// TestProviderFromSpec_ResolvesCSharpLsAlternative verifies that when omnisharp
// is not on PATH but csharp-ls is, NewProviderFromSpec resolves to csharp-ls —
// and the resulting Provider still reports servesCSharp() == true, so the
// C#-scoped hardening (pre-restore, NU19xx advisory-diagnostic filter,
// MSBuild-wedge timeout) applies to the alternative unchanged.
func TestProviderFromSpec_ResolvesCSharpLsAlternative(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PATH exec-bit faking is POSIX-only")
	}
	dir := t.TempDir()
	// A fake csharp-ls on PATH; omnisharp is absent because PATH is only dir.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "csharp-ls"), []byte("#!/bin/sh\n"), 0o755))
	t.Setenv("PATH", dir)

	p := NewProviderFromSpec(SpecByName("omnisharp"), zap.NewNop())
	require.NotNil(t, p)
	assert.Equal(t, "csharp-ls", p.command, "alt command should win when omnisharp is off PATH")
	assert.True(t, p.servesCSharp(), "a csharp-ls-constructed provider still serves C#")
	assert.Contains(t, p.Languages(), "csharp")
}
