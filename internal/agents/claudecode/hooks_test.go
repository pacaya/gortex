package claudecode

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHookCommandPathIsEphemeral covers every branch of the
// ephemeral-path detector because this function's output directly
// decides whether a user's settings.local.json gets rewritten on
// re-run. A false positive here would thrash the user's hook
// config; a false negative leaves them with stale /tmp paths.
func TestHookCommandPathIsEphemeral(t *testing.T) {
	// /bin/sh is a stable POSIX path, not under any ephemeral root.
	// os.Executable() would land in /private/var/folders under go
	// test, which is itself ephemeral — hence hard-coding.
	const existing = "/bin/sh"
	if _, err := os.Stat(existing); err != nil {
		t.Skipf("test fixture %s not present: %v", existing, err)
	}

	missing := filepath.Join("/nonexistent-root-for-gortex-test", "ghost-binary")

	cases := []struct {
		name    string
		cmd     string
		want    bool
		comment string
	}{
		{"empty", "", false, "no fields to inspect"},
		{"bareName", "gortex hook", false, "PATH lookup happens at fire time"},
		{"relative", "./gortex hook", false, "relative paths are user choice, not ephemeral"},
		{"tmp", "/tmp/gortex-hook-fix hook", true, "/tmp is wiped between sessions"},
		{"varFolders", "/var/folders/x/y/z/gortex hook", true, "macOS go-build cache"},
		{"privateTmp", "/private/tmp/gortex hook", true, "macOS resolves /tmp via /private/tmp"},
		{"privateVarFolders", "/private/var/folders/x/y/z/gortex hook", true, "fully resolved go-build cache"},
		{"missingAbsolute", missing + " hook", true, "absolute path that no longer exists"},
		{"healthyAbsolute", existing + " hook", false, "absolute path that exists on disk"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := HookCommandPathIsEphemeral(tc.cmd)
			assert.Equal(t, tc.want, got, tc.comment)
		})
	}
}

func TestHealStaleHookCommands(t *testing.T) {
	const newCmd = "/opt/homebrew/bin/gortex hook"

	t.Run("emptyHooks", func(t *testing.T) {
		hooks := map[string]any{}
		got := healStaleHookCommands(hooks, newCmd)
		assert.Equal(t, 0, got)
	})

	t.Run("noGortexEntries", func(t *testing.T) {
		hooks := map[string]any{
			"PreToolUse": []any{
				makeHookEntry("Read", "/usr/local/bin/some-other-tool run"),
			},
		}
		got := healStaleHookCommands(hooks, newCmd)
		assert.Equal(t, 0, got)
		entries := hooks["PreToolUse"].([]any)
		inner := entries[0].(map[string]any)["hooks"].([]any)
		cmd := inner[0].(map[string]any)["command"].(string)
		assert.Equal(t, "/usr/local/bin/some-other-tool run", cmd)
	})

	t.Run("healthyGortexEntryUntouched", func(t *testing.T) {
		hooks := map[string]any{
			"PreToolUse": []any{makeHookEntry("Read", "./gortex hook")},
		}
		got := healStaleHookCommands(hooks, newCmd)
		assert.Equal(t, 0, got)
		assert.Equal(t, "./gortex hook", extractCmd(t, hooks, "PreToolUse", 0))
	})

	t.Run("staleEntryRewritten", func(t *testing.T) {
		hooks := map[string]any{
			"Stop": []any{makeHookEntry("", "/tmp/gortex-hook-fix hook")},
		}
		got := healStaleHookCommands(hooks, newCmd)
		assert.Equal(t, 1, got)
		assert.Equal(t, newCmd, extractCmd(t, hooks, "Stop", 0))
	})

	t.Run("multipleEventsAndMixed", func(t *testing.T) {
		hooks := map[string]any{
			"PreToolUse": []any{
				makeHookEntry("Read", "./gortex hook"),
				makeHookEntry("Read", "/usr/local/bin/lint --strict"),
			},
			"PreCompact": []any{makeHookEntry("", "/tmp/gortex-hook-fix hook")},
			"Stop":       []any{makeHookEntry("", "/tmp/gortex-hook-fix hook")},
		}
		got := healStaleHookCommands(hooks, newCmd)
		assert.Equal(t, 2, got)
		assert.Equal(t, "./gortex hook", extractCmd(t, hooks, "PreToolUse", 0))
		assert.Equal(t, "/usr/local/bin/lint --strict", extractCmd(t, hooks, "PreToolUse", 1))
		assert.Equal(t, newCmd, extractCmd(t, hooks, "PreCompact", 0))
		assert.Equal(t, newCmd, extractCmd(t, hooks, "Stop", 0))
	})
}

func TestResolveHookCommand(t *testing.T) {
	t.Run("foundOnPath", func(t *testing.T) {
		dir := t.TempDir()
		fake := filepath.Join(dir, "gortex")
		require.NoError(t, os.WriteFile(fake, []byte("#!/bin/sh\nexit 0\n"), 0o755))
		t.Setenv("PATH", dir)

		got := ResolveHookCommand(io.Discard)
		assert.Equal(t, fake+" hook", got, "should resolve to absolute path on PATH")
	})

	t.Run("notFoundFallsBackToBare", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		got := ResolveHookCommand(io.Discard)
		assert.Equal(t, "gortex hook", got, "fallback to bare name keeps init working in sandboxes")
	})
}

func makeHookEntry(matcher, command string) map[string]any {
	entry := map[string]any{
		"hooks": []any{
			map[string]any{
				"type":    "command",
				"command": command,
				"timeout": 3000,
			},
		},
	}
	if matcher != "" {
		entry["matcher"] = matcher
	}
	return entry
}

func extractCmd(t *testing.T, hooks map[string]any, event string, idx int) string {
	t.Helper()
	list, ok := hooks[event].([]any)
	require.True(t, ok, "event %q missing", event)
	require.Greater(t, len(list), idx, "event %q has fewer than %d entries", event, idx+1)
	entry, ok := list[idx].(map[string]any)
	require.True(t, ok)
	inner, ok := entry["hooks"].([]any)
	require.True(t, ok)
	require.NotEmpty(t, inner)
	em, ok := inner[0].(map[string]any)
	require.True(t, ok)
	cmd, _ := em["command"].(string)
	return cmd
}
