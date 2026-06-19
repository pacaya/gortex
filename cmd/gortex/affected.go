package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

// exitNoAffected is the affected verb's CI sentinel: the change touched nothing
// the test graph covers, so a selective-test runner can skip the suite. It is
// distinct from the generic error exit (1) so a CI script can tell "no tests to
// run" apart from "the command failed".
const exitNoAffected = 3

var (
	affectedStdin bool
	affectedJSON  bool
	affectedQuiet bool
	affectedIndex string
)

var affectedCmd = &cobra.Command{
	Use:   "affected [files...]",
	Short: "List the test files affected by a set of changed files",
	Long: `Resolve which test files cover the symbols in a set of changed files, by
walking the graph's test edges on the daemon that tracks the repo.

Built for CI / git-hook piping: pass changed paths as args or via --stdin
(e.g. ` + "`git diff --name-only | gortex affected --stdin --quiet`" + `), and
branch on the exit code:

  exit 0  one or more test files are affected — run them
  exit 3  nothing the test graph covers changed — skip the suite
  exit 1  the command failed (no daemon, repo not tracked, …)`,
	SilenceErrors: true,
	SilenceUsage:  true,
	RunE:          runAffected,
}

func init() {
	affectedCmd.Flags().BoolVar(&affectedStdin, "stdin", false, "read changed file paths from stdin (whitespace/newline separated)")
	affectedCmd.Flags().BoolVar(&affectedJSON, "json", false, "emit the affected test files as JSON")
	affectedCmd.Flags().BoolVarP(&affectedQuiet, "quiet", "q", false, "print nothing; communicate only via the exit code")
	affectedCmd.Flags().StringVar(&affectedIndex, "index", "", "repository path the daemon tracks (default: current directory)")
	rootCmd.AddCommand(affectedCmd)
}

func runAffected(cmd *cobra.Command, args []string) error {
	changed := append([]string(nil), args...)
	if affectedStdin {
		changed = append(changed, parseAffectedStdin(cmd.InOrStdin())...)
	}
	changed = dedupeNonEmpty(changed)
	if len(changed) == 0 {
		return fmt.Errorf("affected: no changed files given (pass paths as args or --stdin)")
	}

	repoPath := affectedIndex
	if repoPath == "" {
		repoPath = "."
	}

	targets, err := resolveAffectedTests(repoPath, changed)
	if err != nil {
		return err
	}

	if !affectedQuiet {
		if affectedJSON {
			_ = json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"affected_tests": targets})
		} else {
			for _, t := range targets {
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), t)
			}
		}
	}

	if code := affectedExitCode(len(targets)); code != 0 {
		return &exitCodeError{code: code}
	}
	return nil
}

// affectedExitCode maps an affected-test count to the CI sentinel exit code:
// 0 when something is affected (run the tests), exitNoAffected when nothing is.
func affectedExitCode(targetCount int) int {
	if targetCount > 0 {
		return 0
	}
	return exitNoAffected
}

// parseAffectedStdin reads whitespace/newline-separated file paths from r. It
// tolerates `git diff --name-only` output (one path per line) and a space-
// separated list on a single line equally.
func parseAffectedStdin(r io.Reader) []string {
	var out []string
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		out = append(out, strings.Fields(sc.Text())...)
	}
	return out
}

// dedupeNonEmpty trims, drops blanks, and dedupes while preserving order.
func dedupeNonEmpty(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// resolveAffectedTests maps changed files to the test files that cover them: it
// asks the daemon for each changed file's symbol IDs (get_file_summary), then
// resolves the covering tests in one get_test_targets call. The returned test
// file paths are unique and sorted.
func resolveAffectedTests(repoPath string, changedFiles []string) ([]string, error) {
	idSet := make(map[string]bool)
	for _, f := range changedFiles {
		raw, err := requireDaemonTool(repoPath, "get_file_summary", map[string]any{"file_path": f})
		if err != nil {
			return nil, err
		}
		for _, id := range collectSymbolIDs(raw) {
			idSet[id] = true
		}
	}
	if len(idSet) == 0 {
		return nil, nil // no indexed symbols in the changed files → nothing affected
	}
	ids := make([]string, 0, len(idSet))
	for id := range idSet {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	raw, err := requireDaemonTool(repoPath, "get_test_targets", map[string]any{"ids": strings.Join(ids, ",")})
	if err != nil {
		return nil, err
	}
	return parseTestTargetFiles(raw), nil
}

// parseTestTargetFiles extracts the unique, sorted test file paths from a
// get_test_targets response (its `test_targets[].file` list).
func parseTestTargetFiles(raw []byte) []string {
	var resp struct {
		TestTargets []struct {
			File string `json:"file"`
		} `json:"test_targets"`
	}
	if json.Unmarshal(raw, &resp) != nil {
		return nil
	}
	seen := make(map[string]bool)
	var files []string
	for _, t := range resp.TestTargets {
		if t.File != "" && !seen[t.File] {
			seen[t.File] = true
			files = append(files, t.File)
		}
	}
	sort.Strings(files)
	return files
}

// collectSymbolIDs walks a JSON document and gathers the string values under
// any "id" key that have the symbol-id shape (contain "::"), deduped. Tolerant
// of the differing get_file_summary shapes across versions.
func collectSymbolIDs(raw []byte) []string {
	var doc any
	if json.Unmarshal(raw, &doc) != nil {
		return nil
	}
	seen := make(map[string]bool)
	var out []string
	var walk func(any)
	walk = func(n any) {
		switch t := n.(type) {
		case map[string]any:
			for k, v := range t {
				if k == "id" {
					if sv, ok := v.(string); ok && strings.Contains(sv, "::") && !seen[sv] {
						seen[sv] = true
						out = append(out, sv)
						continue
					}
				}
				walk(v)
			}
		case []any:
			for _, e := range t {
				walk(e)
			}
		}
	}
	walk(doc)
	return out
}
