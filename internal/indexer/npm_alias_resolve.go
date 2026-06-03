package indexer

import (
	"encoding/json"
	"path"
	"strings"
	"sync"

	"github.com/zzet/gortex/internal/modules"
)

// npmAliasIndex implements resolver.NpmAliasResolver: it rewrites a
// JS/TS import specifier that resolves through an npm alias declared
// in the importing file's nearest-ancestor package.json.
//
// An npm alias declares a dependency under one name while pointing it
// at a different real package:
//
//	"dependencies": { "shared": "npm:@acme/shared-lib@1.4.0" }
//
// `import x from 'shared'` then refers to `@acme/shared-lib`, and
// `import x from 'shared/util'` to `@acme/shared-lib/util`. The
// resolver only knows the bare specifier, so without this rewrite a
// locally-vendored `@acme/shared-lib` is missed and the import falls
// through to an external stub.
//
// The index is read-only after construction apart from its parsed-
// manifest cache, which is guarded by mu because the resolver's
// resolveEdge workers run in parallel.
type npmAliasIndex struct {
	// roots maps a repo prefix to its on-disk root. Entries with an
	// empty prefix model single-repo mode (no prefix on graph paths).
	roots map[string]string

	mu sync.Mutex
	// aliasCache memoises one parsed package.json: disk path → the
	// alias map (dependency key → real package name) for that file.
	// A nil value records "read, but no npm-alias entries / missing
	// file" so a miss is not re-read on every import edge.
	aliasCache map[string]map[string]string
	// exportsCache memoises the parsed `exports` subpath map of one
	// package.json: disk path → packageExports. A nil value records
	// "read, but no usable `exports` field / missing file" so a miss
	// is not re-parsed on every import edge.
	exportsCache map[string]*packageExports
}

// packageExports is one package.json's parsed `exports` field: the
// declaring package's own `"name"` plus its subpath map flattened to
// subpath-key → target file path. The name lets the resolver confirm
// an import addresses this very package before consulting the map.
type packageExports struct {
	name string
	// subpaths maps an `exports` key (`"."`, `"./feature"`, `"./*"`)
	// to its resolved target file (`"./dist/feature.js"`), with the
	// leading `./` kept so the matcher can splice wildcard tails.
	subpaths map[string]string
}

// newNpmAliasIndex builds an index over the given repo roots. Returns
// nil when no usable root is supplied — callers treat a nil resolver
// as "no alias rewriting", which is the pre-feature behaviour.
func newNpmAliasIndex(roots map[string]string) *npmAliasIndex {
	usable := make(map[string]string, len(roots))
	for prefix, root := range roots {
		if root != "" {
			usable[prefix] = root
		}
	}
	if len(usable) == 0 {
		return nil
	}
	return &npmAliasIndex{
		roots:        usable,
		aliasCache:   map[string]map[string]string{},
		exportsCache: map[string]*packageExports{},
	}
}

// Resolve is the resolver.NpmAliasResolver entry point. callerFile is
// the importing file's graph path (repo-prefixed in multi-repo mode);
// specifier is the verbatim import specifier. It returns the specifier
// with its package portion swapped for the npm-alias real name, or ""
// when the specifier is not an npm alias for this importer.
func (x *npmAliasIndex) Resolve(callerFile, specifier string) string {
	if x == nil || callerFile == "" || specifier == "" {
		return ""
	}
	// Only JS/TS imports go through npm aliases. A relative or
	// absolute specifier is a path import, never a package name.
	if !isJSTSFile(callerFile) {
		return ""
	}
	if strings.HasPrefix(specifier, ".") || strings.HasPrefix(specifier, "/") {
		return ""
	}
	root, relDir, ok := x.locate(callerFile)
	if !ok {
		return ""
	}
	pkgName, subPath := splitPackageSpecifier(specifier)
	if pkgName == "" {
		return ""
	}
	// Walk from the importing file's directory up to the repo root,
	// stopping at the first package.json that declares the specifier
	// — npm resolution honours the nearest manifest.
	for dir := relDir; ; dir = path.Dir(dir) {
		manifest := joinPath(root, joinRel(dir, "package.json"))
		if real, found := x.aliasesFor(manifest)[pkgName]; found {
			if subPath == "" {
				return real
			}
			return real + "/" + subPath
		}
		// `exports` subpath map: when this manifest IS the imported
		// package (its `"name"` matches the specifier's package
		// portion), resolve the sub-path through the package's declared
		// `exports` entry points rather than treating it as a bare
		// directory import. `pkg/feature` → `pkg/dist/feature.js`.
		if mapped := x.exportTargetFor(manifest, pkgName, subPath); mapped != "" {
			return pkgName + "/" + mapped
		}
		if dir == "." || dir == "" || dir == "/" {
			return ""
		}
	}
}

