package main

import "testing"

// TestExploreCmdBuildsArgs covers the smart_context argument shaping the
// explore verb performs, including omission of empty optionals.
func TestExploreCmdBuildsArgs(t *testing.T) {
	args := buildExploreArgs("find the parser", "internal/p.go::Parse", "gcx", 7)
	if args["task"] != "find the parser" {
		t.Errorf("task = %v", args["task"])
	}
	if args["entry_point"] != "internal/p.go::Parse" {
		t.Errorf("entry_point = %v", args["entry_point"])
	}
	if args["format"] != "gcx" {
		t.Errorf("format = %v", args["format"])
	}
	if args["max_symbols"] != 7 {
		t.Errorf("max_symbols = %v", args["max_symbols"])
	}

	bare := buildExploreArgs("t", "", "", 5)
	if _, ok := bare["entry_point"]; ok {
		t.Error("an empty entry_point must be omitted")
	}
	if _, ok := bare["format"]; ok {
		t.Error("an empty format must be omitted")
	}
	if bare["max_symbols"] != 5 {
		t.Errorf("max_symbols = %v, want 5", bare["max_symbols"])
	}
}
