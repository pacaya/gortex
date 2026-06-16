package main

import (
	"reflect"
	"testing"

	"github.com/zzet/gortex/internal/config"
)

// TestLSPDisabledSet_ConfigOnly — a `semantic.providers` entry with
// `enabled: false` whose name matches a known LSP spec lands in the
// disabled set. Entries with unknown names are ignored (so an
// `enabled: false` for a custom non-registry daemon doesn't shadow
// a same-named LSP).
func TestLSPDisabledSet_ConfigOnly(t *testing.T) {
	got := lspDisabledSet([]config.SemanticProviderConfig{
		{Name: "gopls", Enabled: false},
		{Name: "tsserver", Enabled: true}, // explicitly enabled — must NOT land in disabled
		{Name: "not-a-real-lsp", Enabled: false},
	}, "")
	want := map[string]bool{"gopls": true}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// TestLSPDisabledSet_EnvOnly — comma-separated names land in the
// disabled set. Whitespace is trimmed; empty entries are skipped.
func TestLSPDisabledSet_EnvOnly(t *testing.T) {
	got := lspDisabledSet(nil, "gopls, tsserver,, ,pyright")
	want := map[string]bool{"gopls": true, "tsserver": true, "pyright": true}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// TestLSPDisabledSet_EnvAllKillSwitch — the literal value "all" or
// "*" sets the special "__all__" key, signalling callers to skip
// auto-registration entirely.
func TestLSPDisabledSet_EnvAllKillSwitch(t *testing.T) {
	for _, env := range []string{"all", "ALL", "*", " all "} {
		got := lspDisabledSet(nil, env)
		if !got["__all__"] {
			t.Fatalf("env=%q: expected __all__ kill switch, got %v", env, got)
		}
	}
}

// TestLSPDisabledSet_ConfigAndEnvMerge — disables from both sources
// merge cleanly into one map.
func TestLSPDisabledSet_ConfigAndEnvMerge(t *testing.T) {
	got := lspDisabledSet([]config.SemanticProviderConfig{
		{Name: "gopls", Enabled: false},
	}, "tsserver,pyright")
	want := map[string]bool{
		"gopls":    true,
		"tsserver": true,
		"pyright":  true,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// TestLSPDisabledSet_Empty — no providers, empty env yields an empty
// map (not nil — callers index into it).
func TestLSPDisabledSet_Empty(t *testing.T) {
	got := lspDisabledSet(nil, "")
	if got == nil {
		t.Fatal("expected non-nil empty map")
	}
	if len(got) != 0 {
		t.Fatalf("expected empty map, got %v", got)
	}
}

// TestWarmMtimePrefix covers the single- vs multi-repo prefix decision the
// warm-restart mtime lookup hangs on. The bug it guards: a lone repo indexes
// unprefixed (rows under ""), but EffectiveRepoPrefix returns the basename, so
// looking up the basename finds nothing and forces a paid cold re-index every
// restart.
func TestWarmMtimePrefix(t *testing.T) {
	cases := []struct {
		name       string
		effective  string
		repoCount  int
		wantPrefix string
		wantOK     bool
	}{
		{"single repo uses empty prefix (the bug)", "drools", 1, "", true},
		{"single repo, zero configured still unprefixed", "drools", 0, "", true},
		{"multi-repo keeps its derived prefix", "drools", 2, "drools", true},
		{"multi-repo with no prefix is untrustworthy", "", 3, "", false},
		{"single repo already empty prefix", "", 1, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotPrefix, gotOK := warmMtimePrefix(tc.effective, tc.repoCount)
			if gotPrefix != tc.wantPrefix || gotOK != tc.wantOK {
				t.Fatalf("warmMtimePrefix(%q, %d) = (%q, %v), want (%q, %v)",
					tc.effective, tc.repoCount, gotPrefix, gotOK, tc.wantPrefix, tc.wantOK)
			}
		})
	}
}
