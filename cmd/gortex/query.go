package main

import (
	"context"
	"encoding/json"
	"errors"
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
	queryCmd.PersistentFlags().StringVar(&queryFormat, "format", "text", "output format: text|json|dot|mermaid")
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
	if queryFormat == "mermaid" {
		if sg, ok := v.(*query.SubGraph); ok {
			_, _ = fmt.Fprint(cmd.OutOrStdout(), sg.ToMermaid())
			return nil
		}
		return fmt.Errorf("--format mermaid is only supported for graph traversal queries (deps, dependents, callers, calls, usages, cluster)")
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

// tryDaemonTool runs a tool against the warm daemon when one owns the
// repo. It returns (result, true) on a daemon-served answer; (nil, false)
// when no daemon owns the repo or the repo is untracked — the Stage-1
// caller then falls back to a local index; and a real error otherwise
// (distinct from "daemon unavailable", which is never a hard error).
func tryDaemonTool(repoPath, tool string, args map[string]any) (json.RawMessage, bool, error) {
	exec, err := resolveExecutor(repoPath)
	if err != nil {
		if errors.Is(err, ErrNoExecutor) {
			return nil, false, nil
		}
		return nil, false, err
	}
	defer func() { _ = exec.Close() }()
	out, err := exec.CallTool(context.Background(), tool, args)
	if err != nil {
		if errors.Is(err, ErrRepoNotTracked) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return out, true, nil
}

// emitDaemonJSON re-indents and prints a daemon tool result for the
// --format json path.
func emitDaemonJSON(cmd *cobra.Command, raw json.RawMessage) error {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), string(raw))
		return nil
	}
	enc := json.NewEncoder(cmd.OutOrStdout())
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func printDaemonSearchSymbols(cmd *cobra.Command, raw json.RawMessage) error {
	if queryFormat == "json" {
		return emitDaemonJSON(cmd, raw)
	}
	var payload struct {
		Results []struct {
			ID       string `json:"id"`
			Kind     string `json:"kind"`
			FilePath string `json:"file_path"`
			Line     int    `json:"line"`
		} `json:"results"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return err
	}
	for _, r := range payload.Results {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%-12s %-40s %s:%d\n", r.Kind, r.ID, r.FilePath, r.Line)
	}
	return nil
}

func printDaemonStats(cmd *cobra.Command, raw json.RawMessage) error {
	if queryFormat == "json" {
		return emitDaemonJSON(cmd, raw)
	}
	var payload struct {
		TotalNodes int            `json:"total_nodes"`
		TotalEdges int            `json:"total_edges"`
		ByKind     map[string]int `json:"by_kind"`
		ByLanguage map[string]int `json:"by_language"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	_, _ = fmt.Fprintf(out, "Nodes: %d  Edges: %d\n", payload.TotalNodes, payload.TotalEdges)
	if len(payload.ByKind) > 0 {
		_, _ = fmt.Fprintln(out, "By kind:")
		for k, v := range payload.ByKind {
			_, _ = fmt.Fprintf(out, "  %-12s %d\n", k, v)
		}
	}
	if len(payload.ByLanguage) > 0 {
		_, _ = fmt.Fprintln(out, "By language:")
		for k, v := range payload.ByLanguage {
			_, _ = fmt.Fprintf(out, "  %-12s %d\n", k, v)
		}
	}
	return nil
}

var querySymbolCmd = &cobra.Command{
	Use:   "symbol <name>",
	Short: "Find symbols matching name",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		// Daemon-first: serve from the warm graph when one owns the repo.
		if out, ok, err := tryDaemonTool(queryIndex, "search_symbols",
			map[string]any{"query": args[0], "limit": queryLimit}); err != nil {
			return err
		} else if ok {
			return printDaemonSearchSymbols(cmd, out)
		}
		// Fallback: local index (Stage-1 backward compatibility).
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
		if out, ok, err := tryDaemonTool(queryIndex, "graph_stats", map[string]any{}); err != nil {
			return err
		} else if ok {
			return printDaemonStats(cmd, out)
		}
		eng, err := buildEngine()
		if err != nil {
			return err
		}
		return printResult(cmd, eng.Stats())
	},
}
