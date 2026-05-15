package agents

import (
	"encoding/json"
	"testing"
)

// TestGlobalGortexMCPEntryUsesProxy verifies that the user-level MCP
// entry is daemon-only — no cwd-relative `--index .` arg. This is the
// load-bearing property for gortexhq/gortex#19: Cursor launches the
// global config with cwd=$HOME, so any cwd-relative indexing would
// fail the entry-point handshake.
func TestGlobalGortexMCPEntryUsesProxy(t *testing.T) {
	entry := GlobalGortexMCPEntry()

	cmd, _ := entry["command"].(string)
	if cmd != "gortex" {
		t.Fatalf("command = %q, want %q", cmd, "gortex")
	}

	args, _ := entry["args"].([]string)
	if len(args) == 0 || args[0] != "mcp" {
		t.Fatalf("args[0] = %v, want mcp; full args=%v", args, args)
	}
	wantProxy := false
	for _, a := range args {
		if a == "--index" {
			t.Errorf("global entry must not pass --index (resolves to launch cwd, breaks Cursor global config): args=%v", args)
		}
		if a == "--proxy" {
			wantProxy = true
		}
	}
	if !wantProxy {
		t.Errorf("global entry must use --proxy so an absent daemon fails fast instead of silently binding to $HOME: args=%v", args)
	}
}

// TestIsGortexAuthoredMCPEntry checks the heuristic used by
// UpsertMCPServerWithMigration to decide which existing stanzas are
// safe to overwrite during a global-install migration.
func TestIsGortexAuthoredMCPEntry(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want bool
	}{
		{
			name: "legacy --index . shape (the bug from issue 19)",
			raw:  `{"command":"gortex","args":["mcp","--index",".","--watch"]}`,
			want: true,
		},
		{
			name: "current --proxy shape",
			raw:  `{"command":"gortex","args":["mcp","--proxy"]}`,
			want: true,
		},
		{
			name: "user wrapper that shells gortex through a script",
			raw:  `{"command":"/usr/local/bin/wrap-gortex.sh","args":["mcp"]}`,
			want: false,
		},
		{
			name: "user's own MCP server, not gortex at all",
			raw:  `{"command":"other-tool","args":["serve"]}`,
			want: false,
		},
		{
			name: "gortex command but a different subcommand",
			raw:  `{"command":"gortex","args":["daemon","start"]}`,
			want: false,
		},
		{
			name: "empty args list",
			raw:  `{"command":"gortex","args":[]}`,
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var v any
			if err := json.Unmarshal([]byte(tc.raw), &v); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			got := IsGortexAuthoredMCPEntry(v)
			if got != tc.want {
				t.Errorf("IsGortexAuthoredMCPEntry(%s) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}

// TestUpsertMCPServerWithMigrationReplacesLegacy exercises the
// migration path: a user with an old global config containing
// `--index . --watch` should get rewritten to the proxy entry on next
// `gortex install`, even though opts.Force is off.
func TestUpsertMCPServerWithMigrationReplacesLegacy(t *testing.T) {
	root := map[string]any{
		"mcpServers": map[string]any{
			"gortex": map[string]any{
				"command": "gortex",
				"args":    []any{"mcp", "--index", ".", "--watch"},
				"env":     map[string]any{"GORTEX_INDEX_WORKERS": "8"},
			},
		},
	}
	want := GlobalGortexMCPEntry()

	changed := UpsertMCPServerWithMigration(root, "gortex", want, ApplyOpts{})
	if !changed {
		t.Fatalf("expected migration to rewrite legacy entry, got changed=false")
	}
	servers, _ := root["mcpServers"].(map[string]any)
	got, _ := servers["gortex"].(map[string]any)
	if got["command"] != "gortex" {
		t.Fatalf("command lost during migration: %v", got)
	}
	args, _ := got["args"].([]string)
	for _, a := range args {
		if a == "--index" {
			t.Fatalf("migration left legacy --index in args: %v", got)
		}
	}
}

// TestUpsertMCPServerWithMigrationIdempotent verifies the second run
// is a no-op once the config is already in the desired shape — no
// spurious rewrite, no noisy "modified" log.
func TestUpsertMCPServerWithMigrationIdempotent(t *testing.T) {
	// Use the on-disk shape (args as []any) to mirror what comes back
	// out of json.Unmarshal on an existing mcp.json.
	root := map[string]any{
		"mcpServers": map[string]any{
			"gortex": map[string]any{
				"command": "gortex",
				"args":    []any{"mcp", "--proxy"},
				"env":     map[string]any{"GORTEX_INDEX_WORKERS": "8"},
			},
		},
	}
	if changed := UpsertMCPServerWithMigration(root, "gortex", GlobalGortexMCPEntry(), ApplyOpts{}); changed {
		t.Errorf("expected no rewrite when entry already matches; got changed=true")
	}
}

// TestUpsertMCPServerWithMigrationPreservesUserCustomization confirms
// the migration is fingerprint-gated: a user who hand-rolled their
// own gortex wrapper (non-`gortex` command, or a different subcommand)
// should not have their config silently overwritten.
func TestUpsertMCPServerWithMigrationPreservesUserCustomization(t *testing.T) {
	root := map[string]any{
		"mcpServers": map[string]any{
			"gortex": map[string]any{
				"command": "/opt/scripts/my-gortex.sh",
				"args":    []any{"mcp"},
			},
		},
	}
	if changed := UpsertMCPServerWithMigration(root, "gortex", GlobalGortexMCPEntry(), ApplyOpts{}); changed {
		t.Errorf("user-customized entry was overwritten without --force")
	}
	// And with --force the same call should overwrite — that's the
	// escape hatch users have when they want Gortex to take over.
	if changed := UpsertMCPServerWithMigration(root, "gortex", GlobalGortexMCPEntry(), ApplyOpts{Force: true}); !changed {
		t.Errorf("opts.Force must overwrite user-customized entry")
	}
}
