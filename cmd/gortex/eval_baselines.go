// eval_baselines.go — wires `gortex eval baselines` to the
// bench/baselines/ harness. Same shell-out pattern as eval_tokens
// so the harness stays the source of truth.
package main

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/spf13/cobra"
)

var (
	evalBaselinesRepo        string
	evalBaselinesQueries     string
	evalBaselinesGroundtruth string
	evalBaselinesAgainst     string
	evalBaselinesTopK        int
	evalBaselinesFormat      string
	evalBaselinesOut         string
	evalBaselinesJSON        string
	evalBaselinesSmoke       bool
)

var evalBaselinesCmd = &cobra.Command{
	Use:   "baselines",
	Short: "Per-baseline NDCG@10 + latency across retrieval adapters (ripgrep, probe, colgrep, grepai, coderankembed, semble)",
	Long: `Runs the same query set through each registered baseline
adapter and reports NDCG@10 + median latency in a side-by-side
table. Per-adapter Available() checks let the harness skip
baselines whose external deps aren't installed locally — useful
in CI where the heavy Python ones (colgrep / grepai /
coderankembed / semble) typically aren't.

Use --smoke to just probe availability without running anything.

Substrate: bench/baselines/. See bench/baselines/README.md for
install instructions per adapter.`,
	RunE: runEvalBaselines,
}

func init() {
	evalBaselinesCmd.Flags().StringVar(&evalBaselinesRepo, "repo", ".", "indexed corpus path")
	evalBaselinesCmd.Flags().StringVar(&evalBaselinesQueries, "queries", "bench/baselines/queries.json", "JSON query set")
	evalBaselinesCmd.Flags().StringVar(&evalBaselinesGroundtruth, "groundtruth", "bench/baselines/groundtruth.json", "JSON per-query expected file paths")
	evalBaselinesCmd.Flags().StringVar(&evalBaselinesAgainst, "against", "ripgrep,probe,colgrep,grepai,coderankembed,semble", "comma-separated adapter names")
	evalBaselinesCmd.Flags().IntVar(&evalBaselinesTopK, "top-k", 10, "top-K per query (matches NDCG@10)")
	evalBaselinesCmd.Flags().StringVar(&evalBaselinesFormat, "format", "markdown", "markdown | json")
	evalBaselinesCmd.Flags().StringVar(&evalBaselinesOut, "out", "", "primary output path (default stdout)")
	evalBaselinesCmd.Flags().StringVar(&evalBaselinesJSON, "json", "", "companion JSON metrics output")
	evalBaselinesCmd.Flags().BoolVar(&evalBaselinesSmoke, "smoke", false, "skip runs; only probe Available() for each adapter")
	evalCmd.AddCommand(evalBaselinesCmd)
}

func runEvalBaselines(cmd *cobra.Command, _ []string) error {
	args := []string{
		"run", "./bench/baselines",
		"-repo", evalBaselinesRepo,
		"-queries", evalBaselinesQueries,
		"-groundtruth", evalBaselinesGroundtruth,
		"-against", evalBaselinesAgainst,
		"-top-k", fmt.Sprintf("%d", evalBaselinesTopK),
		"-format", evalBaselinesFormat,
	}
	if evalBaselinesOut != "" {
		args = append(args, "-out", evalBaselinesOut)
	}
	if evalBaselinesJSON != "" {
		args = append(args, "-json", evalBaselinesJSON)
	}
	if evalBaselinesSmoke {
		args = append(args, "-smoke")
	}
	subproc := exec.Command("go", args...)
	subproc.Stdin = os.Stdin
	subproc.Stdout = cmd.OutOrStdout()
	subproc.Stderr = cmd.ErrOrStderr()
	return subproc.Run()
}