// exportTargetFor resolves an import of `pkgName` (sub-path `subPath`,
// "" for the package root) through the `exports` field of the
// package.json at absPath, but only when that manifest declares
// `pkgName` as its own `"name"`. It returns the mapped target file
// relative to the package root with the leading `./` stripped (so the
// caller can splice it after `pkgName`), or "" when the manifest is a
// different package, declares no `exports`, or maps no matching
// sub-path.
func (x *npmAliasIndex) exportTargetFor(absPath, pkgName, subPath string) string {
	exp := x.exportsFor(absPath)
	if exp == nil || exp.name != pkgName {
		return ""
	}
	target := resolveExportsSubpath(exp.subpaths, subPath)
	return strings.TrimPrefix(target, "./")
}

// locate resolves callerFile to (repoRoot, repoRelativeDir). The
// longest matching prefix wins so nested repo roots resolve to the
// most specific one.
func (x *npmAliasIndex) locate(callerFile string) (root, relDir string, ok bool) {
	bestPrefix := ""
	bestRoot := ""
	for prefix, r := range x.roots {
		switch {
		case prefix == "":
			// Single-repo mode: graph paths carry no prefix.
			if bestRoot == "" {
				bestRoot = r
			}
		case callerFile == prefix || strings.HasPrefix(callerFile, prefix+"/"):
			if len(prefix) > len(bestPrefix) {
				bestPrefix = prefix
				bestRoot = r
			}
		}
	}
	if bestRoot == "" {
		return "", "", false
	}
	rel := callerFile
	if bestPrefix != "" {
		rel = strings.TrimPrefix(callerFile, bestPrefix+"/")
	}
	return bestRoot, path.Dir(rel), true
}

// aliasesFor returns the npm-alias map (dependency key → real package
// name) parsed from the package.json at absPath, reading and caching
// it on first request. The result is never nil-returned to callers as
// a map — a missing or alias-free manifest yields an empty map so the
// caller's lookup is a clean miss.
func (x *npmAliasIndex) aliasesFor(absPath string) map[string]string {
	x.mu.Lock()
	defer x.mu.Unlock()
	if cached, seen := x.aliasCache[absPath]; seen {
		return cached
	}
	var aliases map[string]string
	if src, ok := readDiskFile(absPath); ok {
		for _, spec := range modules.ParsePackageJSON(src) {
			if spec.Ecosystem != "npm" || spec.Alias == "" {
				continue
			}
			if aliases == nil {
				aliases = map[string]string{}
			}
			aliases[spec.Path] = spec.Alias
		}
	}
	x.aliasCache[absPath] = aliases
	return aliases
}

// exportsFor returns the parsed `exports` subpath map of the
// package.json at absPath, reading and caching it on first request. A
// nil result records "read, but no usable `exports` field / missing
// file" so a miss is not re-parsed on every import edge.
func (x *npmAliasIndex) exportsFor(absPath string) *packageExports {
	x.mu.Lock()
	defer x.mu.Unlock()
	if cached, seen := x.exportsCache[absPath]; seen {
		return cached
	}
	var exp *packageExports
	if src, ok := readDiskFile(absPath); ok {
		exp = parsePackageExports(src)
	}
	x.exportsCache[absPath] = exp
	return exp
}

// parsePackageExports parses the `name` and `exports` fields of a
// package.json. The `exports` field is the modern package entry-point
// map; this flattens it to subpath-key → target file, handling the two
// target shapes npm supports:
//
//	"./feature": "./dist/feature.js"                     (string)
//	"./feature": { "import": "...", "default": "..." }   (conditional)
//
// For a conditional object the `import` condition is preferred, then
// `default` — the order an ES-module consumer would resolve. A bare
// string `exports` (`"exports": "./index.js"`) is treated as the `"."`
// root entry. Returns nil when the manifest is unparseable or declares
// no usable `exports` map.
func parsePackageExports(source []byte) *packageExports {
	if len(source) == 0 {
		return nil
	}
	var manifest struct {
		Name    string          `json:"name"`
		Exports json.RawMessage `json:"exports"`
	}
	if err := json.Unmarshal(source, &manifest); err != nil {
		return nil
	}
	if len(manifest.Exports) == 0 {
		return nil
	}
	subpaths := map[string]string{}
	// A string `exports` is the package root entry point; an object is
	// the subpath map. Conditional objects keyed by condition (`import`
	// / `default`) at the top level also collapse to the `"."` root.
	var asString string
	if err := json.Unmarshal(manifest.Exports, &asString); err == nil {
		if t := strings.TrimSpace(asString); t != "" {
			subpaths["."] = t
		}
	} else {
		var asMap map[string]json.RawMessage
		if err := json.Unmarshal(manifest.Exports, &asMap); err != nil {
			return nil
		}
		// A top-level conditional object (no `"."` / `"./..."` keys, just
		// `import` / `default`) is the root entry point in disguise.
		if !hasSubpathKeys(asMap) {
			if t := pickConditionalTarget(manifest.Exports); t != "" {
				subpaths["."] = t
			}
		} else {
			for key, raw := range asMap {
				if t := pickConditionalTarget(raw); t != "" {
					subpaths[key] = t
				}
			}
		}
	}
	if len(subpaths) == 0 {
		return nil
	}
	return &packageExports{name: manifest.Name, subpaths: subpaths}
}

