package indexer

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// UnsafeIndexRootReason returns a human-readable reason when indexing the given
// path would be dangerous — the filesystem root, a Windows drive root, or the
// user's home directory — or "" when the path is safe to index. Indexing such a
// location walks an unbounded tree (every repo, cache, and dotfile on the
// machine), risking file-descriptor exhaustion and a runaway crawl, so the
// caller should require an explicit override before proceeding.
func UnsafeIndexRootReason(path string) string {
	if path == "" {
		return ""
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return ""
	}
	abs = filepath.Clean(abs)
	switch {
	case isFilesystemRoot(abs):
		return "refusing to index the filesystem root " + abs + " — this would crawl the entire machine"
	case isDriveRoot(abs):
		return "refusing to index the drive root " + abs + " — this would crawl the entire drive"
	default:
		return homeDirReason(abs)
	}
}

// unsafeRootBlocked reports the refusal reason for indexing or tracking path,
// or ("", false) when the path is safe or the caller explicitly forced the
// operation. It is the shared gate behind the index and track entry points.
func unsafeRootBlocked(path string, force bool) (reason string, blocked bool) {
	if force {
		return "", false
	}
	if r := UnsafeIndexRootReason(path); r != "" {
		return r, true
	}
	return "", false
}

// isFilesystemRoot reports whether abs is the POSIX filesystem root.
func isFilesystemRoot(abs string) bool {
	return abs == "/" || abs == string(filepath.Separator)
}

// isDriveRoot reports whether abs is a bare Windows drive root such as `C:\`.
func isDriveRoot(abs string) bool {
	if runtime.GOOS != "windows" {
		return false
	}
	vol := filepath.VolumeName(abs)
	if vol == "" {
		return false
	}
	rest := strings.TrimPrefix(abs, vol)
	return rest == "" || rest == "\\" || rest == "/" || rest == string(filepath.Separator)
}

// homeDirReason returns a reason when abs is exactly the user's home directory,
// or "" otherwise. A subdirectory of home is safe — only the home root itself is
// refused, because that is the runaway-crawl footgun.
func homeDirReason(abs string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	if filepath.Clean(home) == abs {
		return "refusing to index the home directory " + abs + " — this would crawl every project and dotfile under it"
	}
	return ""
}
