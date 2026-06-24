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

// TestHeuristicIncludeDirs pins the no-compile-DB fallback: conventional roots
// in priority order, followed by any other top-level dir directly containing a
// C/C++ header; dirs without headers and dotfile dirs are skipped.
func TestHeuristicIncludeDirs(t *testing.T) {
	root := t.TempDir()
	mk := func(parts ...string) {
		require.NoError(t, os.MkdirAll(filepath.Join(append([]string{root}, parts...)...), 0o755))
	}
	wr := func(rel, body string) {
		require.NoError(t, os.WriteFile(filepath.Join(root, rel), []byte(body), 0o644))
	}

	mk("include", "proj") // conventional; header is nested, not direct
	wr("include/proj/api.h", "// h")
	mk("src") // conventional, exists
	mk("thirdparty")
	wr("thirdparty/lib.hpp", "// h") // non-conventional dir with a direct header
	mk("docs")
	wr("docs/readme.md", "x") // no header → ignored
	mk(".git")
	wr(".git/x.h", "x") // dotfile dir → skipped

	got := heuristicIncludeDirs(root)
	assert.Equal(t, []string{"include", "src", "thirdparty"}, got)
}

func TestHeuristicIncludeDirs_Empty(t *testing.T) {
	assert.Nil(t, heuristicIncludeDirs(""))
	assert.Empty(t, heuristicIncludeDirs(t.TempDir()), "no conventional roots and no header dirs")
}

// TestLoadCompileCommands_CacheAndClear pins the cache-and-invalidate contract:
// a second load returns the cached include dirs even after the file changes on
// disk, and clearing the cache (as an incremental reindex does) re-reads it —
// so an edited compile_commands.json takes effect without a daemon restart.
func TestLoadCompileCommands_CacheAndClear(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "compile_commands.json")
	t.Cleanup(func() { clearCppIncludeDirCache(root) })

	write := func(incDir string) {
		body := `[{"directory":"` + root + `","file":"src/main.c","command":"cc -I` + incDir + ` -c src/main.c"}]`
		require.NoError(t, os.WriteFile(path, []byte(body), 0o644))
	}

	write("inc1")
	require.Equal(t, []string{"inc1"}, loadCompileCommands(root)["src/main.c"].includeDirs)

	// Editing the DB on disk is not observed until the cache is invalidated.
	write("inc2")
	assert.Equal(t, []string{"inc1"}, loadCompileCommands(root)["src/main.c"].includeDirs,
		"second load returns the cached result")

	// Clearing the cache (the incremental-reindex hook) re-reads the new dir.
	clearCppIncludeDirCache(root)
	assert.Equal(t, []string{"inc2"}, loadCompileCommands(root)["src/main.c"].includeDirs,
		"after clear the edited -I dir is picked up")
}
