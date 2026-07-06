package main

import (
	"fmt"
	"io"
	"strings"

	gortexmcp "github.com/zzet/gortex/internal/mcp"
	"github.com/zzet/gortex/internal/toolref"
)

// maybeToolInvocationHint intercepts the `gortex <tool>` misuse: an agent that
// saw a bare MCP tool name and tried to run it as a top-level verb (e.g.
// `gortex read_file foo.go`). There is no such verb — the tool is reachable
// only as `gortex call <tool> --arg …`. Cobra would print a bare "unknown
// command" with no recovery path; instead, when the first positional argument
// names a registered MCP tool that is NOT already a cobra subcommand/alias,
// print a did-you-mean and return true so the caller exits nonzero.
//
// Cheap and daemon-free: the fast rejects (flag, known verb) run first, so the
// tool registry is only consulted for an argument that is otherwise an unknown
// command — never on a normal invocation.
func maybeToolInvocationHint(w io.Writer, args []string) bool {
	verb := firstPositionalArg(args)
	if verb == "" || strings.HasPrefix(verb, "-") {
		return false
	}
	if isKnownRootCommand(verb) {
		return false // a real cobra verb — let cobra route it
	}
	if !gortexmcp.IsRegisteredToolName(verb) {
		return false // genuinely unknown and not a tool — let cobra's error stand
	}

	fmt.Fprintf(w, "gortex: %q is not a gortex command, but it is a Gortex MCP tool.\n", verb)
	fmt.Fprintf(w, "Run it from a shell with:\n  %s\n", toolref.CLIFallback(verb))
	fmt.Fprintln(w, "General form: gortex call <tool> --arg k=v  (there is no bare `gortex <tool>` verb).")
	return true
}

// firstPositionalArg returns the first argument that is not an option flag,
// skipping the two value-taking persistent flags in their space-separated form
// so `gortex --config x read_file` still resolves to the intended verb. Stops
// at a "--" terminator.
func firstPositionalArg(args []string) string {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			if i+1 < len(args) {
				return args[i+1]
			}
			return ""
		}
		if strings.HasPrefix(a, "-") {
			if a == "--config" || a == "--log-level" {
				i++ // skip the flag's space-separated value
			}
			continue
		}
		return a
	}
	return ""
}

// isKnownRootCommand reports whether name matches a registered top-level cobra
// command or one of its aliases. No daemon, no tool registry — a plain walk of
// the already-registered command tree.
func isKnownRootCommand(name string) bool {
	for _, c := range rootCmd.Commands() {
		if c.Name() == name {
			return true
		}
		for _, a := range c.Aliases {
			if a == name {
				return true
			}
		}
	}
	return false
}