// hasSubpathKeys reports whether an `exports` object is a subpath map
// (keys begin with `.`) rather than a bare top-level conditional object
// (keys like `import` / `default`). An empty map is treated as a
// subpath map — there is nothing to collapse to the root.
func hasSubpathKeys(m map[string]json.RawMessage) bool {
	for key := range m {
		if strings.HasPrefix(key, ".") {
			return true
		}
	}
	return len(m) == 0
}

// pickConditionalTarget resolves one `exports` value to a target file
// string. A JSON string is returned verbatim; a conditional object
// (`{ "import": "...", "require": "...", "default": "..." }`) is
// resolved by preferring the `import` condition, then `default` — the
// resolution an ES-module consumer performs. Returns "" for any other
// shape (nested arrays, the `null` block-out sentinel, unknown
// conditions only).
func pickConditionalTarget(raw json.RawMessage) string {
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		return strings.TrimSpace(asString)
	}
	var conditions map[string]json.RawMessage
	if err := json.Unmarshal(raw, &conditions); err != nil {
		return ""
	}
	for _, cond := range []string{"import", "default"} {
		if sub, ok := conditions[cond]; ok {
			if t := pickConditionalTarget(sub); t != "" {
				return t
			}
		}
	}
	return ""
}

// resolveExportsSubpath matches an imported sub-path against a parsed
// `exports` subpath map and returns the mapped target file (leading
// `./` preserved), or "" for no match. The import key is the package
// root (`"."`) when subPath is empty, else `"./" + subPath`. Matching
// order mirrors Node: an exact key wins; otherwise the longest `./*`
// wildcard whose static prefix matches splices the captured tail into
// the target's own `*`. A sub-path containing a `..` segment escapes
// the package and is rejected outright, as Node's resolver does.
func resolveExportsSubpath(subpaths map[string]string, subPath string) string {
	if len(subpaths) == 0 || hasDotDotSegment(subPath) {
		return ""
	}
	key := "."
	if subPath != "" {
		key = "./" + subPath
	}
	if target, ok := subpaths[key]; ok {
		return target
	}
	bestPrefixLen := -1
	best := ""
	for pat, target := range subpaths {
		star := strings.IndexByte(pat, '*')
		if star < 0 {
			continue
		}
		prefix := pat[:star]
		suffix := pat[star+1:]
		if !strings.HasPrefix(key, prefix) || !strings.HasSuffix(key, suffix) {
			continue
		}
		if len(key) < len(prefix)+len(suffix) {
			continue
		}
		if len(prefix) <= bestPrefixLen {
			continue
		}
		captured := key[len(prefix) : len(key)-len(suffix)]
		best = strings.Replace(target, "*", captured, 1)
		bestPrefixLen = len(prefix)
	}
	return best
}

// hasDotDotSegment reports whether a slash-separated sub-path contains
// a `..` segment — a parent-directory escape an `exports` lookup must
// reject so a wildcard like `./*` can never resolve outside the
// package.
func hasDotDotSegment(subPath string) bool {
	for _, seg := range strings.Split(subPath, "/") {
		if seg == ".." {
			return true
		}
	}
	return false
}

// splitPackageSpecifier splits an import specifier into its package
// name and the sub-path within that package. A scoped package keeps
// its `@scope/name` as the package portion:
//
//	"shared"            → ("shared", "")
//	"shared/util"       → ("shared", "util")
//	"@acme/lib"         → ("@acme/lib", "")
//	"@acme/lib/util/x"  → ("@acme/lib", "util/x")
func splitPackageSpecifier(specifier string) (pkgName, subPath string) {
	parts := strings.SplitN(specifier, "/", 4)
	if strings.HasPrefix(specifier, "@") {
		// Scoped: the first two segments form the package name.
		if len(parts) < 2 || parts[0] == "@" || parts[1] == "" {
			return "", ""
		}
		pkgName = parts[0] + "/" + parts[1]
		subPath = strings.TrimPrefix(specifier, pkgName)
		return pkgName, strings.TrimPrefix(subPath, "/")
	}
	pkgName = parts[0]
	subPath = strings.TrimPrefix(specifier, pkgName)
	return pkgName, strings.TrimPrefix(subPath, "/")
}

// joinRel joins a repo-relative directory with a file name, treating
// the repo root (".") as no directory prefix.
func joinRel(dir, name string) string {
	if dir == "." || dir == "" {
		return name
	}
	return dir + "/" + name
}

// isJSTSFile reports whether filePath has a JavaScript/TypeScript
// extension — the only files whose imports resolve through npm.
func isJSTSFile(filePath string) bool {
	switch path.Ext(filePath) {
	case ".ts", ".tsx", ".js", ".jsx", ".mts", ".cts", ".mjs", ".cjs":
		return true
	}
	return false
}
