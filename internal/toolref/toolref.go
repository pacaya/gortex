// Package toolref renders references to Gortex MCP tools for guidance text so
// every hook, adapter, and CLI message names a tool the SAME way — and never
// mints the invalid bare `gortex <tool>` shell shape that led agents astray
// (an agent that sees a bare tool name in a shell context invents
// `gortex read_file <path>`, which is not a real verb). MCP-directed guidance
// says "call the `<tool>` MCP tool"; a shell fallback renders the real
// `gortex call <tool> --arg k=v` invocation, which is the ONE correct way to
// reach a tool that has no dedicated CLI verb.
package toolref

import "strings"

// exampleArg maps a Gortex MCP tool to a realistic `--arg` for its shell
// fallback, so the rendered hint reads `gortex call read_file --arg path=<file>`
// rather than a shapeless `key=value`. A tool absent here falls back to the
// generic `key=value`, which is still a valid invocation shape.
var exampleArg = map[string]string{
	"read_file":           "path=<file>",
	"get_symbol_source":   "symbol=<file>::<Name>",
	"get_editing_context": "path=<file>",
	"get_file_summary":    "path=<file>",
	"get_symbol":          "id=<file>::<Name>",
	"search_symbols":      "query=<name>",
	"search_text":         "query=<text>",
	"find_usages":         "symbol=<file>::<Name>",
	"get_callers":         "symbol=<file>::<Name>",
	"smart_context":       "task=<what you want to do>",
	"get_repo_outline":    "path_prefix=<dir>/",
	"edit_file":           "path=<file>",
	"edit_symbol":         "id=<file>::<Name>",
}

// MCPRef renders an MCP-directed reference to a tool: "call the `read_file` MCP
// tool". Use wherever guidance assumes the agent has the Gortex MCP server
// mounted and can call the tool directly.
func MCPRef(tool string) string {
	return "call the `" + tool + "` MCP tool"
}

// CLIFallback renders the shell-fallback invocation for one tool:
// `gortex call read_file --arg path=<file>`. This is the single place a tool
// name becomes a shell command — nothing else should hand-assemble a
// `gortex …` shape, so the bare-verb mistake can never be re-minted piecemeal.
func CLIFallback(tool string) string {
	arg := exampleArg[tool]
	if arg == "" {
		arg = "key=value"
	}
	return "gortex call " + tool + " --arg " + arg
}

// FallbackLine is the standard one-line advisory appended to graph-tool
// guidance. It teaches the `gortex call <tool> --arg …` shape (with a realistic
// example for the primary tool) so an agent in a shell context — MCP unmounted
// or degraded, or one that chose Bash — never invents the invalid
// `gortex <tool>` verb. primary names the most relevant tool for the worked
// example; pass "" to emit only the generic form. The returned string ends in a
// newline so it drops straight into a bulleted guidance block.
func FallbackLine(primary string) string {
	var b strings.Builder
	b.WriteString("  - Shell only (no MCP tools)? Reach any tool above with `gortex call <tool> --arg k=v`")
	if primary != "" {
		b.WriteString(" — e.g. `" + CLIFallback(primary) + "`")
	}
	b.WriteString(". There is no bare `gortex <tool>` verb.\n")
	return b.String()
}
