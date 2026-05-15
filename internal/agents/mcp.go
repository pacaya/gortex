package agents

import (
	"encoding/json"
	"reflect"
)

// UpsertMCPServer merges a gortex-flavored MCP server stanza into a
// map that follows the standard {"mcpServers": {<name>: {...}}}
// shape used by Claude Code, Cursor, VS Code, Continue.dev, Cline,
// and Kiro. Returns true when the map was modified (false when a
// gortex stanza was already present and opts.Force is off).
//
// serverName is the key under mcpServers (canonically "gortex").
// entry is the stanza value — adapters produce their own variant
// when the target client uses a different shape (e.g. Cline's
// alwaysAllow list, Kiro's autoApprove list).
func UpsertMCPServer(root map[string]any, serverName string, entry map[string]any, opts ApplyOpts) (changed bool) {
	servers, ok := root["mcpServers"].(map[string]any)
	if !ok {
		servers = make(map[string]any)
	}
	if _, exists := servers[serverName]; exists && !opts.Force {
		return false
	}
	servers[serverName] = entry
	root["mcpServers"] = servers
	return true
}

// UpsertMCPServerWithMigration is like UpsertMCPServer but also
// rewrites entries that look Gortex-authored (any `gortex mcp ...`
// stanza) even without opts.Force. This lets `gortex install` swap
// the legacy `mcp --index . --watch` shape — which fails Cursor's
// global-cwd handshake — for the daemon-proxy shape on next install.
//
// If the existing entry is already byte-identical to `entry`, returns
// false so the caller reports "already-configured" instead of a noisy
// rewrite. User-customized entries (anything that doesn't look like
// Gortex authored it) are left untouched unless opts.Force is set.
func UpsertMCPServerWithMigration(root map[string]any, serverName string, entry map[string]any, opts ApplyOpts) (changed bool) {
	servers, ok := root["mcpServers"].(map[string]any)
	if !ok {
		servers = make(map[string]any)
	}
	if existing, exists := servers[serverName]; exists {
		if mcpEntriesEqual(existing, entry) {
			return false
		}
		if !opts.Force && !IsGortexAuthoredMCPEntry(existing) {
			return false
		}
	}
	servers[serverName] = entry
	root["mcpServers"] = servers
	return true
}

// IsGortexAuthoredMCPEntry returns true for MCP server stanzas that
// look like Gortex wrote them — `command == "gortex"` and the args
// list starts with the `mcp` subcommand. Used by global-mode
// installers to migrate their own legacy stanzas without clobbering
// user-customized servers.
func IsGortexAuthoredMCPEntry(entry any) bool {
	m, ok := entry.(map[string]any)
	if !ok {
		return false
	}
	cmd, _ := m["command"].(string)
	if cmd != "gortex" {
		return false
	}
	args, ok := m["args"].([]any)
	if !ok || len(args) == 0 {
		return false
	}
	first, _ := args[0].(string)
	return first == "mcp"
}

// mcpEntriesEqual compares two MCP stanzas by their JSON-marshaled
// form. Round-tripping is the simplest way to handle the []string vs
// []any drift between freshly-built entries and entries decoded from
// an existing mcp.json on disk.
func mcpEntriesEqual(a, b any) bool {
	aj, err := json.Marshal(a)
	if err != nil {
		return false
	}
	bj, err := json.Marshal(b)
	if err != nil {
		return false
	}
	var av, bv any
	if err := json.Unmarshal(aj, &av); err != nil {
		return false
	}
	if err := json.Unmarshal(bj, &bv); err != nil {
		return false
	}
	return reflect.DeepEqual(av, bv)
}

// DefaultGortexMCPEntry returns the shared {command, args, env}
// stanza most clients accept for project-local MCP configs (where
// the editor launches the process with cwd set to the project root).
// Adapters that want extra keys wrap this and add them (e.g. Cline's
// alwaysAllow, Kiro's autoApprove).
//
// The command intentionally points at the bare "gortex" binary on
// PATH rather than os.Executable() — users who installed via
// Homebrew or `go install` get a stable path, and installers that
// run `go build -o /tmp/...` don't bake the transient path into
// long-lived configs.
func DefaultGortexMCPEntry() map[string]any {
	return map[string]any{
		"command": "gortex",
		"args":    []string{"mcp", "--index", ".", "--watch"},
		"env":     map[string]string{"GORTEX_INDEX_WORKERS": "8"},
	}
}

// GlobalGortexMCPEntry returns the daemon-proxy MCP entry suitable
// for user-level (global) configs, where the editor may launch the
// MCP process with cwd set to the user's home directory rather than
// any open project — Cursor's global-mcp behaviour reported in
// gortexhq/gortex#19.
//
// The proxy shape carries no cwd-relative state: the daemon resolves
// the active workspace per session from the request handshake, so
// the global config never trips the strict entry-point check on the
// home directory. If no daemon is running, `gortex mcp --proxy` exits
// with a clear "no daemon" error rather than silently falling back
// to a broken indexer.
func GlobalGortexMCPEntry() map[string]any {
	return map[string]any{
		"command": "gortex",
		"args":    []string{"mcp", "--proxy"},
		"env":     map[string]string{"GORTEX_INDEX_WORKERS": "8"},
	}
}
