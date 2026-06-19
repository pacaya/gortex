package main

import "testing"

// TestNodeCmdBuildsArgs covers the get_symbol_source argument shaping the node
// verb performs, including omission of an empty format.
func TestNodeCmdBuildsArgs(t *testing.T) {
	args := buildNodeArgs("internal/p.go::Parse", "json", 5)
	if args["id"] != "internal/p.go::Parse" {
		t.Errorf("id = %v", args["id"])
	}
	if args["format"] != "json" {
		t.Errorf("format = %v", args["format"])
	}
	if args["context_lines"] != 5 {
		t.Errorf("context_lines = %v", args["context_lines"])
	}

	bare := buildNodeArgs("x::Y", "", 3)
	if _, ok := bare["format"]; ok {
		t.Error("an empty format must be omitted")
	}
	if bare["context_lines"] != 3 {
		t.Errorf("context_lines = %v, want 3", bare["context_lines"])
	}
}
