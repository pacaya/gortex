package indexer

import "testing"

// TestSlowWatchMount_NormalMountNotDegraded guards the safe default: a
// normal local filesystem (the test temp dir) must never be flagged as a
// slow mount, so fsnotify is only disabled on a genuine WSL2 9p/SMB mount.
func TestSlowWatchMount_NormalMountNotDegraded(t *testing.T) {
	if slowWatchMount(t.TempDir()) {
		t.Error("a normal local mount must not be flagged slow (fsnotify must stay enabled)")
	}
	if slowWatchMount("") {
		t.Error("an empty path must not be flagged slow")
	}
}
