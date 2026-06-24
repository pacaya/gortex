package resolver

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// TestResolveRelativeImports_PythonStemMatchesModuleFile pins the
// "layout-aware resolution" branch of A22 for Python: a project-rooted
// stem like `app/util` lands on the existing `app/util.py` file node
// instead of staying as an `external::*` stub.
func TestResolveRelativeImports_PythonStemMatchesModuleFile(t *testing.T) {
	g := graph.New()
	seedFile(g, "app/main.py", "python")
	seedFile(g, "app/util.py", "python")
	e := seedExternalImport(g, "app/main.py", "app/util")

	r := New(g)
	r.resolveRelativeImports()

	assert.Equal(t, "app/util.py", e.To, "stem must resolve to module file ID")
}

// TestResolveRelativeImports_PythonStemMatchesPackageInit pins the
// fallback path: when `<stem>.py` doesn't exist but `<stem>/__init__.py`
// does, the edge lands on the package marker.
func TestResolveRelativeImports_PythonStemMatchesPackageInit(t *testing.T) {
	g := graph.New()
	seedFile(g, "app/main.py", "python")
	seedFile(g, "app/sub/__init__.py", "python")
	e := seedExternalImport(g, "app/main.py", "app/sub")

	r := New(g)
	r.resolveRelativeImports()

	assert.Equal(t, "app/sub/__init__.py", e.To)
}

// TestResolveRelativeImports_PythonStemUnresolvedStaysExternal — when no
// matching file exists, the edge stays external. attributeNonGoModule-
// Imports must then refuse to attribute it (no phantom pypi packages
// named after directory layout).
func TestResolveRelativeImports_PythonStemUnresolvedStaysExternal(t *testing.T) {
	g := graph.New()
	seedFile(g, "app/main.py", "python")
	e := seedExternalImport(g, "app/main.py", "app/missing")

	r := New(g)
	r.resolveRelativeImports()
	r.attributeNonGoModuleImports()

	assert.Equal(t, "external::app/missing", e.To, "unresolvable stem must stay external")
	assert.Nil(t, g.GetNode("module::pypi:app"), "must not invent a pypi package named after the dir")
}

func TestResolveRelativeImports_DartRelativeJoinsAgainstImportingDir(t *testing.T) {
	g := graph.New()
	seedFile(g, "lib/main.dart", "dart")
	seedFile(g, "lib/models/user.dart", "dart")
	e := seedExternalImport(g, "lib/main.dart", "models/user.dart")

	r := New(g)
	r.resolveRelativeImports()

	assert.Equal(t, "lib/models/user.dart", e.To)
}

func TestResolveRelativeImports_DartRelativeWithParentSegments(t *testing.T) {
	g := graph.New()
	seedFile(g, "lib/feature/main.dart", "dart")
	seedFile(g, "lib/shared/util.dart", "dart")
	e := seedExternalImport(g, "lib/feature/main.dart", "../shared/util.dart")

	r := New(g)
	r.resolveRelativeImports()

	assert.Equal(t, "lib/shared/util.dart", e.To)
}

func TestResolveRelativeImports_DartLeavesPackageURIsAlone(t *testing.T) {
	g := graph.New()
	seedFile(g, "lib/main.dart", "dart")
	e := seedExternalImport(g, "lib/main.dart", "package:flutter/material.dart")

	r := New(g)
	r.resolveRelativeImports()

	assert.Equal(t, "external::package:flutter/material.dart", e.To,
		"package: URIs are owned by the module-attribution pass, not this one")
}

func TestResolveRelativeImports_DartLeavesCoreLibraryAlone(t *testing.T) {
	g := graph.New()
	seedFile(g, "lib/main.dart", "dart")
	e := seedExternalImport(g, "lib/main.dart", "dart:async")

	r := New(g)
	r.resolveRelativeImports()

	assert.Equal(t, "external::dart:async", e.To,
		"dart: URIs are owned by the module-attribution pass, not this one")
}

