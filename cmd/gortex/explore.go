package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

var (
	exploreEntryPoint string
	exploreFormat     string
	exploreMaxSymbols int
	exploreIndex      string
)

var exploreCmd = &cobra.Command{
	Use:   "explore <task...>",
	Short: "Assemble the relevant working set for a task (shell smart_context)",
	Long: `Run smart_context for a task against the daemon that tracks the repo and print
the assembled working set — the same payload an MCP client gets, so a non-MCP
Task subagent (or a shell script) can explore the graph without a protocol.`,
	Args: cobra.MinimumNArgs(1),
	RunE: runExplore,
}

func init() {
	exploreCmd.Flags().StringVarP(&exploreEntryPoint, "entry-point", "e", "", "symbol ID or file path to start from")
	exploreCmd.Flags().StringVarP(&exploreFormat, "format", "f", "", "output format: json|gcx|toon (default: tool default)")
	exploreCmd.Flags().IntVarP(&exploreMaxSymbols, "max-symbols", "n", 5, "max symbols to include source for")
	exploreCmd.Flags().StringVar(&exploreIndex, "index", "", "repository path the daemon tracks (default: current directory)")
	rootCmd.AddCommand(exploreCmd)
}

// buildExploreArgs builds the smart_context tool arguments from the explore
// flags, omitting empty optional fields.
func buildExploreArgs(task, entryPoint, format string, maxSymbols int) map[string]any {
	args := map[string]any{"task": task, "max_symbols": maxSymbols}
	if entryPoint != "" {
		args["entry_point"] = entryPoint
	}
	if format != "" {
		args["format"] = format
	}
	return args
}

func runExplore(cmd *cobra.Command, args []string) error {
	task := strings.TrimSpace(strings.Join(args, " "))
	if task == "" {
		return fmt.Errorf("explore: a task description is required")
	}
	repoPath := exploreIndex
	if repoPath == "" {
		repoPath = "."
	}
	out, err := requireDaemonTool(repoPath, "smart_context",
		buildExploreArgs(task, exploreEntryPoint, exploreFormat, exploreMaxSymbols))
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintln(cmd.OutOrStdout(), string(out))
	return nil
}
