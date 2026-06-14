package config

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// loadRepoGitignore reads the `.gitignore` file at the repo root and
// returns its entries as gitignore-syntax patterns ready to feed into
// the excludes matcher. Blank lines and `#` comments are stripped.
//
// We deliberately do NOT walk the per-directory hierarchy here:
//   - The repo-root file already covers ~95% of the value (build
//     output, dependency caches, generated code).
//   - Per-directory `.gitignore` semantics (path anchoring relative to
//     the file's location) would require either nested matchers or a
//     pattern-rewrite pass; both add complexity for marginal coverage.
//   - Users with sub-directory ignores can list them in `.gortex.yaml`
//     `excludes` until/unless we add hierarchy support.
//
// Returns nil when the file is absent or unreadable — gitignore reading
// is a convenience, never a hard requirement, so a missing or
// permission-denied file silently no-ops.
func loadRepoGitignore(repoPath string) []string {
	if repoPath == "" {
		return nil
	}
	f, err := os.Open(filepath.Join(repoPath, ".gitignore"))
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()

	var patterns []string
	scanner := bufio.NewScanner(f)
	// Tolerate pathologically long lines (a DLP-encrypted or otherwise
	// non-text .gitignore) instead of aborting the whole read on the
	// default 64 KiB scanner limit.
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// A non-UTF-8 line is not a valid gitignore pattern — feeding it to
		// the matcher would mis-scan. Skip it defensively so a corrupt or
		// encrypted .gitignore can never poison exclusion.
		if !utf8.ValidString(line) {
			continue
		}
		patterns = append(patterns, line)
	}
	// A read error (a still-too-long line, a transient I/O fault) must not
	// discard the patterns already read — gitignore loading is best-effort,
	// never a hard failure.
	return patterns
}
