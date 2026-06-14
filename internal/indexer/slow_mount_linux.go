//go:build linux

package indexer

import (
	"os"
	"strings"
	"syscall"
)

// slowWatchMount reports whether path lives on a filesystem where native
// fsnotify is unreliable or prohibitively slow — notably a Windows drive
// surfaced into WSL2 via 9p/drvfs (inotify events arrive late or never), or
// an SMB/CIFS share. On such a mount the watcher disables fsnotify and
// relies on the adaptive poller + git hooks. GORTEX_FORCE_FSNOTIFY=1 forces
// native fsnotify on regardless.
func slowWatchMount(path string) bool {
	if path == "" || os.Getenv("GORTEX_FORCE_FSNOTIFY") == "1" {
		return false
	}
	if !runningUnderWSL() {
		return false
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return false
	}
	switch int64(st.Type) {
	case 0x01021997, // V9FS_MAGIC — 9p, WSL2's drvfs transport for Windows drives
		0xFF534D42: // CIFS_MAGIC — SMB/CIFS share
		return true
	}
	return false
}

// runningUnderWSL reports whether the process is inside the Windows
// Subsystem for Linux, probed from /proc/version's microsoft/WSL marker.
func runningUnderWSL() bool {
	b, err := os.ReadFile("/proc/version")
	if err != nil {
		return false
	}
	v := strings.ToLower(string(b))
	return strings.Contains(v, "microsoft") || strings.Contains(v, "wsl")
}
