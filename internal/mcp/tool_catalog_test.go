package mcp

import (
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/daemon"
)

// TestToolDescriptors_CoversEveryToolOnce asserts the catalog enumerates
// every registered tool — live and deferred — exactly once, with no
// duplicates, and that the total matches the raw live+deferred surface.
func TestToolDescriptors_CoversEveryToolOnce(t *testing.T) {
	srv := newFullTestServer(t)

	descs := srv.ToolDescriptors()

	// Expected universe: the raw live MCP surface plus any deferred names.
	expected := make(map[string]bool)
	for name := range srv.mcpServer.ListTools() {
		expected[name] = true
	}
	if srv.lazy != nil {
		for _, name := range srv.lazy.DeferredNames() {
			expected[name] = true
		}
	}

	if len(descs) != len(expected) {
		t.Errorf("ToolDescriptors returned %d descriptors, want %d (live+deferred)", len(descs), len(expected))
	}

	seen := make(map[string]int, len(descs))
	for _, d := range descs {
		seen[d.Name]++
	}
	for name, n := range seen {
		if n != 1 {
			t.Errorf("tool %q appears %d times in descriptors (want exactly 1)", name, n)
		}
	}
	for name := range expected {
		if _, ok := seen[name]; !ok {
			t.Errorf("registered tool %q is missing from descriptors", name)
		}
	}
}

// TestToolDescriptors_NonEmptyCategory asserts every descriptor carries a
// category — toolCategory always classifies (falling back to "other"),
// so a blank category means the join broke.
func TestToolDescriptors_NonEmptyCategory(t *testing.T) {
	srv := newFullTestServer(t)
	for _, d := range srv.ToolDescriptors() {
		if d.Category == "" {
			t.Errorf("tool %q has an empty category", d.Name)
		}
	}
}

// TestToolDescriptors_MutatingMatchesDaemon asserts each descriptor's
// Mutating flag agrees with the authoritative daemon.MutatingTools set.
func TestToolDescriptors_MutatingMatchesDaemon(t *testing.T) {
	srv := newFullTestServer(t)
	for _, d := range srv.ToolDescriptors() {
		if want := daemon.MutatingTools[d.Name]; d.Mutating != want {
			t.Errorf("tool %q Mutating = %v, want %v (daemon.MutatingTools)", d.Name, d.Mutating, want)
		}
	}
}

// TestToolDescriptors_DescriptionNonEmptyAndTrimmed asserts every descriptor
// with a non-blank raw MCP description carries a matching, whitespace-trimmed
// Description, and that Summary is never longer than Description (Summary is
// derived from Description via firstSentence, so it can only truncate it).
func TestToolDescriptors_DescriptionNonEmptyAndTrimmed(t *testing.T) {
	srv := newFullTestServer(t)
	for _, d := range srv.ToolDescriptors() {
		if d.Description != strings.TrimSpace(d.Description) {
			t.Errorf("tool %q Description is not trimmed: %q", d.Name, d.Description)
		}
		if d.Summary != "" && len(d.Summary) > len(d.Description) {
			t.Errorf("tool %q Summary (%q) is longer than Description (%q)", d.Name, d.Summary, d.Description)
		}
		if d.Description == "" && d.Summary != "" {
			t.Errorf("tool %q has a Summary but an empty Description", d.Name)
		}
	}
}

// TestFirstSentence_DivergesFromDescriptionOnMultiSentenceInput asserts the
// Summary/Description split ToolDescriptors relies on: firstSentence
// truncates at the first ". " boundary, but a period immediately followed by
// a newline (no space) does not count as a boundary, so the whole
// (multi-line) description passes through unchanged.
func TestFirstSentence_DivergesFromDescriptionOnMultiSentenceInput(t *testing.T) {
	tests := []struct {
		name string
		desc string
		want string
	}{
		{"empty", "", ""},
		{"whitespace only", "   \n\t ", ""},
		{"single sentence", "Show who calls a function", "Show who calls a function"},
		{"multi-sentence period-space", "Show who calls a function. Returns the caller subgraph.", "Show who calls a function."},
		{"period-newline is not a boundary", "Show who calls a function.\nReturns the caller subgraph.", "Show who calls a function.\nReturns the caller subgraph."},
		{"leading/trailing whitespace trimmed", "  Show who calls a function.  ", "Show who calls a function."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := firstSentence(tt.desc); got != tt.want {
				t.Errorf("firstSentence(%q) = %q, want %q", tt.desc, got, tt.want)
			}
		})
	}
}

// TestToolDescriptors_CorePresetMembership asserts every tool in the core
// preset roster carries "core" in its Presets — guarding the
// presetsContaining join against drift in the preset rosters.
func TestToolDescriptors_CorePresetMembership(t *testing.T) {
	srv := newFullTestServer(t)
	presets := make(map[string][]string)
	for _, d := range srv.ToolDescriptors() {
		presets[d.Name] = d.Presets
	}
	for _, name := range corePresetTools {
		got, ok := presets[name]
		if !ok {
			t.Errorf("core preset tool %q not present in descriptors", name)
			continue
		}
		if !containsString(got, "core") {
			t.Errorf("core preset tool %q does not list \"core\" in its presets: %v", name, got)
		}
	}
}
