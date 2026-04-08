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
)

var (
	indexLanguages []string
	indexExclude   []string
	indexWorkers   int
	indexOutput    string
	indexWatch     bool
)

var indexCmd = &cobra.Command{
	Use:   "index [path...]",
	Short: "Index one or more repositories and print stats",
	Args:  cobra.MinimumNArgs(0),
	RunE:  runIndex,
}

func init() {
	indexCmd.Flags().StringSliceVar(&indexLanguages, "languages", nil, "languages to parse (default: auto-detect)")
	indexCmd.Flags().StringSliceVar(&indexExclude, "exclude", nil, "additional glob patterns to exclude")
	indexCmd.Flags().IntVar(&indexWorkers, "workers", 0, "parallel parsing workers (default: NumCPU)")
	indexCmd.Flags().StringVar(&indexOutput, "output", "text", "output format: text|json")
	indexCmd.Flags().BoolVar(&indexWatch, "watch", false, "stay running and reindex on file changes")
	rootCmd.AddCommand(indexCmd)
}

func runIndex(cmd *cobra.Command, args []string) error {
	logger := newLogger()
	defer func() { _ = logger.Sync() }()

	// Default to current directory if no paths given.
	paths := args
	if len(paths) == 0 {
		paths = []string{"."}
	}

	cfg, err := config.Load(cfgFile)
	if err != nil {
		return err
	}

	if indexWorkers > 0 {
		cfg.Index.Workers = indexWorkers
	}
	if len(indexExclude) > 0 {
		cfg.Index.Exclude = append(cfg.Index.Exclude, indexExclude...)
	}

	// Index each path as a separate repository.
	for _, path := range paths {
		g := graph.New()
		reg := parser.NewRegistry()
		languages.RegisterAll(reg)

		idx := indexer.New(g, reg, cfg.Index, logger)
		result, err := idx.Index(path)
		if err != nil {
			return fmt.Errorf("indexing %s: %w", path, err)
		}

		switch indexOutput {
		case "json":
			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")
			if err := enc.Encode(result); err != nil {
				return err
			}
		default:
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Indexed %s: %d files in %dms\n", path, result.FileCount, result.DurationMs)
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  Nodes: %d\n", result.NodeCount)
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  Edges: %d\n", result.EdgeCount)
			if len(result.Errors) > 0 {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  Errors: %d\n", len(result.Errors))
				for _, e := range result.Errors {
					_, _ = fmt.Fprintf(cmd.OutOrStdout(), "    %s: %s\n", e.FilePath, e.Error)
				}
			}
		}
	}

	if indexWatch {
		_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "[gortex] watch mode not yet implemented")
	}

	return nil
}
