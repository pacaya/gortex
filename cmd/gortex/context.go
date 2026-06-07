package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

var (
	contextTask       string
	contextEntryPoint string
	contextMaxSymbols int
	contextFormat     string
	contextBudget     int
	contextIndex      string
)

var contextCmd = &cobra.Command{
	Use:   "context [flags]",
	Short: "Generate a portable context briefing for a task",
	Long: `Runs export_context for the given task against the daemon that owns the
repo and renders the result as a self-contained markdown or JSON briefing.
Use for sharing context outside MCP — paste into Slack, PRs, docs, or other
AI tools. Requires a running daemon that tracks the repo.`,
	RunE: runContext,
}

func init() {
	contextCmd.Flags().StringVarP(&contextTask, "task", "t", "", "task description (required)")
	contextCmd.Flags().StringVarP(&contextEntryPoint, "entry-point", "e", "", "symbol ID or file path to start from")
	contextCmd.Flags().IntVarP(&contextMaxSymbols, "max-symbols", "n", 5, "max symbols to include")
	contextCmd.Flags().StringVarP(&contextFormat, "format", "f", "markdown", "output format: markdown or json")
	contextCmd.Flags().IntVar(&contextBudget, "token-budget", 2000, "approximate token budget for output")
	contextCmd.Flags().StringVar(&contextIndex, "index", "", "repository path the daemon must track (default: current directory)")
	_ = contextCmd.MarkFlagRequired("task")
	rootCmd.AddCommand(contextCmd)
}

func runContext(cmd *cobra.Command, args []string) error {
	repoPath := "."
	if contextIndex != "" {
		repoPath = contextIndex
	} else if len(args) > 0 {
		repoPath = args[0]
	}

	out, err := requireDaemonTool(repoPath, "export_context", map[string]any{
		"task":         contextTask,
		"entry_point":  contextEntryPoint,
		"max_symbols":  contextMaxSymbols,
		"format":       contextFormat,
		"token_budget": contextBudget,
	})
	if err != nil {
		return err
	}
	// export_context returns the rendered briefing (markdown or JSON) as
	// the tool's text content; print it verbatim.
	_, _ = fmt.Fprintln(cmd.OutOrStdout(), string(out))
	return nil
}
