package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestDaemonRequiredErr_GuidesTracking asserts the daemon-required error
// always tells the user how to make the daemon able to answer — naming
// `gortex track <abs path>` regardless of whether a daemon is running.
func TestDaemonRequiredErr_GuidesTracking(t *testing.T) {
	dir := t.TempDir()
	err := daemonRequiredErr(dir)
	if err == nil {
		t.Fatal("daemonRequiredErr returned nil")
	}
	abs, _ := filepath.Abs(dir)
	msg := err.Error()
	if !strings.Contains(msg, "gortex track") {
		t.Errorf("error should suggest `gortex track`: %q", msg)
	}
	if !strings.Contains(msg, abs) {
		t.Errorf("error should name the absolute repo path %q: %q", abs, msg)
	}
}