// TestResolveRelativeImports_PythonAbsolutePathUnchanged guards the
// regression where the pass might over-attribute: an absolute Python
// module reference like `numpy.array` doesn't contain `/` and must
// flow through to attributeNonGoModuleImports unchanged.
func TestResolveRelativeImports_PythonAbsolutePathUnchanged(t *testing.T) {
	g := graph.New()
	seedFile(g, "app/main.py", "python")
	e := seedExternalImport(g, "app/main.py", "numpy.array")

	r := New(g)
	r.resolveRelativeImports()

	assert.Equal(t, "external::numpy.array", e.To)
}

func TestResolveRelativeImports_DartRelativeUnresolvedStaysExternal(t *testing.T) {
	g := graph.New()
	seedFile(g, "lib/main.dart", "dart")
	e := seedExternalImport(g, "lib/main.dart", "models/user.dart")

	r := New(g)
	r.resolveRelativeImports()

	assert.Equal(t, "external::models/user.dart", e.To,
		"with no target file in the graph, the edge stays external")
}

func TestResolveRelativeImports_DartEscapesRepoRoot(t *testing.T) {
	g := graph.New()
	seedFile(g, "main.dart", "dart")
	e := seedExternalImport(g, "main.dart", "../escape.dart")

	r := New(g)
	r.resolveRelativeImports()

	assert.Equal(t, "external::../escape.dart", e.To,
		"path walks above the repo root must not silently resolve")
}

func TestJoinRelativePath_VariousShapes(t *testing.T) {
	cases := []struct {
		dir  string
		rel  string
		want string
	}{
		{"lib", "models/user.dart", "lib/models/user.dart"},
		{"lib/feature", "../shared/util.dart", "lib/shared/util.dart"},
		{"lib", "./util.dart", "lib/util.dart"},
		{"lib/a/b", "../../c.dart", "lib/c.dart"},
		{"", "x.dart", "x.dart"},
		{"a", "../../escape.dart", ""},
		{"a/b/c", "../../../d.dart", "d.dart"},
		{"a/b/c", "../../../../d.dart", ""},
	}
	for _, c := range cases {
		got := joinRelativePath(c.dir, c.rel)
		require.Equal(t, c.want, got, "dir=%q rel=%q", c.dir, c.rel)
	}
}

// TestResolveRelativeImports_CQuotedInclude pins the C11 C-include resolution:
// a quoted `#include "bar.h"` (Meta include_kind=quoted) lands on the local
// header file node; a system include is left external.
func TestResolveRelativeImports_CQuotedInclude(t *testing.T) {
	g := graph.New()
	seedFile(g, "src/main.c", "c")
	seedFile(g, "src/bar.h", "c")

	quoted := &graph.Edge{
		From: "src/main.c", To: "unresolved::import::bar.h", Kind: graph.EdgeImports,
		Meta: map[string]any{"include_kind": "quoted"},
	}
	system := &graph.Edge{
		From: "src/main.c", To: "unresolved::import::stdio.h", Kind: graph.EdgeImports,
		Meta: map[string]any{"include_kind": "system"},
	}
	g.AddEdge(quoted)
	g.AddEdge(system)

	r := New(g)
	r.resolveRelativeImports()

	assert.Equal(t, "src/bar.h", quoted.To, "quoted include resolves to the local header")
	assert.Equal(t, "unresolved::import::stdio.h", system.To, "system include stays external")
}

// cInclude builds a quoted #include import edge for the C-include tests.
func cInclude(g graph.Store, from, rel string) *graph.Edge {
	e := &graph.Edge{
		From: from, To: "unresolved::import::" + rel, Kind: graph.EdgeImports,
		Meta: map[string]any{"include_kind": "quoted"},
	}
	g.AddEdge(e)
	return e
}

// TestResolveRelativeImports_CIncludeViaIncludeRoot pins the -I include-dir
// search: a multi-segment include `foo/bar.h` not relative to the including
// file still resolves to the uniquely-matching header reachable via an include
// root.
func TestResolveRelativeImports_CIncludeViaIncludeRoot(t *testing.T) {
	g := graph.New()
	seedFile(g, "src/main.c", "c")
	seedFile(g, "include/foo/bar.h", "c") // only reachable via `-Iinclude`
	e := cInclude(g, "src/main.c", "foo/bar.h")

	r := New(g)
	r.resolveRelativeImports()

	assert.Equal(t, "include/foo/bar.h", e.To, "non-relative include resolves via include root")
}

