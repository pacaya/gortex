package main

import "testing"

// TestHelpCommandGroups asserts `gortex --help` is organized into intent
// groups (not a flat command list): the groups are registered, key
// commands carry the right group, and no command references an
// unregistered group (which cobra would panic on at help time).
func TestHelpCommandGroups(t *testing.T) {
	assignCommandGroups()

	registered := map[string]bool{}
	for _, g := range rootCmd.Groups() {
		registered[g.ID] = true
	}
	for _, id := range []string{"serve", "engine", "query", "edit", "memory", "index", "setup"} {
		if !registered[id] {
			t.Errorf("intent group %q is not registered", id)
		}
	}

	// Representative commands land in the expected group.
	want := map[string]string{
		"mcp": "serve", "daemon": "engine", "track": "engine",
		"query": "query", "audit": "query", "edit": "edit", "memory": "memory", "enrich": "index", "config": "setup",
	}
	got := map[string]string{}
	for _, c := range rootCmd.Commands() {
		got[c.Name()] = c.GroupID
	}
	for name, wantID := range want {
		if got[name] != wantID {
			t.Errorf("command %q group = %q, want %q", name, got[name], wantID)
		}
	}

	// Every assigned GroupID must reference a registered group.
	for _, c := range rootCmd.Commands() {
		if c.GroupID != "" && !registered[c.GroupID] {
			t.Errorf("command %q has unregistered group %q", c.Name(), c.GroupID)
		}
	}

	// Idempotent: a second call must not duplicate groups.
	before := len(rootCmd.Groups())
	assignCommandGroups()
	if after := len(rootCmd.Groups()); after != before {
		t.Errorf("assignCommandGroups not idempotent: groups %d -> %d", before, after)
	}
}
