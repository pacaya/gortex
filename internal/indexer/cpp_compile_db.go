package indexer

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// cppTU is one C/C++ translation unit's reconstructed include search path,
// keyed in the compile-DB result by the TU's repo-relative source path.
type cppTU struct {
	file        string   // repo-relative source path
	includeDirs []string // repo-relative -I / -isystem / -iquote dirs, in order
}

// compileCommand is one entry of a compile_commands.json database.
type compileCommand struct {
	Directory string   `json:"directory"`
	File      string   `json:"file"`
	Command   string   `json:"command"`
	Arguments []string `json:"arguments"`
}

// cppIncludeDirCache is the long-lived, memory-bounded LRU of per-repo-root
// compile-DB include-dir sets (bounded by GORTEX_RESOLVER_CACHE_MAX_MB).
var cppIncludeDirCache = newCppIncludeDirCache()

// loadCompileCommands parses compile_commands.json at the repo root (and any
// build*/compile_commands.json), reconstructing each TU's ordered include search
// path (the `-I` / `-isystem` / `-iquote` dirs) normalized to repo-relative slash
// paths, dropping directories outside the repo (toolchain / system). The result
// is cached per repo root, keyed on the newest compile_commands.json modtime: a
// later edit to compile_commands.json (even with no other source change) is
// re-read on the next load. clearCppIncludeDirCache also invalidates it on a full
// reindex.
func loadCompileCommands(repoRoot string) map[string]cppTU {
	if repoRoot == "" {
		return nil
	}
	mtime := compileDBMtime(repoRoot)
	if c, ok := cppIncludeDirCache.get(repoRoot, mtime); ok {
		return c
	}

	out := map[string]cppTU{}
	for _, dbPath := range compileDBLocations(repoRoot) {
		data, err := os.ReadFile(dbPath)
		if err != nil {
			continue
		}
		var cmds []compileCommand
		if json.Unmarshal(data, &cmds) != nil {
			continue
		}
		for _, cc := range cmds {
			fileRel := repoRelPath(repoRoot, cc.Directory, cc.File)
			if fileRel == "" {
				continue
			}
			out[fileRel] = cppTU{file: fileRel, includeDirs: extractIncludeDirs(cc, repoRoot)}
		}
	}

	cppIncludeDirCache.put(repoRoot, out, mtime)
	return out
}

// compileDBMtime returns the newest modtime (unix nanoseconds) across the
// compile_commands.json files that loadCompileCommands reads for repoRoot, or 0
// when none exist. It is the cache freshness key: when this exceeds the cached
// entry's recorded mtime, the entry is reloaded.
func compileDBMtime(repoRoot string) int64 {
	var newest int64
	for _, dbPath := range compileDBLocations(repoRoot) {
		fi, err := os.Stat(dbPath)
		if err != nil {
			continue
		}
		if m := fi.ModTime().UnixNano(); m > newest {
			newest = m
		}
	}
	return newest
}

// clearCppIncludeDirCache drops the cached include-dir set for a repo root so
// the next index re-reads compile_commands.json.
func clearCppIncludeDirCache(repoRoot string) {
	cppIncludeDirCache.clear(repoRoot)
}

// compileDBLocations returns the absolute paths of the compile_commands.json
// files to consider: the repo root plus any build*/compile_commands.json.
func compileDBLocations(repoRoot string) []string {
	var out []string
	root := filepath.Join(repoRoot, "compile_commands.json")
	if _, err := os.Stat(root); err == nil {
		out = append(out, root)
	}
	if matches, err := filepath.Glob(filepath.Join(repoRoot, "build*", "compile_commands.json")); err == nil {
		out = append(out, matches...)
	}
	return out
}

// extractIncludeDirs reconstructs the ordered include search path from a
// compile command, preferring the structured arguments array and falling back
// to a shlex split of the command string.
func extractIncludeDirs(cc compileCommand, repoRoot string) []string {
	toks := cc.Arguments
	if len(toks) == 0 && cc.Command != "" {
		toks = shlexSplit(cc.Command)
	}
	var dirs []string
	seen := map[string]bool{}
	add := func(raw string) {
		if rel := repoRelPath(repoRoot, cc.Directory, raw); rel != "" && !seen[rel] {
			seen[rel] = true
			dirs = append(dirs, rel)
		}
	}
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		switch {
		case strings.HasPrefix(t, "-I") && len(t) > 2:
			add(t[2:])
		case t == "-I" && i+1 < len(toks):
			i++
			add(toks[i])
		case (t == "-isystem" || t == "-iquote" || t == "-idirafter") && i+1 < len(toks):
			i++
			add(toks[i])
		}
	}
	return dirs
}

// heuristicIncludeDirs returns the conventional C/C++ include-root search path
// for a repo that has no compile_commands.json: the conventional roots
// (include / src / inc / api / lib) that actually exist, in priority order,
// followed by any other top-level directory that directly contains a C/C++
// header. Paths are repo-relative slash paths. Feeds the resolver's ordered
// include probe so collisions still break deterministically without a DB.
func heuristicIncludeDirs(repoRoot string) []string {
	if repoRoot == "" {
		return nil
	}
	var dirs []string
	seen := map[string]bool{}
	add := func(d string) {
		if d != "" && !seen[d] {
			seen[d] = true
			dirs = append(dirs, d)
		}
	}
	for _, name := range []string{"include", "src", "inc", "api", "lib"} {
		if fi, err := os.Stat(filepath.Join(repoRoot, name)); err == nil && fi.IsDir() {
			add(name)
		}
	}
	if entries, err := os.ReadDir(repoRoot); err == nil {
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			name := e.Name()
			if strings.HasPrefix(name, ".") {
				continue
			}
			if dirHasHeader(filepath.Join(repoRoot, name)) {
				add(name)
			}
		}
	}
	return dirs
}

// dirHasHeader reports whether dir directly contains a C/C++ header file.
func dirHasHeader(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		switch filepath.Ext(e.Name()) {
		case ".h", ".hpp", ".hh", ".hxx", ".h++":
			return true
		}
	}
	return false
}

// repoRelPath resolves p (relative to dir, or repoRoot when dir is empty)
// against the repo root, returning a clean repo-relative slash path — or ""
// when p resolves outside the repo (a toolchain / system path) or to the root
// itself.
func repoRelPath(repoRoot, dir, p string) string {
	if p == "" {
		return ""
	}
	abs := p
	if !filepath.IsAbs(abs) {
		base := dir
		if base == "" {
			base = repoRoot
		}
		abs = filepath.Join(base, p)
	}
	rel, err := filepath.Rel(repoRoot, filepath.Clean(abs))
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return ""
	}
	return filepath.ToSlash(rel)
}
