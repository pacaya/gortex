package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/zzet/gortex/internal/mcp"
)

// guideCmd prints the Gortex reference guide — the same content served by the
// `gortex://guide` MCP resource, for harnesses without MCP-resource support or
// for shell use. The installed CLAUDE.md carries only the mandatory policy
// core and points here for everything else.
var guideCmd = &cobra.Command{
	Use:   "guide [topic]",
	Short: "Print the Gortex reference guide (providers, capabilities, tokens, analyze, search_ast, resources, workflow)",
	Long: `Prints the Gortex reference guide — the on-demand home for detail that is
not pre-paid in the installed CLAUDE.md: the LLM-provider matrix, the
non-obvious capabilities catalog, the token-economy deep-dive, the MCP
resources list, and the analyze / search_ast catalogs.

With no argument the full guide is printed with a topic index. Pass a topic
to print just that section:

  gortex guide providers
  gortex guide capabilities
  gortex guide tokens
  gortex guide analyze
  gortex guide search_ast
  gortex guide resources
  gortex guide workflow

The same content is served by the ` + "`gortex://guide`" + ` MCP resource
(section-addressable at ` + "`gortex://guide/{topic}`" + `).`,
	Args:      cobra.MaximumNArgs(1),
	ValidArgs: mcp.GuideTopics(),
	Run: func(cmd *cobra.Command, args []string) {
		topic := ""
		if len(args) == 1 {
			topic = args[0]
		}
		fmt.Fprint(cmd.OutOrStdout(), strings.TrimRight(mcp.GuideText(topic), "\n")+"\n")
	},
}

func init() {
	rootCmd.AddCommand(guideCmd)
}
