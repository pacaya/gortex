package main

import (
	"reflect"
	"strings"
	"testing"
)

// TestFilesCmdTreeFormat covers the files verb's three layouts and the
// find_files path extraction.
func TestFilesCmdTreeFormat(t *testing.T) {
	paths := []string{"internal/a/x.go", "internal/a/y.go", "internal/b/z.go", "main.go"}

	// Tree: sorted, hierarchical, directories suffixed with "/".
	wantTree := "internal/\n  a/\n    x.go\n    y.go\n  b/\n    z.go\nmain.go\n"
	if tree := renderFilesTree(paths); tree != wantTree {
		t.Errorf("renderFilesTree =\n%q\nwant\n%q", tree, wantTree)
	}

	// Flat: one sorted path per line.
	if flat := renderFilesFlat([]string{"b.go", "a.go"}); flat != "a.go\nb.go\n" {
		t.Errorf("renderFilesFlat = %q, want \"a.go\\nb.go\\n\"", flat)
	}

	// Grouped: files under their parent directory; root files under "(root)".
	grouped := renderFilesGrouped([]string{"internal/a/x.go", "main.go"})
	if !strings.Contains(grouped, "internal/a/\n  x.go\n") {
		t.Errorf("grouped missing internal/a group:\n%s", grouped)
	}
	if !strings.Contains(grouped, "(root)/\n  main.go\n") {
		t.Errorf("grouped missing root group:\n%s", grouped)
	}

	// renderFiles dispatches by format.
	if renderFiles(paths, "flat") != renderFilesFlat(paths) {
		t.Error("renderFiles(flat) must match renderFilesFlat")
	}
	if renderFiles(paths, "anything-else") != renderFilesTree(paths) {
		t.Error("renderFiles default must be the tree layout")
	}

	// parseFindFilesPaths extracts files[].path.
	raw := []byte(`{"files":[{"path":"a.go"},{"path":"b.go"}],"truncated":false}`)
	if got := parseFindFilesPaths(raw); !reflect.DeepEqual(got, []string{"a.go", "b.go"}) {
		t.Errorf("parseFindFilesPaths = %v, want [a.go b.go]", got)
	}
}
