package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/zzet/gortex/internal/coverage"
	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/progress"
)

var enrichCmd = &cobra.Command{
	Use:   "enrich",
	Short: "Run one-shot enrichments (churn, blame, coverage, releases, cochange) via the running daemon",
	Long: `Enrich stamps additional metadata onto the daemon's graph from
external data sources — git blame for authorship, git history for churn
and co-change, git tags for release timelines, and Go cover profiles for
test coverage.

Every enrichment is forwarded to the running daemon, which owns the warm
graph and its on-disk store write lock. The daemon runs the enricher
in-process against that graph so the persisted metadata is immediately
queryable by the analyze / get_churn_rate / coverage tools.

A daemon must be running. If none is, the command exits with an error
rather than building a throwaway in-memory graph that nothing would
read — start one with ` + "`gortex daemon start`" + ` and re-run.`,
}

var enrichReleasesBranch string

// errNoDaemon is the single clean error every enrich subcommand returns
// when no daemon is reachable. The enrichers only make sense against the
// daemon's warm, prefix-stamped graph; a standalone in-memory pass would
// be discarded and a direct on-disk write would race the daemon's writer.
var errNoDaemon = errors.New("enrich requires a running daemon; start it with `gortex daemon start`")

var enrichBlameCmd = &cobra.Command{
	Use:   "blame [path]",
	Short: "Stamp meta.last_authored on every symbol via git blame",
	Args:  cobra.MaximumNArgs(1),
	RunE:  runEnrichBlame,
}

var enrichCoverageCmd = &cobra.Command{
	Use:   "coverage <profile> [path]",
	Short: "Stamp meta.coverage_pct on every symbol from a Go cover profile",
	Args:  cobra.RangeArgs(1, 2),
	RunE:  runEnrichCoverage,
}

var enrichReleasesCmd = &cobra.Command{
	Use:   "releases [path]",
	Short: "Stamp meta.added_in on every file from git tag history",
	Args:  cobra.MaximumNArgs(1),
	RunE:  runEnrichReleases,
}

var enrichCochangeCmd = &cobra.Command{
	Use:   "cochange [path]",
	Short: "Add co_change edges between files that git history shows change together",
	Args:  cobra.MaximumNArgs(1),
	RunE:  runEnrichCochange,
}

var (
	enrichAllBlame    bool
	enrichAllReleases bool
	enrichAllCochange bool
	enrichAllChurn    bool
	enrichAllProfile  string
)

