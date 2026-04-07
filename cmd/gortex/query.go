package main

import (
	"encoding/json"
	"fmt"
	"github.com/spf13/cobra"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/query"
)

var (
	queryIndex  string
	queryDepth  int
	queryFormat string
	queryLimit  int
)

var queryCmd = &cobra.Command{
	Use:   "query",
	Short: "Query the knowledge graph",
}

func init() {
	queryCmd.PersistentFlags().StringVar(&queryIndex, "index", ".", "repository path to index")
	queryCmd.PersistentFlags().IntVar(&queryDepth, "depth", 3, "traversal depth")
	queryCmd.PersistentFlags().StringVar(&queryFormat, "format", "text", "output format: text|json|dot")
	queryCmd.PersistentFlags().IntVar(&queryLimit, "limit", 50, "max nodes in result")

	queryCmd.AddCommand(querySymbolCmd)
	queryCmd.AddCommand(queryDepsCmd)
	queryCmd.AddCommand(queryDependentsCmd)
	queryCmd.AddCommand(queryCallersCmd)
	queryCmd.AddCommand(queryCallsCmd)
	queryCmd.AddCommand(queryImplementationsCmd)
	queryCmd.AddCommand(queryUsagesCmd)
	queryCmd.AddCommand(queryStatsCmd)

	rootCmd.AddCommand(queryCmd)
}

func buildEngine() (*query.Engine, error) {
	logger := newLogger()
	cfg, err := config.Load(cfgFile)
	if err != nil {
		return nil, err
	}
	g := graph.New()
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	idx := indexer.New(g, reg, cfg.Index, logger)
	if _, err := idx.Index(queryIndex); err != nil {
		return nil, err
	}
	eng := query.NewEngine(g)
	eng.SetSearch(idx.Search())
	return eng, nil
}

func opts() query.QueryOptions {
	return query.QueryOptions{Depth: queryDepth, Limit: queryLimit, Detail: "brief"}
}

func printResult(cmd *cobra.Command, v any) error {
	if queryFormat == "json" {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(v)
	}
	if queryFormat == "dot" {
		if sg, ok := v.(*query.SubGraph); ok {
			_, _ = fmt.Fprint(cmd.OutOrStdout(), sg.ToDot())
			return nil
		}
		return fmt.Errorf("--format dot is only supported for graph traversal queries (deps, dependents, callers, calls, usages, cluster)")
	}
	// Text format.
	switch val := v.(type) {
	case *query.SubGraph:
		for _, n := range val.Nodes {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%-12s %-40s %s:%d\n", n.Kind, n.ID, n.FilePath, n.StartLine)
		}
		if val.Truncated {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "... truncated (%d total)\n", val.TotalNodes)
		}
	case []*graph.Node:
		for _, n := range val {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%-12s %-40s %s:%d\n", n.Kind, n.ID, n.FilePath, n.StartLine)
		}
	case *graph.GraphStats:
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Nodes: %d  Edges: %d\n", val.TotalNodes, val.TotalEdges)
		if len(val.ByKind) > 0 {
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "By kind:")
			for k, v := range val.ByKind {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  %-12s %d\n", k, v)
			}
		}
		if len(val.ByLanguage) > 0 {
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "By language:")
			for k, v := range val.ByLanguage {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  %-12s %d\n", k, v)
			}
		}
	}
	return nil
}

var querySymbolCmd = &cobra.Command{
	Use:   "symbol <name>",
	Short: "Find symbols matching name",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		eng, err := buildEngine()
		if err != nil {
			return err
		}
		nodes := eng.FindSymbols(args[0])
		return printResult(cmd, nodes)
	},
}

var queryDepsCmd = &cobra.Command{
	Use:   "deps <id>",
	Short: "Show dependencies of a symbol",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		eng, err := buildEngine()
		if err != nil {
			return err
		}
		return printResult(cmd, eng.GetDependencies(args[0], opts()))
	},
}

var queryDependentsCmd = &cobra.Command{
	Use:   "dependents <id>",
	Short: "Show blast radius for a symbol",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		eng, err := buildEngine()
		if err != nil {
			return err
		}
		return printResult(cmd, eng.GetDependents(args[0], opts()))
	},
}

var queryCallersCmd = &cobra.Command{
	Use:   "callers <func-id>",
	Short: "Show who calls a function",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		eng, err := buildEngine()
		if err != nil {
			return err
		}
		return printResult(cmd, eng.GetCallers(args[0], opts()))
	},
}

var queryCallsCmd = &cobra.Command{
	Use:   "calls <func-id>",
	Short: "Show what a function calls",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		eng, err := buildEngine()
		if err != nil {
			return err
		}
		return printResult(cmd, eng.GetCallChain(args[0], opts()))
	},
}

var queryImplementationsCmd = &cobra.Command{
	Use:   "implementations <interface-id>",
	Short: "Show implementations of an interface",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		eng, err := buildEngine()
		if err != nil {
			return err
		}
		return printResult(cmd, eng.FindImplementations(args[0]))
	},
}

var queryUsagesCmd = &cobra.Command{
	Use:   "usages <id>",
	Short: "Show all usages of a symbol",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		eng, err := buildEngine()
		if err != nil {
			return err
		}
		return printResult(cmd, eng.FindUsages(args[0]))
	},
}

var queryStatsCmd = &cobra.Command{
	Use:   "stats",
	Short: "Show graph statistics",
	RunE: func(cmd *cobra.Command, args []string) error {
		eng, err := buildEngine()
		if err != nil {
			return err
		}
		return printResult(cmd, eng.Stats())
	},
}
