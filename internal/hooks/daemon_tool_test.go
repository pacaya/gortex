package hooks

import "testing"

// The daemon-socket fallback must only fire when a hook CWD has been recorded,
// so the pure-HTTP unit tests (which never set one) keep their "no bridge"
// semantics and never touch a real daemon.
func TestCallServerToolDaemonFallbackGatedOnHookCWD(t *testing.T) {
	var called int
	old := callServerToolDaemonFn
	callServerToolDaemonFn = func(_, name string, _ map[string]any) string {
		called++
		return "daemon:" + name
	}
	t.Cleanup(func() { callServerToolDaemonFn = old })

	// No hook CWD → HTTP (port 0, unreachable) fails and the fallback is
	// skipped, leaving the historical empty-string result.
	setHookCWD("")
	if got := callServerTool(0, "detect_changes", nil); got != "" {
		t.Fatalf("want empty result without a hook CWD, got %q", got)
	}
	if called != 0 {
		t.Fatalf("daemon fallback must not fire without a hook CWD, fired %d times", called)
	}

	// With a hook CWD set, an unreachable HTTP surface falls back to the
	// daemon socket scoped to that CWD.
	setHookCWD("/repo")
	defer setHookCWD("")
	if got := callServerTool(0, "detect_changes", nil); got != "daemon:detect_changes" {
		t.Fatalf("want daemon fallback result, got %q", got)
	}
	if called != 1 {
		t.Fatalf("daemon fallback should fire exactly once, fired %d times", called)
	}
}
