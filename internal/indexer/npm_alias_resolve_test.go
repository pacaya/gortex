package indexer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/resolver"
)

// writePackageJSON writes a package.json into dir, creating any
// missing parent directories. Returns dir for convenient chaining.
func writePackageJSON(t *testing.T, dir, body string) string {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "package.json"), []byte(body), 0o644))
	return dir
}

func TestSplitPackageSpecifier(t *testing.T) {
	cases := []struct {
		in           string
		pkg, subPath string
	}{
		{"shared", "shared", ""},
		{"shared/util", "shared", "util"},
		{"shared/util/deep", "shared", "util/deep"},
		{"@acme/lib", "@acme/lib", ""},
		{"@acme/lib/util", "@acme/lib", "util"},
		{"@acme/lib/util/deep", "@acme/lib", "util/deep"},
		{"@bad", "", ""}, // scope with no name
	}
	for _, c := range cases {
		pkg, sub := splitPackageSpecifier(c.in)
		if pkg != c.pkg || sub != c.subPath {
			t.Errorf("splitPackageSpecifier(%q) = (%q, %q), want (%q, %q)",
				c.in, pkg, sub, c.pkg, c.subPath)
		}
	}
}

// TestNpmAliasIndex_Resolve drives the disk-backed resolver over a
// real monorepo layout: a workspace-root package.json plus a nested
// per-package one, both holding npm-alias dependency entries.
func TestNpmAliasIndex_Resolve(t *testing.T) {
	root := t.TempDir()

	// Workspace-root package.json — declares one alias.
	writePackageJSON(t, root, `{
  "name": "monorepo-root",
  "dependencies": {
    "rootdep": "npm:@acme/root-lib@2.0.0"
  }
}`)
	// Nested package package.json — the nearest-ancestor manifest for
	// files under packages/app. Covers scoped+version, plain+version,
	// no-version, a dev-dependency alias, and an ordinary dep.
	writePackageJSON(t, filepath.Join(root, "packages", "app"), `{
  "name": "@acme/app",
  "dependencies": {
    "shared": "npm:@acme/shared-lib@1.4.0",
    "lodash4": "npm:lodash@4.17.21",
    "nover": "npm:@acme/no-version",
    "react": "^18.2.0"
  },
  "devDependencies": {
    "test-utils": "npm:@acme/test-utils@0.1.0"
  }
}`)

	idx := newNpmAliasIndex(map[string]string{"": root})
	require.NotNil(t, idx)

	caller := "packages/app/src/components/Widget.ts"
	cases := []struct {
		name      string
		callerRel string
		specifier string
		want      string
	}{
		{"scoped alias with version", caller, "shared", "@acme/shared-lib"},
		{"plain alias with version", caller, "lodash4", "lodash"},
		{"alias without version", caller, "nover", "@acme/no-version"},
		{"alias sub-path import", caller, "shared/util", "@acme/shared-lib/util"},
		{"alias deep sub-path", caller, "shared/util/deep", "@acme/shared-lib/util/deep"},
		{"dev-dependency alias", caller, "test-utils", "@acme/test-utils"},
		{"ordinary dep is not rewritten", caller, "react", ""},
		{"unknown specifier is not rewritten", caller, "express", ""},
		{"relative import is never an alias", caller, "./local", ""},
		{"non-JS/TS caller is skipped", "packages/app/main.go", "shared", ""},
		// A file directly under the workspace root sees only the root
		// manifest — `shared` belongs to the nested package, not here.
		{"nearest-ancestor scoping (root file)", "index.ts", "shared", ""},
		{"root manifest alias resolves at root", "index.ts", "rootdep", "@acme/root-lib"},
		// The nested file falls through to the root manifest for an
		// alias the nested package.json does not declare.
		{"ancestor walk reaches the root manifest", caller, "rootdep", "@acme/root-lib"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := idx.Resolve(c.callerRel, c.specifier)
			assert.Equal(t, c.want, got)
		})
	}
}

func TestNpmAliasIndex_NilRootsYieldsNil(t *testing.T) {
	assert.Nil(t, newNpmAliasIndex(nil))
	assert.Nil(t, newNpmAliasIndex(map[string]string{"repo": ""}))
}

