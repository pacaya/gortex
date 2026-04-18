package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestInitDryRunJSONReportShape is the end-to-end contract test for
// the non-interactive init flags. It runs runInit with --yes
// --dry-run --json --agents=claude-code against a clean temp dir
// and asserts the JSON report is well-formed and reflects reality.
//
// Covers the slice of Step 2 not exercised by adapter-level tests:
// the cobra command wiring, flag parsing, registry filter, and JSON
// report emission.
func TestInitDryRunJSONReportShape(t *testing.T) {
	// Save and restore mutable package-level init flags — tests that
	// run after this one would otherwise see leftover state.
	saved := struct {
		yes, dryRun, json bool
		agents            string
	}{initYes, initDryRun, initJSON, initAgents}
	t.Cleanup(func() {
		initYes, initDryRun, initJSON = saved.yes, saved.dryRun, saved.json
		initAgents = saved.agents
	})

	repo := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)
	chdir(t, repo)

	initYes = true
	initDryRun = true
	initJSON = true
	initAgents = "claude-code"

	var stdout, stderr bytes.Buffer
	initCmd.SetOut(&stdout)
	initCmd.SetErr(&stderr)
	t.Cleanup(func() {
		initCmd.SetOut(nil)
		initCmd.SetErr(nil)
	})

	if err := runInit(initCmd, []string{repo}); err != nil {
		t.Fatalf("runInit: %v\nstderr: %s", err, stderr.String())
	}

	// stdout must be a parseable JSON object.
	var report map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("non-JSON stdout: %v\n%s", err, stdout.String())
	}
	if report["dry_run"] != true {
		t.Fatalf("dry_run flag missing from report: %v", report)
	}
	rawAgents, ok := report["agents"].([]any)
	if !ok || len(rawAgents) != 1 {
		t.Fatalf("expected exactly 1 agent in report, got %v", report["agents"])
	}
	agent := rawAgents[0].(map[string]any)
	if agent["name"] != "claude-code" {
		t.Fatalf("expected claude-code, got %v", agent["name"])
	}
	files, ok := agent["files"].([]any)
	if !ok || len(files) == 0 {
		t.Fatalf("expected non-empty files array, got %v", agent["files"])
	}
	for _, f := range files {
		fm := f.(map[string]any)
		action, _ := fm["action"].(string)
		if !strings.HasPrefix(action, "would-") && action != "skip" {
			t.Fatalf("dry-run emitted non-planning action %q for %v", action, fm["path"])
		}
	}

	// Dry-run must not have created anything on disk.
	for _, p := range []string{
		filepath.Join(repo, ".mcp.json"),
		filepath.Join(repo, "CLAUDE.md"),
	} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("dry-run wrote %s", p)
		}
	}
}

// TestInitAgentsFilterRejectsUnknownName is the hard-error contract
// for unknown --agents values — silent skips would mask typos.
func TestInitAgentsFilterRejectsUnknownName(t *testing.T) {
	saved := struct {
		yes, dryRun, json bool
		agents            string
	}{initYes, initDryRun, initJSON, initAgents}
	t.Cleanup(func() {
		initYes, initDryRun, initJSON = saved.yes, saved.dryRun, saved.json
		initAgents = saved.agents
	})

	repo := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	chdir(t, repo)

	initYes = true
	initDryRun = true
	initJSON = true
	initAgents = "claude-code,nonexistent-agent"

	var stdout, stderr bytes.Buffer
	initCmd.SetOut(&stdout)
	initCmd.SetErr(&stderr)
	t.Cleanup(func() {
		initCmd.SetOut(nil)
		initCmd.SetErr(nil)
	})

	err := runInit(initCmd, []string{repo})
	if err == nil {
		t.Fatal("expected error for unknown agent name, got nil")
	}
	if !strings.Contains(err.Error(), "nonexistent-agent") {
		t.Fatalf("expected error to name the typo, got: %v", err)
	}
}

// chdir cd's into dir and registers a Cleanup that restores the
// original cwd. Needed because runInit resolves "." against cwd.
func chdir(t *testing.T, dir string) {
	t.Helper()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(orig) })
}
