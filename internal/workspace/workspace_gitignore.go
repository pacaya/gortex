package workspace

import (
	"os"
	"path/filepath"
	"strings"
)

// DefaultIndexGitignore is written into <IndexDir>/.gitignore so a project's
// local Gortex state (quarantine, merkle cache, sqlite sidecar, etc.) is never
// accidentally committed. The bare `*` ignores everything under the directory.
const DefaultIndexGitignore = "# Gortex-managed: local index state, do not commit\n*\n"

// knownIndexGitignoreDefaults are prior auto-written contents that
// EnsureIndexDirGitignore is allowed to heal up to the current default. A file
// whose normalized content matches none of these (and isn't already current) is
// treated as user-customized and left untouched.
var knownIndexGitignoreDefaults = []string{
	"*",
}

// EnsureIndexDirGitignore writes DefaultIndexGitignore into <indexDir>/.gitignore
// when it is absent, or heals it when it still holds a known prior default. A
// user-customized .gitignore is left untouched. It returns whether it wrote.
func EnsureIndexDirGitignore(indexDir string) (wrote bool, err error) {
	gi := filepath.Join(indexDir, ".gitignore")
	existing, readErr := os.ReadFile(gi)
	switch {
	case readErr == nil:
		cur := normalizeGitignore(string(existing))
		if cur == normalizeGitignore(DefaultIndexGitignore) {
			return false, nil // already current
		}
		if !isLegacyIndexGitignore(cur) {
			return false, nil // user-customized: leave it
		}
		// stale default → heal below
	case os.IsNotExist(readErr):
		// absent → write below
	default:
		return false, readErr
	}

	if err := os.MkdirAll(indexDir, 0o755); err != nil {
		return false, err
	}
	if err := os.WriteFile(gi, []byte(DefaultIndexGitignore), 0o644); err != nil {
		return false, err
	}
	return true, nil
}

func normalizeGitignore(s string) string {
	return strings.TrimSpace(s)
}

func isLegacyIndexGitignore(normalized string) bool {
	for _, d := range knownIndexGitignoreDefaults {
		if normalizeGitignore(d) == normalized {
			return true
		}
	}
	return false
}