// TestResolveRelativeImports_CIncludeAmbiguousRefused pins that an include
// matching more than one root header is refused (no false edge).
func TestResolveRelativeImports_CIncludeAmbiguousRefused(t *testing.T) {
	g := graph.New()
	seedFile(g, "src/main.c", "c")
	seedFile(g, "a/foo/bar.h", "c")
	seedFile(g, "b/foo/bar.h", "c")
	e := cInclude(g, "src/main.c", "foo/bar.h")

	r := New(g)
	r.resolveRelativeImports()

	assert.Equal(t, "unresolved::import::foo/bar.h", e.To, "ambiguous include must stay unresolved")
}

// TestResolveRelativeImports_CIncludeBareHeaderNotProbed pins the multi-segment
// restriction: a single-segment `bar.h` not in the including dir is NOT
// resolved via the include-root search (too ambiguous), staying external.
func TestResolveRelativeImports_CIncludeBareHeaderNotProbed(t *testing.T) {
	g := graph.New()
	seedFile(g, "src/main.c", "c")
	seedFile(g, "vendor/bar.h", "c") // elsewhere, single-segment
	e := cInclude(g, "src/main.c", "bar.h")

	r := New(g)
	r.resolveRelativeImports()

	assert.Equal(t, "unresolved::import::bar.h", e.To, "bare header is not probed across roots")
}

// TestResolveRelativeImports_CIncludeViaCompileDB pins the compile_commands.json
// path: a generated header reachable only via a declared `-I` dir binds to that
// file and records the include dir it was found under.
func TestResolveRelativeImports_CIncludeViaCompileDB(t *testing.T) {
	g := graph.New()
	seedFile(g, "src/main.c", "c")
	seedFile(g, "gen/include/proj/api.h", "c") // generated, reachable via -Igen/include
	e := cInclude(g, "src/main.c", "proj/api.h")

	r := New(g)
	r.SetCppIncludeDirs(map[string][]string{"src/main.c": {"gen/include"}})
	r.resolveRelativeImports()

	assert.Equal(t, "gen/include/proj/api.h", e.To, "include binds via the declared -I dir")
	assert.Equal(t, "gen/include", e.Meta["include_dir"])
	assert.Equal(t, "compile_db", e.Meta["resolved_via"])
}

// TestResolveRelativeImports_CIncludeCollisionFirstIncludeWins pins that a
// basename collision the suffix search would refuse is broken deterministically
// by the translation unit's `-I` order (authoritative): the first declared dir
// containing the header wins.
func TestResolveRelativeImports_CIncludeCollisionFirstIncludeWins(t *testing.T) {
	g := graph.New()
	seedFile(g, "src/app.c", "c")
	seedFile(g, "inc1/config.h", "c")
	seedFile(g, "inc2/config.h", "c")
	e := cInclude(g, "src/app.c", "config.h")

	r := New(g)
	r.SetCppIncludeDirs(map[string][]string{"src/app.c": {"inc1", "inc2"}})
	r.resolveRelativeImports()

	assert.Equal(t, "inc1/config.h", e.To, "first -I dir wins the basename collision")
	assert.Equal(t, "compile_db", e.Meta["resolved_via"])
}

// TestResolveRelativeImports_CIncludeFallbackToSuffix pins that when the
// importing TU is absent from compile_commands.json the resolver falls back to
// the suffix-unique behavior — resolving without stamping a compile-DB origin.
func TestResolveRelativeImports_CIncludeFallbackToSuffix(t *testing.T) {
	g := graph.New()
	seedFile(g, "src/main.c", "c")
	seedFile(g, "gen/include/proj/api.h", "c")
	e := cInclude(g, "src/main.c", "proj/api.h")

	r := New(g)
	// compile_commands.json present, but this source has no TU entry of its own;
	// the union dir does not contain the header → suffix-unique fallback resolves.
	r.SetCppIncludeDirs(map[string][]string{"other/x.c": {"third/inc"}})
	r.resolveRelativeImports()

	assert.Equal(t, "gen/include/proj/api.h", e.To, "falls back to suffix-unique match")
	assert.Nil(t, e.Meta["resolved_via"], "suffix fallback is not stamped compile_db")
}