var enrichAllCmd = &cobra.Command{
	Use:   "all [path]",
	Short: "Run every enrichment against the daemon's graph in one invocation",
	Long: `Combined enrichment that runs the requested enrichers against the
daemon's graph via successive control calls.

By default runs churn, blame, releases, and co-change (all git-only, no
extra data needed). Pass --coverage <profile> to also project a Go cover
profile. Each enrichment is independently toggleable via the
--no-churn / --no-blame / --no-releases / --no-cochange flags.

Like every enrich subcommand, this requires a running daemon.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runEnrichAll,
}

func init() {
	enrichReleasesCmd.Flags().StringVar(&enrichReleasesBranch, "branch", "",
		"restrict to tags reachable from this branch (default: resolve origin/main/master). Empty means every tag in the repo")
	enrichAllCmd.Flags().BoolVar(&enrichAllChurn, "churn", true,
		"run churn enrichment (default: on)")
	enrichAllCmd.Flags().BoolVar(&enrichAllBlame, "blame", true,
		"run blame enrichment (default: on)")
	enrichAllCmd.Flags().BoolVar(&enrichAllReleases, "releases", true,
		"run releases enrichment (default: on)")
	enrichAllCmd.Flags().BoolVar(&enrichAllCochange, "cochange", true,
		"run co-change enrichment (default: on)")
	enrichAllCmd.Flags().StringVar(&enrichAllProfile, "coverage", "",
		"path to a Go cover.out profile — coverage enrichment is skipped when empty")
	enrichCmd.AddCommand(enrichBlameCmd)
	enrichCmd.AddCommand(enrichCoverageCmd)
	enrichCmd.AddCommand(enrichReleasesCmd)
	enrichCmd.AddCommand(enrichCochangeCmd)
	enrichCmd.AddCommand(enrichAllCmd)
	rootCmd.AddCommand(enrichCmd)
}

// enrichAbsPath resolves the optional [path] argument to an absolute
// path. Empty args default to the current directory; the abs path is the
// repo scope handed to the daemon (matched against tracked prefixes /
// roots, or "" for "every tracked repo").
func enrichAbsPath(args []string) (string, error) {
	path := "."
	if len(args) >= 1 {
		path = args[0]
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("abs path %q: %w", path, err)
	}
	return abs, nil
}

// dialEnrichDaemon opens a control connection to the running daemon for
// the given client name. Callers must have already checked
// daemon.IsRunning(); a dial failure here means the socket was present
// but unusable (a dying daemon) — surfaced as a clear error.
func dialEnrichDaemon(clientName string) (*daemon.Client, error) {
	c, err := daemon.Dial(daemon.Handshake{Mode: daemon.ModeControl, ClientName: clientName})
	if err != nil {
		if errors.Is(err, daemon.ErrDaemonUnavailable) {
			return nil, fmt.Errorf("daemon socket detected but dial failed; restart it with `gortex daemon restart`")
		}
		return nil, fmt.Errorf("dial daemon: %w", err)
	}
	return c, nil
}

// controlEnrich sends one control request on c, validates the daemon
// accepted it, and decodes the typed result into out (which must be a
// pointer). Centralises the OK / error-code handling every forwarder
// repeats.
func controlEnrich(c *daemon.Client, kind string, params, out any) error {
	resp, err := c.Control(kind, params)
	if err != nil {
		return fmt.Errorf("control %s: %w", kind, err)
	}
	if !resp.OK {
		return fmt.Errorf("daemon rejected %s [%s]: %s", kind, resp.ErrorCode, resp.ErrorMsg)
	}
	if out != nil && len(resp.Result) > 0 {
		if err := json.Unmarshal(resp.Result, out); err != nil {
			return fmt.Errorf("parse daemon %s response: %w", kind, err)
		}
	}
	return nil
}

func runEnrichBlame(cmd *cobra.Command, args []string) error {
	abs, err := enrichAbsPath(args)
	if err != nil {
		return err
	}
	if !daemon.IsRunning() {
		return errNoDaemon
	}
	c, err := dialEnrichDaemon("cli-enrich-blame")
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()

	var out daemon.EnrichBlameResult
	if err := controlEnrich(c, daemon.ControlEnrichBlame, daemon.EnrichBlameParams{Path: abs}, &out); err != nil {
		return err
	}
	sp := newCLISpinner(cmd, "Enriched via daemon")
	sp.Set("", fmt.Sprintf("%d nodes stamped", out.Nodes))
	sp.Done()
	return printEnrichResult(map[string]any{
		"enriched":    out.Nodes,
		"duration_ms": out.DurationMS,
		"path":        abs,
		"mode":        "daemon",
	})
}

func runEnrichCoverage(cmd *cobra.Command, args []string) error {
	profilePath := args[0]
	abs, err := enrichAbsPath(args[1:])
	if err != nil {
		return err
	}
	// Parse the profile CLI-side: the path is relative to the caller's
	// cwd, not the daemon's, so the daemon can't read it. We hand the
	// daemon the parsed segments instead.
	segments, err := coverage.ParseFile(profilePath)
	if err != nil {
		return fmt.Errorf("read profile: %w", err)
	}
	if !daemon.IsRunning() {
		return errNoDaemon
	}
	c, err := dialEnrichDaemon("cli-enrich-coverage")
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()

	wire := make([]daemon.EnrichCoverageSegment, len(segments))
	for i, s := range segments {
		wire[i] = daemon.EnrichCoverageSegment{
			File:      s.File,
			StartLine: s.StartLine,
			EndLine:   s.EndLine,
			NumStmt:   s.NumStmt,
			Count:     s.Count,
		}
	}

	var out daemon.EnrichCoverageResult
	if err := controlEnrich(c, daemon.ControlEnrichCoverage, daemon.EnrichCoverageParams{Path: abs, Segments: wire}, &out); err != nil {
		return err
	}
	sp := newCLISpinner(cmd, "Enriched via daemon")
	sp.Set("", fmt.Sprintf("%d symbols · %d segments", out.Symbols, out.Segments))
	sp.Done()
	return printEnrichResult(map[string]any{
		"enriched":    out.Symbols,
		"segments":    out.Segments,
		"profile":     profilePath,
		"duration_ms": out.DurationMS,
		"path":        abs,
		"mode":        "daemon",
	})
}

func runEnrichReleases(cmd *cobra.Command, args []string) error {
	abs, err := enrichAbsPath(args)
	if err != nil {
		return err
	}
	if !daemon.IsRunning() {
		return errNoDaemon
	}
	c, err := dialEnrichDaemon("cli-enrich-releases")
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()

	var out daemon.EnrichReleasesResult
	if err := controlEnrich(c, daemon.ControlEnrichReleases, daemon.EnrichReleasesParams{Path: abs, Branch: enrichReleasesBranch}, &out); err != nil {
		return err
	}
	sp := newCLISpinner(cmd, "Enriched via daemon")
	sp.Set("", fmt.Sprintf("%d files · %s", out.Files, out.Branch))
	sp.Done()
	return printEnrichResult(map[string]any{
		"enriched":    out.Files,
		"branch":      out.Branch,
		"duration_ms": out.DurationMS,
		"path":        abs,
		"mode":        "daemon",
	})
}

func runEnrichCochange(cmd *cobra.Command, args []string) error {
	abs, err := enrichAbsPath(args)
	if err != nil {
		return err
	}
	if !daemon.IsRunning() {
		return errNoDaemon
	}
	c, err := dialEnrichDaemon("cli-enrich-cochange")
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()

	var out daemon.EnrichCochangeResult
	if err := controlEnrich(c, daemon.ControlEnrichCochange, daemon.EnrichCochangeParams{Path: abs}, &out); err != nil {
		return err
	}
	sp := newCLISpinner(cmd, "Enriched via daemon")
	sp.Set("", fmt.Sprintf("%d edges added", out.Edges))
	sp.Done()
	return printEnrichResult(map[string]any{
		"enriched":    out.Edges,
		"duration_ms": out.DurationMS,
		"path":        abs,
		"mode":        "daemon",
	})
}

func runEnrichAll(cmd *cobra.Command, args []string) error {
	abs, err := enrichAbsPath(args)
	if err != nil {
		return err
	}
	// Parse the coverage profile (if any) up front so a bad path fails
	// before we touch the daemon.
	var covSegments []daemon.EnrichCoverageSegment
	if enrichAllProfile != "" {
		segments, err := coverage.ParseFile(enrichAllProfile)
		if err != nil {
			return fmt.Errorf("read profile: %w", err)
		}
		covSegments = make([]daemon.EnrichCoverageSegment, len(segments))
		for i, s := range segments {
			covSegments[i] = daemon.EnrichCoverageSegment{
				File:      s.File,
				StartLine: s.StartLine,
				EndLine:   s.EndLine,
				NumStmt:   s.NumStmt,
				Count:     s.Count,
			}
		}
	}
	if !daemon.IsRunning() {
		return errNoDaemon
	}
	c, err := dialEnrichDaemon("cli-enrich-all")
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()

	result := map[string]any{
		"path": abs,
		"mode": "daemon",
	}

	if enrichAllChurn {
		sp := newCLISpinner(cmd, "Stamping churn")
		var out daemon.EnrichChurnResult
		if err := controlEnrich(c, daemon.ControlEnrichChurn, daemon.EnrichChurnParams{Path: abs}, &out); err != nil {
			sp.Fail(err)
			return err
		}
		sp.Set("", fmt.Sprintf("%d files · %d symbols", out.Files, out.Symbols))
		sp.Done()
		result["churn_files"] = out.Files
		result["churn_symbols"] = out.Symbols
		result["churn_branch"] = out.Branch
	}
	if enrichAllBlame {
		sp := newCLISpinner(cmd, "Stamping blame")
		var out daemon.EnrichBlameResult
		if err := controlEnrich(c, daemon.ControlEnrichBlame, daemon.EnrichBlameParams{Path: abs}, &out); err != nil {
			sp.Fail(err)
			return err
		}
		sp.Set("", fmt.Sprintf("%d nodes stamped", out.Nodes))
		sp.Done()
		result["blame_enriched"] = out.Nodes
	}
	if enrichAllReleases {
		sp := newCLISpinner(cmd, "Stamping releases")
		var out daemon.EnrichReleasesResult
		if err := controlEnrich(c, daemon.ControlEnrichReleases, daemon.EnrichReleasesParams{Path: abs}, &out); err != nil {
			sp.Fail(err)
			return err
		}
		sp.Set("", fmt.Sprintf("%d files stamped", out.Files))
		sp.Done()
		result["releases_enriched"] = out.Files
	}
	if enrichAllCochange {
		sp := newCLISpinner(cmd, "Mining co-change")
		var out daemon.EnrichCochangeResult
		if err := controlEnrich(c, daemon.ControlEnrichCochange, daemon.EnrichCochangeParams{Path: abs}, &out); err != nil {
			sp.Fail(err)
			return err
		}
		sp.Set("", fmt.Sprintf("%d edges added", out.Edges))
		sp.Done()
		result["cochange_edges"] = out.Edges
	}
	if len(covSegments) > 0 {
		sp := newCLISpinner(cmd, "Stamping coverage")
		sp.Set("", enrichAllProfile)
		var out daemon.EnrichCoverageResult
		if err := controlEnrich(c, daemon.ControlEnrichCoverage, daemon.EnrichCoverageParams{Path: abs, Segments: covSegments}, &out); err != nil {
			sp.Fail(err)
			return err
		}
		sp.Set("", fmt.Sprintf("%d symbols · %d segments", out.Symbols, out.Segments))
		sp.Done()
		result["coverage_enriched"] = out.Symbols
		result["coverage_segments"] = out.Segments
	}
	return printEnrichResult(result)
}

// printEnrichResult emits the enrichment summary as JSON when stdout
// is captured by a script and as a one-line human-readable text
// when invoked interactively. On a terminal we keep stdout quiet — the
// spinner already showed the per-pass count — and just caption the path /
// profile. On a pipe / redirect we still emit JSON for scripts.
func printEnrichResult(payload map[string]any) error {
	if progress.IsTTY(os.Stdout) {
		if v, ok := payload["path"]; ok {
			_, _ = fmt.Fprintln(os.Stdout, "  "+progress.Caption("path: "+fmt.Sprint(v)))
		}
		if v, ok := payload["profile"]; ok {
			_, _ = fmt.Fprintln(os.Stdout, "  "+progress.Caption("profile: "+fmt.Sprint(v)))
		}
		return nil
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(data))
	return nil
}
