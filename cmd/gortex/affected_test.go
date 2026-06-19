package main

import (
	"reflect"
	"strings"
	"testing"
)

// TestAffectedStdinExitCode covers the affected verb's CI contract: the
// exit-code mapping, stdin parsing, input dedupe, and test-target extraction.
func TestAffectedStdinExitCode(t *testing.T) {
	// Exit-code contract: something affected → 0 (run tests); nothing → sentinel.
	if got := affectedExitCode(0); got != exitNoAffected {
		t.Errorf("affectedExitCode(0) = %d, want %d", got, exitNoAffected)
	}
	if got := affectedExitCode(3); got != 0 {
		t.Errorf("affectedExitCode(3) = %d, want 0", got)
	}

	// Stdin parsing tolerates newline- and space-separated paths + blank lines.
	got := parseAffectedStdin(strings.NewReader("a.go\nb.go c.go\n\n  d.go  \n"))
	if want := []string{"a.go", "b.go", "c.go", "d.go"}; !reflect.DeepEqual(got, want) {
		t.Errorf("parseAffectedStdin = %v, want %v", got, want)
	}

	// dedupeNonEmpty trims, drops blanks, dedupes, preserves order.
	if d := dedupeNonEmpty([]string{" a ", " a ", "", "b"}); !reflect.DeepEqual(d, []string{"a", "b"}) {
		t.Errorf("dedupeNonEmpty = %v, want [a b]", d)
	}

	// parseTestTargetFiles pulls unique, sorted test files from the payload.
	raw := []byte(`{"test_targets":[{"file":"z_test.go","functions":["T1"]},{"file":"a_test.go"},{"file":"z_test.go"}]}`)
	if files := parseTestTargetFiles(raw); !reflect.DeepEqual(files, []string{"a_test.go", "z_test.go"}) {
		t.Errorf("parseTestTargetFiles = %v, want [a_test.go z_test.go]", files)
	}
}