// TestParsePackageExports covers every `exports` shape the parser
// recognises: string subpath targets, conditional-object targets
// (`import` preferred over `default`), the `"."` root key, a bare
// string `exports` collapsing to the root, a top-level conditional
// object collapsing to the root, and a missing / non-`exports`
// manifest yielding nil.
func TestParsePackageExports(t *testing.T) {
	t.Run("subpath string and conditional targets", func(t *testing.T) {
		exp := parsePackageExports([]byte(`{
  "name": "@acme/pkg",
  "exports": {
    ".": "./dist/index.js",
    "./feature": "./dist/feature.js",
    "./util": { "import": "./dist/util.mjs", "require": "./dist/util.cjs", "default": "./dist/util.js" },
    "./only-default": { "default": "./dist/only.js" }
  }
}`))
		require.NotNil(t, exp)
		assert.Equal(t, "@acme/pkg", exp.name)
		assert.Equal(t, "./dist/index.js", exp.subpaths["."])
		assert.Equal(t, "./dist/feature.js", exp.subpaths["./feature"])
		// `import` wins over `require` / `default`.
		assert.Equal(t, "./dist/util.mjs", exp.subpaths["./util"])
		// Falls back to `default` when `import` is absent.
		assert.Equal(t, "./dist/only.js", exp.subpaths["./only-default"])
	})

	t.Run("bare string exports collapses to root", func(t *testing.T) {
		exp := parsePackageExports([]byte(`{"name":"pkg","exports":"./index.js"}`))
		require.NotNil(t, exp)
		assert.Equal(t, "./index.js", exp.subpaths["."])
	})

	t.Run("top-level conditional object collapses to root", func(t *testing.T) {
		exp := parsePackageExports([]byte(`{"name":"pkg","exports":{"import":"./esm.js","default":"./cjs.js"}}`))
		require.NotNil(t, exp)
		assert.Equal(t, "./esm.js", exp.subpaths["."])
	})

	t.Run("no exports field yields nil", func(t *testing.T) {
		assert.Nil(t, parsePackageExports([]byte(`{"name":"pkg","main":"./index.js"}`)))
		assert.Nil(t, parsePackageExports(nil))
		assert.Nil(t, parsePackageExports([]byte(`not json`)))
	})
}

// TestResolveExportsSubpath checks the subpath matcher: an exact key
// wins, an empty sub-path resolves the `"."` root, and a `./*`
// wildcard splices the captured tail into the target's own `*`. The
// longest static prefix wins when several wildcards could match.
func TestResolveExportsSubpath(t *testing.T) {
	subpaths := map[string]string{
		".":              "./dist/index.js",
		"./feature":      "./dist/feature.js",
		"./components/*": "./dist/components/*.js",
		"./*":            "./dist/*.js",
	}
	cases := []struct {
		name, subPath, want string
	}{
		{"root", "", "./dist/index.js"},
		{"exact subpath", "feature", "./dist/feature.js"},
		{"wildcard tail", "math", "./dist/math.js"},
		{"longest-prefix wildcard wins", "components/Button", "./dist/components/Button.js"},
		{"no match", "../escape", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, resolveExportsSubpath(subpaths, c.subPath))
		})
	}
	assert.Equal(t, "", resolveExportsSubpath(nil, "feature"))
}

// TestNpmAliasIndex_ResolveExports drives the disk-backed resolver over
// a package whose package.json declares an `exports` map: an import of
// the package by name with a sub-path resolves through `exports` to the
// mapped target file, the `"."` root resolves a bare package import,
// and a `./*` wildcard splices the tail. A subpath the map does not
// declare falls through (returns ""), and a package WITHOUT an
// `exports` field is left untouched (no regression to bare-directory
// resolution).
func TestNpmAliasIndex_ResolveExports(t *testing.T) {
	root := t.TempDir()
	// The package itself lives at packages/ui; its files import it by
	// its published name `@acme/ui`.
	writePackageJSON(t, filepath.Join(root, "packages", "ui"), `{
  "name": "@acme/ui",
  "exports": {
    ".": "./dist/index.js",
    "./feature": "./dist/feature.js",
    "./button": { "import": "./dist/button.mjs", "default": "./dist/button.js" },
    "./components/*": "./dist/components/*.js"
  }
}`)
	// A sibling package with no `exports` map — imports of it must not
	// be rewritten through exports resolution.
	writePackageJSON(t, filepath.Join(root, "packages", "plain"), `{
  "name": "@acme/plain"
}`)

	idx := newNpmAliasIndex(map[string]string{"": root})
	require.NotNil(t, idx)

	caller := "packages/ui/src/widget.ts"
	cases := []struct {
		name      string
		callerRel string
		specifier string
		want      string
	}{
		{"subpath maps to target file", caller, "@acme/ui/feature", "@acme/ui/dist/feature.js"},
		{"conditional subpath prefers import", caller, "@acme/ui/button", "@acme/ui/dist/button.mjs"},
		{"wildcard subpath splices the tail", caller, "@acme/ui/components/Card", "@acme/ui/dist/components/Card.js"},
		{"root entry resolves a bare package import", caller, "@acme/ui", "@acme/ui/dist/index.js"},
		{"undeclared subpath falls through", caller, "@acme/ui/missing", ""},
		{"package without exports is untouched", "packages/plain/src/x.ts", "@acme/plain/sub", ""},
		{"third-party package is untouched", caller, "react/jsx-runtime", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, idx.Resolve(c.callerRel, c.specifier))
		})
	}
}

