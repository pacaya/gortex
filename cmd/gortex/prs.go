package main

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/zzet/gortex/internal/churn"
	"github.com/zzet/gortex/internal/forge"
	"github.com/zzet/gortex/internal/indexer"
)

var (
	prsBase      string
	prsRepo      string
	prsFormat    string
	prsWorktrees bool
)

// Seams. The forge free functions and the daemon-tool relay are indirected
// through package vars so a test can inject canned PRs / files and stub the
// daemon call without touching the network or a real daemon.
var (
	forgeAvailable = forge.Available
	forgeListPRs   = forge.ListPRs
	forgePRFiles   = forge.PRFiles
	prsDaemonTool  = requireDaemonTool
)

var prsCmd = &cobra.Command{
	Use:   "prs [number]",
	Short: "List open pull requests, or deep-dive a PR's blast radius",
	Long: `Without an argument, lists the repository's open pull requests as a table
with each PR's CI rollup, review decision, age, and a one-shot review-state
classification (DRAFT / BASE_MISMATCH / CHANGES_REQUESTED / APPROVED / STALE
/ READY).

With a PR number, deep-dives that PR: fetches its changed files from the
forge and joins them against the knowledge graph (via the daemon) to print
the changed files, blast radius, and risk score.

Listing needs a GitHub token (GH_TOKEN or GITHUB_TOKEN). The deep-dive also
needs a running daemon that tracks the repo.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runPRs,
}

func init() {
	prsCmd.Flags().StringVarP(&prsBase, "base", "b", "", "default base branch used to flag BASE_MISMATCH (default: the repo's default branch)")
	prsCmd.Flags().StringVar(&prsRepo, "repo", "", "repository path the forge / daemon must own (default: current directory)")
	prsCmd.Flags().StringVar(&prsFormat, "format", "text", "output format: text or json")
	prsCmd.Flags().BoolVar(&prsWorktrees, "worktrees", false, "annotate each PR whose head branch is checked out in a local worktree")
	rootCmd.AddCommand(prsCmd)
}

func runPRs(cmd *cobra.Command, args []string) error {
	repoPath := "."
	if prsRepo != "" {
		repoPath = prsRepo
	}

	// Deep-dive: `gortex prs <N>`.
	if len(args) == 1 {
		n, err := strconv.Atoi(strings.TrimSpace(args[0]))
		if err != nil || n <= 0 {
			return fmt.Errorf("invalid PR number %q", args[0])
		}
		return runPRDeepDive(cmd, repoPath, n)
	}

	// Dashboard: `gortex prs`.
	return runPRList(cmd, repoPath)
}

// runPRList prints the open-PR table (or its JSON form). A missing forge
// token is not an error: it prints an actionable GH_TOKEN hint and exits 0.
func runPRList(cmd *cobra.Command, repoPath string) error {
	ctx := context.Background()

	if !forgeAvailable(ctx) {
		_, _ = fmt.Fprintln(cmd.OutOrStdout(),
			"no GitHub token found — set GH_TOKEN (or GITHUB_TOKEN) to list pull requests")
		return nil
	}

	prs, err := forgeListPRs(ctx, repoPath, forge.ListOpts{
		State:        "open",
		Limit:        30,
		WithDecision: true,
		WithCI:       true,
	})
	if err != nil {
		return fmt.Errorf("listing pull requests: %w", err)
	}

	base := resolvePRBase(repoPath)

	var worktreeBranches map[string]bool
	if prsWorktrees {
		worktreeBranches = localWorktreeBranches(ctx, repoPath)
	}

	rows := classifyPRRows(prs, base)

	if prsFormat == "json" {
		return emitPRListJSON(cmd, rows)
	}
	emitPRListTable(cmd, rows, worktreeBranches)
	return nil
}

// prRow is the projection of a classified PR onto the documented wire shape.
type prRow struct {
	Number   int      `json:"number"`
	Title    string   `json:"title"`
	Author   string   `json:"author"`
	AgeDays  int      `json:"age_days"`
	CI       string   `json:"ci"`
	Review   string   `json:"review"`
	State    string   `json:"state"`
	Blockers []string `json:"blockers"`
	headRef  string
}

// classifyPRRows classifies every PR against the resolved default base.
func classifyPRRows(prs []forge.PR, base string) []prRow {
	rows := make([]prRow, 0, len(prs))
	for _, pr := range prs {
		st := forge.ClassifyStatus(pr, base)
		blockers := st.Blockers
		if blockers == nil {
			blockers = []string{}
		}
		rows = append(rows, prRow{
			Number:   pr.Number,
			Title:    pr.Title,
			Author:   pr.Author,
			AgeDays:  st.AgeDays,
			CI:       forge.RollupCI(pr),
			Review:   pr.ReviewDecision,
			State:    st.State,
			Blockers: blockers,
			headRef:  pr.HeadRef,
		})
	}
	return rows
}

// emitPRListJSON renders the documented {prs:[…]} shape.
func emitPRListJSON(cmd *cobra.Command, rows []prRow) error {
	enc := json.NewEncoder(cmd.OutOrStdout())
	enc.SetIndent("", "  ")
	return enc.Encode(map[string]any{"prs": rows})
}

// emitPRListTable renders the dashboard table. When worktreeBranches is
// non-nil a PR whose head branch is locally checked out is marked.
func emitPRListTable(cmd *cobra.Command, rows []prRow, worktreeBranches map[string]bool) {
	out := cmd.OutOrStdout()
	if len(rows) == 0 {
		_, _ = fmt.Fprintln(out, "No open pull requests.")
		return
	}
	tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	header := "#\tSTATE\tCI\tREVIEW\tAGE\tAUTHOR\tTITLE"
	if worktreeBranches != nil {
		header += "\tWORKTREE"
	}
	_, _ = fmt.Fprintln(tw, header)
	for _, r := range rows {
		review := r.Review
		if review == "" {
			review = "-"
		}
		line := fmt.Sprintf("%d\t%s\t%s\t%s\t%dd\t%s\t%s",
			r.Number, r.State, r.CI, review, r.AgeDays, r.Author, truncate(r.Title, 50))
		if worktreeBranches != nil {
			mark := ""
			if r.headRef != "" && worktreeBranches[r.headRef] {
				mark = "yes"
			}
			line += "\t" + mark
		}
		_, _ = fmt.Fprintln(tw, line)
	}
	_ = tw.Flush()
}

// runPRDeepDive fetches a PR's changed files from the forge (when a token is
// available) and runs the daemon's get_pr_impact tool, passing the file set
// so the daemon need not refetch. It prints the changed files, blast radius,
// and risk score.
func runPRDeepDive(cmd *cobra.Command, repoPath string, number int) error {
	ctx := context.Background()

	args := map[string]any{"number": number}
	if prsRepo != "" {
		args["repo"] = prsRepo
	}

	// Best-effort: pass the CLI-fetched file set so the daemon skips a
	// redundant forge fetch. When no token is resolvable we simply omit
	// `files` and let the daemon self-serve (or degrade with its own hint).
	if forgeAvailable(ctx) {
		files, err := forgePRFiles(ctx, repoPath, number)
		if err != nil {
			return fmt.Errorf("fetching PR #%d files: %w", number, err)
		}
		if encoded, merr := json.Marshal(files); merr == nil {
			args["files"] = string(encoded)
		}
	}

	raw, err := prsDaemonTool(repoPath, "get_pr_impact", args)
	if err != nil {
		return err
	}

	if prsFormat == "json" {
		return emitDaemonJSON(cmd, raw)
	}
	return printPRImpact(cmd, number, raw)
}

// prImpactPayload mirrors the get_pr_impact wire shape the deep-dive renders.
type prImpactPayload struct {
	Number           int      `json:"number"`
	Risk             string   `json:"risk"`
	Score            float64  `json:"score"`
	ReviewPriorities []struct {
		Axis   string  `json:"axis"`
		Score  float64 `json:"score"`
		Reason string  `json:"reason"`
	} `json:"review_priorities"`
	ChangedFiles   []string `json:"changed_files"`
	ChangedSymbols []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		Kind string `json:"kind"`
		File string `json:"file"`
	} `json:"changed_symbols"`
	Communities []string `json:"communities"`
	// degradation shape
	Error string `json:"error"`
	Hint  string `json:"hint"`
}

// printPRImpact renders the deep-dive: changed files, blast radius, and risk.
func printPRImpact(cmd *cobra.Command, number int, raw json.RawMessage) error {
	out := cmd.OutOrStdout()
	var p prImpactPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		// Unknown shape — fall back to pretty JSON rather than fail.
		return emitDaemonJSON(cmd, raw)
	}
	if p.Error != "" {
		_, _ = fmt.Fprintf(out, "PR #%d: %s", number, p.Error)
		if p.Hint != "" {
			_, _ = fmt.Fprintf(out, " — %s", p.Hint)
		}
		_, _ = fmt.Fprintln(out)
		return nil
	}

	_, _ = fmt.Fprintf(out, "PR #%d — risk %s (score %.1f)\n", p.Number, p.Risk, p.Score)

	_, _ = fmt.Fprintf(out, "\nChanged files (%d):\n", len(p.ChangedFiles))
	for _, f := range p.ChangedFiles {
		_, _ = fmt.Fprintf(out, "  %s\n", f)
	}

	_, _ = fmt.Fprintf(out, "\nBlast radius: %d changed symbol(s), %d communit(ies)\n",
		len(p.ChangedSymbols), len(p.Communities))
	for _, sym := range p.ChangedSymbols {
		_, _ = fmt.Fprintf(out, "  %-8s %s\n", sym.Kind, sym.ID)
	}

	if len(p.ReviewPriorities) > 0 {
		_, _ = fmt.Fprintln(out, "\nReview priorities:")
		for _, pr := range p.ReviewPriorities {
			_, _ = fmt.Fprintf(out, "  %-10s %5.1f  %s\n", pr.Axis, pr.Score, pr.Reason)
		}
	}
	return nil
}

// resolvePRBase resolves the default base branch used to flag BASE_MISMATCH:
// the explicit --base flag wins, otherwise the repo's default branch.
func resolvePRBase(repoPath string) string {
	if prsBase != "" {
		return prsBase
	}
	abs, err := filepath.Abs(repoPath)
	if err != nil {
		abs = repoPath
	}
	return churn.DefaultBranch(abs)
}

// localWorktreeBranches returns the set of branch names currently checked out
// in a local worktree of the repo, so the dashboard can mark a PR whose head
// is already on disk. A failure to enumerate worktrees yields an empty set.
func localWorktreeBranches(ctx context.Context, repoPath string) map[string]bool {
	abs, err := filepath.Abs(repoPath)
	if err != nil {
		abs = repoPath
	}
	// Anchor to the main checkout so a query from inside a linked worktree
	// still enumerates every sibling worktree.
	if info := indexer.ResolveWorktree(abs); info.MainRepoPath != "" {
		abs = info.MainRepoPath
	}
	branches := map[string]bool{}
	entries, err := forge.LocalWorktrees(ctx, abs)
	if err != nil {
		return branches
	}
	for _, e := range entries {
		if e.Branch != "" {
			branches[e.Branch] = true
		}
	}
	return branches
}
