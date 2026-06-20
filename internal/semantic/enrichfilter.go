package semantic

import (
	"path"
	"path/filepath"
	"strings"
)

// vendoredSegments are path segments whose contents are third-party /
// dependency code that is low value to enrich semantically.
var vendoredSegments = []string{"/vendor/", "/third_party/", "/node_modules/", "/.git/"}

// generatedSuffixes are filename suffixes for machine-generated sources.
var generatedSuffixes = []string{".pb.go", ".pb.cc", ".pb.h", ".pb.py", "_pb2.py", "_generated.go", ".gen.go"}

// IsLowValueForEnrichment reports whether a file should be skipped for
// semantic enrichment because it is machine-generated or vendored. Such
// files — vendored dependencies, and especially huge generated parsers like
// tree-sitter's src/parser.c plus the tree_sitter runtime headers — are low
// value to enrich and dominate language-server background-indexing time
// (clangd indexing a generated parser.c is by far the slowest part of a
// fresh index). Detection is a built-in generated/vendored heuristic plus
// any user-configured exclude globs.
func IsLowValueForEnrichment(filePath string, userGlobs []string) bool {
	if filePath == "" {
		return false
	}
	p := filepath.ToSlash(filePath)
	lower := strings.ToLower(p)

	for _, seg := range vendoredSegments {
		if strings.Contains(lower, seg) {
			return true
		}
	}
	// Tree-sitter runtime headers (src/tree_sitter/{alloc,array,parser}.h) and
	// generated grammar parsers/scanners.
	if strings.Contains(lower, "/tree_sitter/") || strings.HasPrefix(lower, "tree_sitter/") {
		return true
	}
	switch strings.ToLower(path.Base(p)) {
	case "parser.c", "parser.h", "scanner.c", "scanner.cc", "grammar.json":
		return true
	}
	for _, suf := range generatedSuffixes {
		if strings.HasSuffix(lower, suf) {
			return true
		}
	}
	for _, g := range userGlobs {
		if matchExcludeGlob(g, p) {
			return true
		}
	}
	return false
}

// matchExcludeGlob matches a user-supplied glob against a slash path.
// Supports `*` / `?` / `[...]` (within a segment, via path.Match) and `**`
// (across segments). A pattern with no glob metacharacters matches as a
// path substring or an exact basename.
func matchExcludeGlob(pattern, p string) bool {
	pattern = filepath.ToSlash(pattern)
	if pattern == "" {
		return false
	}
	if strings.Contains(pattern, "**") {
		idx := 0
		for _, part := range strings.Split(pattern, "**") {
			part = strings.Trim(part, "/")
			if part == "" {
				continue
			}
			j := strings.Index(p[idx:], part)
			if j < 0 {
				return false
			}
			idx += j + len(part)
		}
		return true
	}
	if !strings.ContainsAny(pattern, "*?[") {
		return strings.Contains(p, pattern) || path.Base(p) == pattern
	}
	if ok, _ := path.Match(pattern, p); ok {
		return true
	}
	ok, _ := path.Match(pattern, path.Base(p))
	return ok
}
