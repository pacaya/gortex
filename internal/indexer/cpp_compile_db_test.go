package indexer

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeCompileDB writes a compile_commands.json at path and registers cleanup of
// the per-root include-dir cache so each test starts cold.
func writeCompileDB(t *testing.T, repoRoot, path, body string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(body), 0o644))
	t.Cleanup(func() { clearCppIncludeDirCache(repoRoot) })
}

func TestLoadCompileCommands_CommandString(t *testing.T) {
	root := t.TempDir()
	writeCompileDB(t, root, filepath.Join(root, "compile_commands.json"), `[
	  {
	    "directory": "`+root+`/build",
	    "file": "../src/main.c",
	    "command": "clang -I../gen/include -isystem /usr/include -c ../src/main.c -o main.o"
	  }
	]`)

	tus := loadCompileCommands(root)
	tu, ok := tus["src/main.c"]
	require.True(t, ok, "main.c TU resolved repo-relative from build/ directory")
	assert.Equal(t, "src/main.c", tu.file)
	// `-I../gen/include` becomes repo-relative; the toolchain `-isystem` path is
	// outside the repo and dropped.
	assert.Equal(t, []string{"gen/include"}, tu.includeDirs)
}

func TestLoadCompileCommands_ArgumentsArray(t *testing.T) {
	root := t.TempDir()
	writeCompileDB(t, root, filepath.Join(root, "compile_commands.json"), `[
	  {
	    "directory": "`+root+`",
	    "file": "src/main.cpp",
	    "arguments": ["clang++", "-Igen/include", "-I", "vendor/inc", "-iquote", "local", "-c", "src/main.cpp"]
	  }
	]`)

	tus := loadCompileCommands(root)
	tu, ok := tus["src/main.cpp"]
	require.True(t, ok)
	assert.Equal(t, []string{"gen/include", "vendor/inc", "local"}, tu.includeDirs)
}

func TestLoadCompileCommands_BuildDirGlob(t *testing.T) {
	root := t.TempDir()
	// No root compile_commands.json — only build-debug/compile_commands.json.
	writeCompileDB(t, root, filepath.Join(root, "build-debug", "compile_commands.json"), `[
	  {
	    "directory": "`+root+`/build-debug",
	    "file": "../src/app.c",
	    "command": "cc -I../include -c ../src/app.c"
	  }
	]`)

	tus := loadCompileCommands(root)
	tu, ok := tus["src/app.c"]
	require.True(t, ok, "TU discovered via build*/compile_commands.json glob")
	assert.Equal(t, []string{"include"}, tu.includeDirs)
}

func TestLoadCompileCommands_None(t *testing.T) {
	root := t.TempDir()
	t.Cleanup(func() { clearCppIncludeDirCache(root) })
	assert.Empty(t, loadCompileCommands(root), "no compile_commands.json yields empty map")
}
