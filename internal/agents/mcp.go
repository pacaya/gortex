package agents

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

// DefaultGortexMCPEntry returns the shared {command, args, env}
// stanza most clients accept. Adapters that want extra keys wrap
// this and add them (e.g. Cline's alwaysAllow, Kiro's autoApprove).
//
// The command intentionally points at the bare "gortex" binary on
// PATH rather than os.Executable() — users who installed via
// Homebrew or `go install` get a stable path, and installers that
// run `go build -o /tmp/...` don't bake the transient path into
// long-lived configs.
func DefaultGortexMCPEntry() map[string]any {
	return map[string]any{
		"command": "gortex",
		"args":    []string{"serve", "--index", ".", "--watch"},
		"env":     map[string]string{"GORTEX_INDEX_WORKERS": "8"},
	}
}