// TestResolveRelativeImports_CIncludeStdlibAngleGuard pins the basename-collision
// guard: a standard-library angle include never binds to an in-tree file that
// shares its basename, even when a declared -I dir would otherwise resolve it.
func TestResolveRelativeImports_CIncludeStdlibAngleGuard(t *testing.T) {
	g := graph.New()
	seedFile(g, "src/main.cpp", "cpp")
	seedFile(g, "lib/vector", "cpp") // in-tree file whose basename collides with <vector>
	e := &graph.Edge{
		From: "src/main.cpp", To: "unresolved::import::vector", Kind: graph.EdgeImports,
		Meta: map[string]any{"include_kind": "system"},
	}
	g.AddEdge(e)

	r := New(g)
	// -Ilib would bind lib/vector were it not for the stdlib guard.
	r.SetCppIncludeDirs(map[string][]string{"src/main.cpp": {"lib"}})
	r.resolveRelativeImports()

	assert.Equal(t, "unresolved::import::vector", e.To, "stdlib <vector> must not bind to in-tree lib/vector")
}

// TestResolveRelativeImports_CIncludeSystemNonStdlibResolves pins that a
// non-stdlib angle include now resolves through the -I search path.
func TestResolveRelativeImports_CIncludeSystemNonStdlibResolves(t *testing.T) {
	g := graph.New()
	seedFile(g, "src/main.c", "c")
	seedFile(g, "include/proj/api.h", "c")
	e := &graph.Edge{
		From: "src/main.c", To: "unresolved::import::proj/api.h", Kind: graph.EdgeImports,
		Meta: map[string]any{"include_kind": "system"},
	}
	g.AddEdge(e)

	r := New(g)
	r.SetCppIncludeDirs(map[string][]string{"src/main.c": {"include"}})
	r.resolveRelativeImports()

	assert.Equal(t, "include/proj/api.h", e.To, "non-stdlib angle include resolves via -I dir")
	assert.Equal(t, "include", e.Meta["include_dir"])
}

// TestResolveRelativeImports_CIncludeViaHeuristicDir pins the no-compile-DB
// fallback: an angle include resolves through the heuristic include root and is
// stamped with its provenance.
func TestResolveRelativeImports_CIncludeViaHeuristicDir(t *testing.T) {
	g := graph.New()
	seedFile(g, "src/main.c", "c")
	seedFile(g, "include/proj/api.h", "c")
	e := &graph.Edge{
		From: "src/main.c", To: "unresolved::import::proj/api.h", Kind: graph.EdgeImports,
		Meta: map[string]any{"include_kind": "system"},
	}
	g.AddEdge(e)

	r := New(g)
	// No compile DB; the heuristic include/ root drives the ordered probe.
	r.SetCppFallbackIncludeDirs([]string{"include"})
	r.resolveRelativeImports()

	assert.Equal(t, "include/proj/api.h", e.To, "angle include resolves via the heuristic include root")
	assert.Equal(t, "include", e.Meta["include_dir"])
	assert.Equal(t, "heuristic", e.Meta["resolved_via"])
}

// TestResolveRelativeImports_CIncludeAngleSuffixFallback pins that with no
// compile DB and no heuristic dirs, a single-match multi-segment angle include
// still resolves through the suffix-unique net, unstamped.
func TestResolveRelativeImports_CIncludeAngleSuffixFallback(t *testing.T) {
	g := graph.New()
	seedFile(g, "src/main.c", "c")
	seedFile(g, "weird/place/widget.h", "c") // non-conventional dir, single match
	e := &graph.Edge{
		From: "src/main.c", To: "unresolved::import::place/widget.h", Kind: graph.EdgeImports,
		Meta: map[string]any{"include_kind": "system"},
	}
	g.AddEdge(e)

	r := New(g)
	r.resolveRelativeImports()

	assert.Equal(t, "weird/place/widget.h", e.To, "single-match header resolves via suffix-unique net")
	assert.Nil(t, e.Meta["resolved_via"], "suffix fallback is not stamped")
}
