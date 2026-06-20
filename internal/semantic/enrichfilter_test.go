package semantic

import "testing"

func TestIsLowValueForEnrichment(t *testing.T) {
	userGlobs := []string{"**/generated/**", "*.thrift.go", "legacy/*.c"}
	cases := []struct {
		path string
		want bool
	}{
		// tree-sitter generated parsers + runtime (the dominant clangd cost)
		{"tree-sitter-dart/src/parser.c", true},
		{"tree-sitter-dart/src/scanner.c", true},
		{"tree-sitter-dart/src/tree_sitter/array.h", true},
		{"tree-sitter-dart/src/tree_sitter/parser.h", true},
		{"some/repo/src/parser.h", true},
		// vendored / dependency dirs
		{"foo/vendor/bar/x.go", true},
		{"app/node_modules/lib/index.js", true},
		{"svc/third_party/proto/x.cc", true},
		// generated suffixes
		{"api/types.pb.go", true},
		{"gen/service_pb2.py", true},
		{"x_generated.go", true},
		// user globs
		{"src/generated/model.go", true},
		{"rpc/order.thrift.go", true},
		{"legacy/old.c", true},
		// real, hand-written sources — must NOT be excluded
		{"internal/semantic/manager.go", false},
		{"web/src/app/page.tsx", false},
		{"src/myparser.c", false}, // not exactly parser.c
		{"cmd/main.go", false},
		{"", false},
	}
	for _, c := range cases {
		if got := IsLowValueForEnrichment(c.path, userGlobs); got != c.want {
			t.Errorf("IsLowValueForEnrichment(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}
