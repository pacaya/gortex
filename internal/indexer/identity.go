package indexer

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/zzet/gortex/internal/config"
)

// RepoIdentity holds the canonical identity of a repository.
type RepoIdentity struct {
	RemoteURL   string // normalized remote origin URL (empty if no remote)
	CanonicalID string // normalized form: "github.com/owner/repo"
	FilePath    string // absolute filesystem path
	RepoPrefix  string // derived short name
}

// sshURLRe matches SSH-style Git URLs like git@github.com:owner/repo.git
var sshURLRe = regexp.MustCompile(`^[\w.-]+@([\w.-]+):(.+)$`)

// DetectIdentity reads the git remote origin URL for the given repo path,
// falling back to the filesystem path if no remote is configured.
func DetectIdentity(repoPath string) (*RepoIdentity, error) {
	absPath, err := filepath.Abs(repoPath)
	if err != nil {
		return nil, fmt.Errorf("resolving path %s: %w", repoPath, err)
	}

	identity := &RepoIdentity{
		FilePath: absPath,
	}

	// Try to read git remote origin URL.
	rawURL, err := gitRemoteOrigin(absPath)
	if err == nil && rawURL != "" {
		normalized := NormalizeRemoteURL(rawURL)
		identity.RemoteURL = normalized
		identity.CanonicalID = normalized
		identity.RepoPrefix = lastPathComponent(normalized)
	} else {
		// Fall back to filesystem path.
		identity.CanonicalID = absPath
		identity.RepoPrefix = filepath.Base(absPath)
	}

	return identity, nil
}

// gitRemoteOrigin runs `git -C <path> remote get-url origin` and returns the URL.
func gitRemoteOrigin(repoPath string) (string, error) {
	cmd := exec.Command("git", "-C", repoPath, "remote", "get-url", "origin")
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// NormalizeRemoteURL normalizes a Git remote URL to a canonical form.
// It handles:
//   - Stripping .git suffix
//   - Converting SSH (git@github.com:owner/repo) to canonical form (github.com/owner/repo)
//   - Lowercasing the host component
//   - Stripping protocol prefix (https://, ssh://, http://)
func NormalizeRemoteURL(rawURL string) string {
	if rawURL == "" {
		return ""
	}

	url := strings.TrimSpace(rawURL)

	// Handle SSH URLs: git@host:owner/repo.git → host/owner/repo
	if m := sshURLRe.FindStringSubmatch(url); m != nil {
		host := strings.ToLower(m[1])
		path := m[2]
		path = strings.TrimSuffix(path, ".git")
		return host + "/" + path
	}

	// Handle ssh:// protocol: ssh://git@host/owner/repo.git → host/owner/repo
	if strings.HasPrefix(url, "ssh://") {
		url = strings.TrimPrefix(url, "ssh://")
		// Strip user@ prefix if present.
		if idx := strings.Index(url, "@"); idx != -1 {
			url = url[idx+1:]
		}
		url = strings.TrimSuffix(url, ".git")
		url = lowercaseHost(url)
		return url
	}

	// Handle https:// and http:// protocols.
	for _, prefix := range []string{"https://", "http://"} {
		if strings.HasPrefix(url, prefix) {
			url = strings.TrimPrefix(url, prefix)
			// Strip user@ prefix if present (e.g., https://user@host/...).
			if idx := strings.Index(url, "@"); idx != -1 {
				slashIdx := strings.Index(url, "/")
				if slashIdx == -1 || idx < slashIdx {
					url = url[idx+1:]
				}
			}
			url = strings.TrimSuffix(url, ".git")
			url = lowercaseHost(url)
			return url
		}
	}

	// Unrecognized format — return as-is (best effort).
	return strings.TrimSuffix(url, ".git")
}

// DeduplicateRepos returns a deduplicated list of repo entries (last occurrence wins)
// and warning messages for any duplicates detected by canonical identity.
func DeduplicateRepos(entries []config.RepoEntry) ([]config.RepoEntry, []string) {
	if len(entries) == 0 {
		return entries, nil
	}

	// Build canonical identity for each entry.
	canonicals := make([]string, len(entries))
	for i, e := range entries {
		absPath, err := filepath.Abs(e.Path)
		if err != nil {
			absPath = e.Path
		}

		// Try to detect identity via git remote.
		rawURL, err := gitRemoteOrigin(absPath)
		if err == nil && rawURL != "" {
			canonicals[i] = NormalizeRemoteURL(rawURL)
		} else {
			canonicals[i] = absPath
		}
	}

	// Track last occurrence of each canonical identity.
	// Iterate in reverse so last occurrence wins.
	seen := make(map[string]int)       // canonical → last index
	duplicates := make(map[string]bool) // canonical → has duplicates
	var warnings []string

	for i := len(entries) - 1; i >= 0; i-- {
		canon := canonicals[i]
		if _, exists := seen[canon]; exists {
			duplicates[canon] = true
		} else {
			seen[canon] = i
		}
	}

	// Generate warnings for duplicates.
	for i, e := range entries {
		canon := canonicals[i]
		if duplicates[canon] && seen[canon] != i {
			warnings = append(warnings, fmt.Sprintf(
				"duplicate repository identity %q: %s (superseded by later entry)",
				canon, e.Path))
		}
	}

	// Build deduplicated list preserving order of last occurrences.
	kept := make(map[string]bool)
	var result []config.RepoEntry
	for i := len(entries) - 1; i >= 0; i-- {
		canon := canonicals[i]
		if !kept[canon] {
			kept[canon] = true
			result = append(result, entries[i])
		}
	}

	// Reverse to restore original order.
	for i, j := 0, len(result)-1; i < j; i, j = i+1, j-1 {
		result[i], result[j] = result[j], result[i]
	}

	return result, warnings
}

// lowercaseHost lowercases the host component of a URL path (everything before the first /).
func lowercaseHost(url string) string {
	idx := strings.Index(url, "/")
	if idx == -1 {
		return strings.ToLower(url)
	}
	return strings.ToLower(url[:idx]) + url[idx:]
}

// lastPathComponent returns the last component of a slash-separated path.
func lastPathComponent(path string) string {
	path = strings.TrimSuffix(path, "/")
	if idx := strings.LastIndex(path, "/"); idx != -1 {
		return path[idx+1:]
	}
	return path
}
