package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/zzet/gortex/internal/daemon"
)

var enrichChurnBranch string

var enrichChurnCmd = &cobra.Command{
	Use:   "churn [path]",
	Short: "Pre-compute per-symbol git churn from a fixed branch (default: origin/main)",
	Long: `Walks the daemon's graph and stamps meta.churn on every file and
function/method with the commit_count / age_days / churn_rate /
last_author / last_commit_at metrics the get_churn_rate MCP tool reads.

The signal is computed against a single branch — typically the
repository's default branch — so feature-branch work-in-progress
doesn't pollute the persisted data. Pass --branch to override.

The enrichment is forwarded to the running daemon, which runs it against
its in-process graph and persists the result (avoiding the on-disk store
write-lock collision a direct CLI write would cause). A daemon must be
running; if none is, the command exits with an error — start one with
` + "`gortex daemon start`" + `.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runEnrichChurn,
}

func init() {
	enrichChurnCmd.Flags().StringVar(&enrichChurnBranch, "branch", "",
		"branch / tag / SHA to compute churn against (default: origin/main, falls back to local main/master)")
	enrichCmd.AddCommand(enrichChurnCmd)
}

func runEnrichChurn(cmd *cobra.Command, args []string) error {
	abs, err := enrichAbsPath(args)
	if err != nil {
		return err
	}
	if !daemon.IsRunning() {
		return errNoDaemon
	}
	c, err := dialEnrichDaemon("cli-enrich-churn")
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()

	var out daemon.EnrichChurnResult
	if err := controlEnrich(c, daemon.ControlEnrichChurn, daemon.EnrichChurnParams{
		Path:   abs,
		Branch: enrichChurnBranch,
	}, &out); err != nil {
		return err
	}
	sp := newCLISpinner(cmd, "Enriched via daemon")
	sp.Set("", fmt.Sprintf("%d files · %d symbols · %s", out.Files, out.Symbols, out.Branch))
	sp.Done()
	return printEnrichResult(map[string]any{
		"files":       out.Files,
		"symbols":     out.Symbols,
		"branch":      out.Branch,
		"head_sha":    out.HeadSHA,
		"duration_ms": out.DurationMS,
		"path":        abs,
		"mode":        "daemon",
	})
}
