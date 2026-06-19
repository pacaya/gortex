package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

var (
	nodeCallers bool
	nodeFormat  string
	nodeContext int
	nodeIndex   string
)

var nodeCmd = &cobra.Command{
	Use:   "node <symbol-id>",
	Short: "Print one symbol's source (or its callers) by graph ID",
	Long: `Fetch a single graph symbol by its human-readable ID (e.g.
internal/foo.go::Bar) from the daemon that tracks the repo — the same source
get_symbol_source returns to an MCP client. With --callers, print the caller
trail (get_callers) instead, so a shell script gets Gortex's provenance-tagged
caller output without speaking the protocol.`,
	Args: cobra.ExactArgs(1),
	RunE: runNode,
}

func init() {
	nodeCmd.Flags().BoolVar(&nodeCallers, "callers", false, "print the symbol's callers (get_callers) instead of its source")
	nodeCmd.Flags().StringVarP(&nodeFormat, "format", "f", "", "output format: json|gcx|toon (default: tool default)")
	nodeCmd.Flags().IntVar(&nodeContext, "context", 3, "extra source lines above/below the symbol")
	nodeCmd.Flags().StringVar(&nodeIndex, "index", "", "repository path the daemon tracks (default: current directory)")
	rootCmd.AddCommand(nodeCmd)
}

// buildNodeArgs builds the get_symbol_source tool arguments for a symbol ID,
// omitting empty optional fields.
func buildNodeArgs(id, format string, contextLines int) map[string]any {
	args := map[string]any{"id": id, "context_lines": contextLines}
	if format != "" {
		args["format"] = format
	}
	return args
}

func runNode(cmd *cobra.Command, args []string) error {
	id := args[0]
	repoPath := nodeIndex
	if repoPath == "" {
		repoPath = "."
	}

	tool := "get_symbol_source"
	toolArgs := buildNodeArgs(id, nodeFormat, nodeContext)
	if nodeCallers {
		tool = "get_callers"
		toolArgs = map[string]any{"id": id}
		if nodeFormat != "" {
			toolArgs["format"] = nodeFormat
		}
	}

	out, err := requireDaemonTool(repoPath, tool, toolArgs)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintln(cmd.OutOrStdout(), string(out))
	return nil
}
