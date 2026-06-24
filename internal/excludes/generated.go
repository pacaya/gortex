package excludes

import (
	"path"
	"strings"
)

// generatedSuffixes lists file-name suffixes that mark generated code
// across the languages Gortex indexes. Kept here (a leaf package) so
// both the MCP response-envelope notes and the search rerank pipeline
// share one source of truth without an import cycle.
var generatedSuffixes = []string{
	// Protobuf / gRPC stubs.
	".pb.go", ".pb.cc", ".pb.h", ".pb.swift",
	"_pb2.py", "_pb2_grpc.py", "_pb2.pyi",
	"_pb.ts", "_pb.js", "_grpc_pb.ts", "_grpc_pb.js",
	// Go generators.
	"_gen.go", ".gen.go", "_generated.go", ".generated.go",
	// TS / JS generators.
	".generated.ts", ".generated.tsx", ".generated.js", ".generated.jsx",
	".gen.ts", ".gen.tsx", ".gen.js", ".gen.jsx",
	// Rust.
	".generated.rs",
	// Dart generators (build_runner, freezed, protobuf, chopper, auto_route, …).
	".g.dart", ".freezed.dart", ".pb.dart", ".pbgrpc.dart", ".pbenum.dart",
	".pbjson.dart", ".pbserver.dart", ".chopper.dart", ".config.dart", ".gr.dart",
	// C#.
	".g.cs", ".designer.cs",
}

// IsGenerated reports whether a file name matches a common
// code-generation convention — protobuf stubs, *_gen.go, mocks,
// Kubernetes zz_generated deepcopy, Dart/C# generators, and friends.
// Edits to such files are overwritten by their generator, so callers
// (omission notes, retrieval ranking) treat them as second-class.
func IsGenerated(p string) bool {
	if p == "" {
		return false
	}
	base := strings.ToLower(path.Base(filepathToSlash(p)))
	for _, suf := range generatedSuffixes {
		if strings.HasSuffix(base, suf) {
			return true
		}
	}
	if strings.HasPrefix(base, "zz_generated") {
		return true
	}
	if strings.HasSuffix(base, ".go") &&
		(strings.HasPrefix(base, "mock_") || strings.HasSuffix(base, "_mock.go") ||
			strings.HasSuffix(base, "_mocks.go") || strings.HasSuffix(base, ".pulsar.go")) {
		return true
	}
	// Java protobuf / gRPC generated classes: FooOuterClass.java, FooGrpc.java.
	if strings.HasSuffix(base, "grpc.java") || strings.HasSuffix(base, "outerclass.java") {
		return true
	}
	// Minified bundles: not generated-from-source, but "don't edit, don't rank
	// highly" the same way — codegraph groups them here.
	if strings.HasSuffix(base, ".min.js") || strings.HasSuffix(base, ".min.mjs") {
		return true
	}
	return false
}

// GeneratedPeerPaths returns the plausible hand-written peer file
// paths a generated file shadows — the "same-named implementation"
// gate the retrieval ranker uses before down-ranking a generated
// file. For foo.pb.go the peer is foo.go; for mock_user.go it is
// user.go; for user_pb2.py it is user.py.
//
// Returns nil when no clean peer name can be derived (e.g.
// zz_generated.deepcopy.go has no same-named hand-written twin). A
// nil result means "do not gate" — i.e. leave the generated file
// un-penalised, which is the safe default: a generated file that is
// the only definition should not be demoted into oblivion.
func GeneratedPeerPaths(p string) []string {
	if p == "" {
		return nil
	}
	norm := filepathToSlash(p)
	dir := path.Dir(norm)
	base := path.Base(norm)
	lower := strings.ToLower(base)

	join := func(name string) string {
		if dir == "." || dir == "" {
			return name
		}
		return dir + "/" + name
	}

	// Suffix markers: strip the generated marker, swap in the
	// hand-written extension. Ordered longest-first so _pb2_grpc.py
	// wins over _pb2.py and .designer.cs over .cs.
	suffixRules := []struct{ suf, ext string }{
		// Python.
		{"_pb2_grpc.py", ".py"},
		{"_pb2.pyi", ".py"},
		{"_pb2.py", ".py"},
		// Go.
		{".pb.go", ".go"},
		{"_generated.go", ".go"},
		{".generated.go", ".go"},
		{"_gen.go", ".go"},
		{".gen.go", ".go"},
		{"_mocks.go", ".go"},
		{"_mock.go", ".go"},
		{".pulsar.go", ".go"},
		// TS / JS — `_grpc_pb` before `_pb`, longer `.generated`/`.gen` first.
		{"_grpc_pb.ts", ".ts"},
		{"_grpc_pb.js", ".js"},
		{"_pb.ts", ".ts"},
		{"_pb.js", ".js"},
		{".generated.tsx", ".tsx"},
		{".generated.jsx", ".jsx"},
		{".generated.ts", ".ts"},
		{".generated.js", ".js"},
		{".gen.tsx", ".tsx"},
		{".gen.jsx", ".jsx"},
		{".gen.ts", ".ts"},
		{".gen.js", ".js"},
		{".min.mjs", ".mjs"},
		{".min.js", ".js"},
		// Rust.
		{".generated.rs", ".rs"},
		// Dart.
		{".pbserver.dart", ".dart"},
		{".pbgrpc.dart", ".dart"},
		{".pbenum.dart", ".dart"},
		{".pbjson.dart", ".dart"},
		{".chopper.dart", ".dart"},
		{".freezed.dart", ".dart"},
		{".config.dart", ".dart"},
		{".pb.dart", ".dart"},
		{".gr.dart", ".dart"},
		{".g.dart", ".dart"},
		// C#.
		{".designer.cs", ".cs"},
		{".g.cs", ".cs"},
		// Java generated classes (no separator before the marker).
		{"grpc.java", ".java"},
		{"outerclass.java", ".java"},
	}
	for _, r := range suffixRules {
		if strings.HasSuffix(lower, r.suf) {
			stem := base[:len(base)-len(r.suf)]
			if stem == "" {
				return nil
			}
			return []string{join(stem + r.ext)}
		}
	}

	// Prefix marker: mock_user.go shadows user.go.
	if strings.HasPrefix(lower, "mock_") && strings.HasSuffix(lower, ".go") {
		rest := base[len("mock_"):]
		if rest != "" && rest != ".go" {
			return []string{join(rest)}
		}
	}
	return nil
}

// filepathToSlash normalises backslashes to forward slashes without
// pulling in path/filepath (which would make the leaf package
// OS-aware). Graph paths are always stored forward-slashed.
func filepathToSlash(p string) string {
	return strings.ReplaceAll(p, "\\", "/")
}
