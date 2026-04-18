package claudecode

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/agents/agentstest"
)

// TestClaudeCodeProjectModeCreatesCanonicalArtifacts is the
// acceptance test for the most important adapter. It asserts that
// a fresh project gets:
//   - .mcp.json with our server stanza
//   - .claude/commands/gortex-*.md (one per SlashCommand)
//   - .claude/settings.json with MCP permissions
//   - .claude/settings.local.json with the three hook events
//   - CLAUDE.md with the instructions block
//   - ~/.claude/skills/gortex-* (one per GlobalSkill)
//
// Re-running must be a no-op (idempotent contract).
func TestClaudeCodeProjectModeCreatesCanonicalArtifacts(t *testing.T) {
	env, _ := agentstest.NewEnv(t)
	a := New()

	res, err := a.Apply(env, agents.ApplyOpts{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !res.Configured {
		t.Fatal("expected Configured=true")
	}

	expected := []string{
		filepath.Join(env.Root, ".mcp.json"),
		filepath.Join(env.Root, ".claude", "settings.json"),
		filepath.Join(env.Root, ".claude", "settings.local.json"),
		filepath.Join(env.Root, "CLAUDE.md"),
	}
	for name := range SlashCommands {
		expected = append(expected, filepath.Join(env.Root, ".claude", "commands", name))
	}
	for name := range GlobalSkills {
		expected = append(expected, filepath.Join(env.Home, ".claude", "skills", name, "SKILL.md"))
	}
	for _, p := range expected {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("missing artifact %s: %v", p, err)
		}
	}

	// CLAUDE.md must contain the sentinel we key idempotency on.
	claudeMd, _ := os.ReadFile(filepath.Join(env.Root, "CLAUDE.md"))
	if !strings.Contains(string(claudeMd), ClaudeMdSentinel) {
		t.Fatalf("CLAUDE.md missing sentinel")
	}

	// Hooks file must reference our test hook command.
	hooksFile, _ := os.ReadFile(filepath.Join(env.Root, ".claude", "settings.local.json"))
	if !strings.Contains(string(hooksFile), "PreToolUse") {
		t.Fatalf("settings.local.json missing PreToolUse: %s", hooksFile)
	}

	// Idempotent re-run: every file should report skip.
	res2, err := a.Apply(env, agents.ApplyOpts{})
	if err != nil {
		t.Fatalf("second apply: %v", err)
	}
	for _, f := range res2.Files {
		if f.Action != agents.ActionSkip {
			t.Errorf("expected skip on re-run for %s, got %s", f.Path, f.Action)
		}
	}
}

// TestClaudeCodeGlobalModeWritesUserFiles verifies that --global
// writes to ~/.claude.json and ~/.claude/settings.local.json and
// leaves the per-repo tree alone.
func TestClaudeCodeGlobalModeWritesUserFiles(t *testing.T) {
	env, _ := agentstest.NewEnv(t)
	env.Mode = agents.ModeGlobal
	a := New()

	res, err := a.Apply(env, agents.ApplyOpts{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !res.Configured {
		t.Fatal("expected Configured=true in global mode")
	}

	// User-level files exist.
	for _, p := range []string{
		filepath.Join(env.Home, ".claude.json"),
		filepath.Join(env.Home, ".claude", "settings.local.json"),
	} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("missing user-level artifact %s: %v", p, err)
		}
	}

	// Per-repo files should *not* exist under global mode.
	for _, p := range []string{
		filepath.Join(env.Root, ".mcp.json"),
		filepath.Join(env.Root, "CLAUDE.md"),
	} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("global mode unexpectedly wrote per-repo file %s", p)
		}
	}
}

// TestClaudeCodeDryRunWritesNothing is the contract for --dry-run:
// Result must still classify every would-be write, but no bytes
// touch disk.
func TestClaudeCodeDryRunWritesNothing(t *testing.T) {
	env, _ := agentstest.NewEnv(t)
	a := New()

	res, err := a.Apply(env, agents.ApplyOpts{DryRun: true})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(res.Files) == 0 {
		t.Fatal("dry-run should still enumerate planned files")
	}
	// No actual files created.
	for _, f := range res.Files {
		if _, err := os.Stat(f.Path); err == nil {
			t.Errorf("dry-run wrote %s", f.Path)
		}
	}
}
