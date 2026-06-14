package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLoadRepoGitignore_SkipsBlanksCommentsAndNonUTF8(t *testing.T) {
	dir := t.TempDir()
	// A valid file interleaved with a blank line, a comment, and a
	// non-UTF-8 (e.g. DLP-encrypted) line that must be skipped, not fed to
	// the matcher and not aborting the read.
	content := "# comment\n\nnode_modules/\n" + string([]byte{0xff, 0xfe, 0xfd}) + "\n*.log\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".gitignore"), []byte(content), 0o644))

	got := loadRepoGitignore(dir)
	require.Equal(t, []string{"node_modules/", "*.log"}, got)
}

func TestLoadRepoGitignore_LongLineDoesNotAbort(t *testing.T) {
	dir := t.TempDir()
	// A pathologically long line (larger than the default 64 KiB scanner
	// buffer) must not discard the patterns that follow it.
	long := make([]byte, 200*1024)
	for i := range long {
		long[i] = 'a'
	}
	content := "*.tmp\n" + string(long) + "\nbuild/\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".gitignore"), []byte(content), 0o644))

	got := loadRepoGitignore(dir)
	require.Contains(t, got, "*.tmp")
	require.Contains(t, got, "build/")
}

func TestLoadRepoGitignore_MissingFileIsNil(t *testing.T) {
	require.Nil(t, loadRepoGitignore(t.TempDir()))
	require.Nil(t, loadRepoGitignore(""))
}
