package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/jedib0t/go-pretty/v6/text"
	"github.com/spf13/cobra"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/persistence"
)

var reposJSON bool

// reposCacheDir is the persistence-store directory `gortex repos`
// inspects for index freshness. Empty resolves to the default
// (~/.cache/gortex/) — the same slot `gortex server` / `gortex mcp`
// persist to. Overridable so tests can point at a temp store.
var reposCacheDir string

var reposCmd = &cobra.Command{
	Use:   "repos",
	Short: "List every tracked repository with its git head and index freshness",
	Long: `Lists the repositories registered in the global config
(~/.config/gortex/config.yaml).

For each repo the command reports the current git HEAD commit and an
index-freshness indicator: when the persisted index was last built and
whether that index still matches HEAD. A repo is "stale" when HEAD has
moved past the commit the cached index was built from, or when no index
has been persisted yet.

The default output is a table; --json emits the same data as a JSON
array suitable for scripting.`,
	RunE: runRepos,
}

func init() {
	reposCmd.Flags().BoolVar(&reposJSON, "json", false, "emit machine-readable JSON instead of a table")
	rootCmd.AddCommand(reposCmd)
}

// repoStatus is one repository's entry in the `gortex repos` output —
// identity plus the git head commit and index-freshness facts. It is
// the JSON shape emitted under --json; the table renderer projects the
// same struct into columns.
type repoStatus struct {
	// Name is the repo's configured name, falling back to the path
	// basename when the global config declares no explicit name.
	Name string `json:"name"`
	// Path is the absolute on-disk path of the repository.
	Path string `json:"path"`
	// Workspace is the workspace slug the repo's nodes are keyed on
	// (the global-config override; empty when none is declared).
	Workspace string `json:"workspace,omitempty"`
	// HeadCommit is the current git HEAD commit SHA, or empty when
	// the path is not a git repository / git is unavailable.
	HeadCommit string `json:"head_commit"`
	// Branch is the current git branch, empty for a detached HEAD.
	Branch string `json:"branch,omitempty"`
	// IndexedCommit is the commit SHA the persisted index was built
	// from. Empty when no index snapshot exists yet.
	IndexedCommit string `json:"indexed_commit,omitempty"`
	// LastIndexed is the timestamp the persisted index was built.
	// Nil (omitted from JSON) when the repo has never been indexed.
	LastIndexed *time.Time `json:"last_indexed,omitempty"`
	// Stale is true when the persisted index does not match the
	// current HEAD — either HEAD moved past IndexedCommit or no
	// index has been persisted at all.
	Stale bool `json:"stale"`
	// Indexed is true when a persisted index snapshot was found for
	// the repo's current branch slot.
	Indexed bool `json:"indexed"`
}

func runRepos(cmd *cobra.Command, _ []string) error {
	repos, err := loadGlobalRepos()
	if err != nil {
		return err
	}

	// The persistence store is read-only here — we only inspect what
	// `gortex server` / `gortex mcp` already persisted. An empty
	// cache dir resolves to the default (~/.cache/gortex/), the same
	// slot those commands write to.
	store, err := persistence.NewFileStore(reposCacheDir, version)
	if err != nil {
		return fmt.Errorf("open persistence store: %w", err)
	}
	defer func() { _ = store.Close() }()

	entries := make([]repoStatus, 0, len(repos))
	for _, r := range repos {
		entries = append(entries, describeRepo(store, r))
	}
	// Stable order regardless of config-file ordering so scripted
	// diffs and the table stay deterministic.
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Name != entries[j].Name {
			return entries[i].Name < entries[j].Name
		}
		return entries[i].Path < entries[j].Path
	})

	if reposJSON {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(entries)
	}

	return renderReposTable(cmd, entries)
}

// describeRepo resolves one RepoEntry into a repoStatus by reading the
// repo's current git HEAD and looking up the persisted index snapshot
// for its branch slot. Snapshots are keyed by (repo, branch), so a slot
// found for the current branch carries the commit it was last indexed
// at — staleness is HEAD having advanced past that commit.
func describeRepo(store persistence.Store, r config.RepoEntry) repoStatus {
	head := gitCommitHash(r.Path)
	branch := gitBranch(r.Path)

	entry := repoStatus{
		Name:       repoLabel(r),
		Path:       r.Path,
		Workspace:  r.Workspace,
		HeadCommit: head,
		Branch:     branch,
		// Default to stale; cleared below only when a persisted
		// index is found whose commit matches HEAD.
		Stale: true,
	}

	// Snapshots are keyed under the canonical (main) repo path so
	// every worktree of a repo shares a base slot — match the key
	// `gortex server` writes with.
	repoKey := canonicalRepo(r.Path)
	snap, loadErr := store.Load(repoKey, branch, head)
	if loadErr != nil || snap == nil {
		// ErrNotFound (or any read error) — treat as never indexed.
		return entry
	}

	entry.Indexed = true
	entry.IndexedCommit = snap.CommitHash
	indexedAt := snap.IndexedAt
	entry.LastIndexed = &indexedAt
	// Fresh only when the persisted index was built from the exact
	// commit HEAD currently points at. An empty HeadCommit (not a
	// git repo) can never be fresh.
	entry.Stale = head == "" || snap.CommitHash != head
	return entry
}

// renderReposTable prints the repo list as an ASCII table — the default
// human-readable form. Columns mirror the repoStatus JSON fields.
func renderReposTable(cmd *cobra.Command, entries []repoStatus) error {
	if len(entries) == 0 {
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "(no tracked repos)")
		return nil
	}

	t := table.NewWriter()
	t.SetOutputMirror(cmd.OutOrStdout())
	t.SetStyle(table.StyleLight)
	t.AppendHeader(table.Row{"repo", "head", "indexed", "last indexed", "freshness", "path"})
	t.SetColumnConfigs([]table.ColumnConfig{
		{Number: 1, Align: text.AlignLeft},
		{Number: 2, Align: text.AlignLeft},
		{Number: 3, Align: text.AlignLeft},
		{Number: 4, Align: text.AlignLeft},
		{Number: 5, Align: text.AlignLeft},
		{Number: 6, Align: text.AlignLeft},
	})

	for _, e := range entries {
		t.AppendRow(table.Row{
			e.Name,
			shortSHA(e.HeadCommit),
			shortSHA(e.IndexedCommit),
			lastIndexedCell(e),
			freshnessCell(e),
			e.Path,
		})
	}
	t.Render()
	return nil
}

// shortSHA abbreviates a 40-char git SHA to its 12-char prefix for the
// table — the full hash stays in the JSON output. Empty in, empty out.
func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	if sha == "" {
		return "(none)"
	}
	return sha
}

// lastIndexedCell renders the last-indexed timestamp for the table, or
// a placeholder when the repo has never been indexed.
func lastIndexedCell(e repoStatus) string {
	if e.LastIndexed == nil {
		return "(never)"
	}
	return e.LastIndexed.Local().Format("2006-01-02 15:04:05")
}

// freshnessCell renders the staleness indicator for the table.
func freshnessCell(e repoStatus) string {
	switch {
	case !e.Indexed:
		return "not indexed"
	case e.Stale:
		return "stale"
	default:
		return "fresh"
	}
}
