package indexer

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// writeFileMk writes content to path, creating parent directories first.
func writeFileMk(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	writeFile(t, path, content)
}

// callTargetForName returns the EdgeCalls target of the named function in
// the given file, or "" when the call edge is unresolved / absent.
func callTargetForName(t *testing.T, g graph.Store, file, name string) string {
	t.Helper()
	id := fnNodeID(t, g, file, name)
	for _, e := range g.GetOutEdges(id) {
		if e.Kind == graph.EdgeCalls {
			return e.To
		}
	}
	return ""
}

// TestTSConfigPathAlias_CallGraph reproduces issue #136: a call to a
// function imported through a tsconfig `paths` alias (`@/lib/auth`) must
// bind the same as the relative-import callers. Same-directory relative
// callers resolve trivially (same package); cross-directory relative
// callers resolve through the post-resolution import closure; the alias
// caller must resolve through the SAME closure once the alias specifier
// is expanded to its real file.
func TestTSConfigPathAlias_CallGraph(t *testing.T) {
	dir := t.TempDir()
	writeFileMk(t, filepath.Join(dir, "tsconfig.json"),
		"{\n  \"compilerOptions\": {\n    \"baseUrl\": \".\",\n    \"paths\": { \"@/*\": [\"src/*\"] }\n  }\n}\n")
	writeFileMk(t, filepath.Join(dir, "src", "lib", "auth.ts"),
		"export function getAuth() { return null }\n")
	writeFileMk(t, filepath.Join(dir, "src", "lib", "mid.ts"),
		"import { getAuth } from './auth'\nexport function viaRelativeSameDir() { return getAuth() }\n")
	writeFileMk(t, filepath.Join(dir, "src", "app", "cross.ts"),
		"import { getAuth } from '../lib/auth'\nexport function viaRelativeCrossDir() { return getAuth() }\n")
	writeFileMk(t, filepath.Join(dir, "src", "app", "route.ts"),
		"import { getAuth } from '@/lib/auth'\nexport function viaAlias() { return getAuth() }\n")

	idx, _ := newSQLiteIndexer(t)
	_, err := idx.Index(dir)
	require.NoError(t, err)
	idx.ResolveAll()

	g := idx.Graph()
	const want = "src/lib/auth.ts::getAuth"

	require.Equal(t, want, callTargetForName(t, g, "src/lib/mid.ts", "viaRelativeSameDir"),
		"same-dir relative import must bind")
	require.Equal(t, want, callTargetForName(t, g, "src/app/cross.ts", "viaRelativeCrossDir"),
		"cross-dir relative import must bind through the import closure")
	require.Equal(t, want, callTargetForName(t, g, "src/app/route.ts", "viaAlias"),
		"tsconfig paths alias import must bind the same as a relative import (issue #136)")

	// The per-binding import edges must also land on the symbol so
	// find_usages / get_callers can answer "who imports getAuth". All
	// three importers should now appear as incoming edges.
	importerFiles := map[string]bool{}
	for _, e := range g.GetInEdges(want) {
		if e.Kind == graph.EdgeImports {
			importerFiles[e.From] = true
		}
	}
	for _, f := range []string{"src/lib/mid.ts", "src/app/cross.ts", "src/app/route.ts"} {
		require.True(t, importerFiles[f],
			"per-binding import edge from %s must resolve onto getAuth (find_usages)", f)
	}
}

// TestTSConfigPathAlias_Incremental proves the alias binding survives an
// incremental single-file reindex — the resolution runs inside
// resolveImport, so the per-file ResolveFile path lands it the same as a
// cold ResolveAll.
func TestTSConfigPathAlias_Incremental(t *testing.T) {
	dir := t.TempDir()
	writeFileMk(t, filepath.Join(dir, "tsconfig.json"),
		"{\n  \"compilerOptions\": {\n    \"baseUrl\": \".\",\n    \"paths\": { \"@/*\": [\"src/*\"] }\n  }\n}\n")
	writeFileMk(t, filepath.Join(dir, "src", "lib", "auth.ts"),
		"export function getAuth() { return null }\n")
	routePath := filepath.Join(dir, "src", "app", "route.ts")
	writeFileMk(t, routePath,
		"import { getAuth } from '@/lib/auth'\nexport function viaAlias() { return getAuth() }\n")

	idx, _ := newSQLiteIndexer(t)
	_, err := idx.Index(dir)
	require.NoError(t, err)
	idx.ResolveAll()

	g := idx.Graph()
	const want = "src/lib/auth.ts::getAuth"
	require.Equal(t, want, callTargetForName(t, g, "src/app/route.ts", "viaAlias"),
		"baseline: alias import must bind after cold index")

	// Re-edit route.ts (add a second alias call) and reindex just that file.
	bumpMtime(t, routePath,
		"import { getAuth } from '@/lib/auth'\nexport function viaAlias() { getAuth(); return getAuth() }\n")
	_, err = idx.IncrementalReindexPaths(dir, []string{routePath})
	require.NoError(t, err)

	require.Equal(t, want, callTargetForName(t, g, "src/app/route.ts", "viaAlias"),
		"alias import must stay bound after an incremental single-file reindex")
}

// TestTSConfigPathAlias_BaseURLBarrelAndExact exercises the remaining
// tsconfig shapes: a wildcard alias whose targets are rooted at a non-"."
// baseUrl and resolve to a directory barrel (`index.ts`), and a
// non-wildcard exact alias.
func TestTSConfigPathAlias_BaseURLBarrelAndExact(t *testing.T) {
	dir := t.TempDir()
	writeFileMk(t, filepath.Join(dir, "tsconfig.json"),
		"{\n  \"compilerOptions\": {\n    \"baseUrl\": \"src\",\n    \"paths\": {\n"+
			"      \"@/*\": [\"*\"],\n      \"@config\": [\"lib/config/index.ts\"]\n    }\n  }\n}\n")
	// Directory barrel reached through `@/lib/api` -> src/lib/api/index.ts.
	writeFileMk(t, filepath.Join(dir, "src", "lib", "api", "index.ts"),
		"export function callApi() { return 1 }\n")
	// Exact (non-wildcard) alias `@config` -> src/lib/config/index.ts.
	writeFileMk(t, filepath.Join(dir, "src", "lib", "config", "index.ts"),
		"export function loadConfig() { return 2 }\n")
	writeFileMk(t, filepath.Join(dir, "src", "app", "page.ts"),
		"import { callApi } from '@/lib/api'\n"+
			"import { loadConfig } from '@config'\n"+
			"export function useBarrel() { return callApi() }\n"+
			"export function useExact() { return loadConfig() }\n")

	idx, _ := newSQLiteIndexer(t)
	_, err := idx.Index(dir)
	require.NoError(t, err)
	idx.ResolveAll()

	g := idx.Graph()
	require.Equal(t, "src/lib/api/index.ts::callApi",
		callTargetForName(t, g, "src/app/page.ts", "useBarrel"),
		"wildcard alias rooted at baseUrl must resolve to the directory index barrel")
	require.Equal(t, "src/lib/config/index.ts::loadConfig",
		callTargetForName(t, g, "src/app/page.ts", "useExact"),
		"non-wildcard exact alias must resolve")
}
