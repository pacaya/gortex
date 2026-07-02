package lsp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The jdtls launch must pass -data pointing inside the resolved cache home
// (honouring XDG_CACHE_HOME), per repo — never letting the launcher default
// the Eclipse workspace to ~/Library/Caches/jdtls.
func TestJdtlsDataArgs_UnderCacheHome(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)

	repo := "/Users/dev/code/petclinic"
	args := jdtlsDataArgs([]string{}, repo)

	require.Len(t, args, 2)
	assert.Equal(t, "-data", args[0])
	data := args[1]
	assert.True(t, strings.HasPrefix(data, cache), "workspace %q must live under the resolved cache home %q", data, cache)
	assert.Contains(t, data, filepath.Join("lsp", "jdtls"))
	// The workspace directory is created.
	info, err := os.Stat(data)
	require.NoError(t, err)
	assert.True(t, info.IsDir())

	// Distinct repos get distinct per-repo workspaces; the same repo is stable.
	assert.Equal(t, data, jdtlsDataArgs([]string{}, repo)[1])
	assert.NotEqual(t, data, jdtlsDataArgs([]string{}, "/Users/dev/code/other")[1])
}

// An explicit -data configured by the operator is respected, not doubled.
func TestJdtlsDataArgs_RespectsExplicit(t *testing.T) {
	base := []string{"-data", "/custom/ws"}
	assert.Equal(t, base, jdtlsDataArgs(base, "/Users/dev/code/petclinic"))
}

// A stale Eclipse lock left by a crashed jdtls is cleared so the next launch is
// not wedged; the workspace directory (and its cached index) survives.
func TestPrepareJdtlsWorkspace_ClearsStaleLock(t *testing.T) {
	dir := t.TempDir()
	meta := filepath.Join(dir, ".metadata")
	require.NoError(t, os.MkdirAll(meta, 0o755))
	lock := filepath.Join(meta, ".lock")
	require.NoError(t, os.WriteFile(lock, []byte("stale"), 0o644))

	prepareJdtlsWorkspace(dir)

	_, err := os.Stat(lock)
	assert.True(t, os.IsNotExist(err), "stale .metadata/.lock must be removed")
	_, err = os.Stat(meta)
	assert.NoError(t, err, "the workspace .metadata (cached index) must be preserved")
}

func TestIsJdtlsCommand(t *testing.T) {
	assert.True(t, isJdtlsCommand("jdtls"))
	assert.True(t, isJdtlsCommand("/opt/homebrew/bin/jdtls"))
	assert.False(t, isJdtlsCommand("gopls"))
	assert.False(t, isJdtlsCommand("/usr/bin/rust-analyzer"))
}