// addPackageNode registers a KindPackage node with the given qualified
// name — this is what CrossRepoResolver.resolveImport matches an
// import path against (mirrors the existing cross-repo import tests).
func addPackageNode(g graph.Store, repo, file, qualName string) {
	g.AddNode(&graph.Node{
		ID: file, Kind: graph.KindPackage, Name: qualName, QualName: qualName,
		FilePath: file, Language: "typescript", RepoPrefix: repo,
	})
}

// TestNpmAliasImportResolution is the end-to-end check: an import edge
// whose specifier is an npm-alias key resolves to the locally-indexed
// real package, including the sub-path and no-version forms; an alias
// whose real package is NOT indexed still resolves to an external
// stub (no regression).
func TestNpmAliasImportResolution(t *testing.T) {
	root := t.TempDir()
	writePackageJSON(t, filepath.Join(root, "packages", "app"), `{
  "name": "@acme/app",
  "dependencies": {
    "shared": "npm:@acme/shared-lib@1.4.0",
    "nover": "npm:@acme/no-version",
    "missing": "npm:@acme/not-indexed@9.9.9"
  }
}`)

	aliasIdx := newNpmAliasIndex(map[string]string{"": root})
	require.NotNil(t, aliasIdx)

	cases := []struct {
		name      string
		specifier string
		wantTo    string
		wantStats func(t *testing.T, s *resolver.CrossRepoStats)
	}{
		{
			name:      "alias key resolves to the locally-indexed real package",
			specifier: "shared",
			wantTo:    "packages/shared-lib/src/index.ts",
			wantStats: func(t *testing.T, s *resolver.CrossRepoStats) {
				assert.Equal(t, 1, s.Resolved)
			},
		},
		{
			name:      "alias sub-path import resolves to the real package",
			specifier: "shared/util",
			wantTo:    "packages/shared-lib/src/index.ts",
			wantStats: func(t *testing.T, s *resolver.CrossRepoStats) {
				assert.Equal(t, 1, s.Resolved)
			},
		},
		{
			name:      "alias without a version resolves to the real package",
			specifier: "nover",
			wantTo:    "packages/no-version/src/index.ts",
			wantStats: func(t *testing.T, s *resolver.CrossRepoStats) {
				assert.Equal(t, 1, s.Resolved)
			},
		},
		{
			// Negative case: the alias real package is not indexed —
			// resolution falls through to an external stub exactly as a
			// plain unindexed import would. No regression.
			name:      "alias whose real package is not indexed stays external",
			specifier: "missing",
			wantTo:    "external::@acme/not-indexed",
			wantStats: func(t *testing.T, s *resolver.CrossRepoStats) {
				assert.Equal(t, 0, s.Resolved)
				assert.Equal(t, 1, s.Unresolved)
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g := graph.New()
			caller := "packages/app/src/main.ts"
			g.AddNode(&graph.Node{
				ID: caller, Kind: graph.KindFile, Name: "main.ts",
				FilePath: caller, Language: "typescript",
			})
			// Locally-vendored real packages, keyed by their npm name.
			addPackageNode(g, "", "packages/shared-lib/src/index.ts", "@acme/shared-lib")
			addPackageNode(g, "", "packages/no-version/src/index.ts", "@acme/no-version")

			edge := &graph.Edge{
				From: caller, To: "unresolved::import::" + c.specifier,
				Kind: graph.EdgeImports, FilePath: caller, Line: 1,
			}
			g.AddEdge(edge)

			cr := resolver.NewCrossRepo(g)
			cr.SetNpmAliasResolver(aliasIdx.Resolve)
			stats := cr.ResolveAll()

			assert.Equal(t, c.wantTo, edge.To)
			c.wantStats(t, stats)
		})
	}
}

// TestNpmAliasImportResolution_NoResolverIsExternal pins the no-regression
// baseline: without the alias resolver installed, an import of an alias
// key resolves to an external stub under the bare specifier — exactly
// the pre-feature behaviour.
func TestNpmAliasImportResolution_NoResolverIsExternal(t *testing.T) {
	g := graph.New()
	caller := "packages/app/src/main.ts"
	g.AddNode(&graph.Node{
		ID: caller, Kind: graph.KindFile, Name: "main.ts",
		FilePath: caller, Language: "typescript",
	})
	addPackageNode(g, "", "packages/shared-lib/src/index.ts", "@acme/shared-lib")

	edge := &graph.Edge{
		From: caller, To: "unresolved::import::shared",
		Kind: graph.EdgeImports, FilePath: caller, Line: 1,
	}
	g.AddEdge(edge)

	cr := resolver.NewCrossRepo(g) // no SetNpmAliasResolver
	cr.ResolveAll()

	require.True(t, strings.HasPrefix(edge.To, "external::"),
		"without the alias resolver the bare specifier must stay external, got %q", edge.To)
	assert.Equal(t, "external::shared", edge.To)
}
