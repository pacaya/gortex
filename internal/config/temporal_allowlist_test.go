package config

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

func writeTemporalAllowlist(t *testing.T, repoPath, body string) {
	t.Helper()
	dir := filepath.Join(repoPath, ".gortex")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "temporal-allowlist.yaml"), []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func TestLoadLocalTemporalEnvHelpers_GateOff(t *testing.T) {
	dir := t.TempDir()
	writeTemporalAllowlist(t, dir, "env_helpers:\n  - FetchActivityName\n")
	t.Setenv(LocalTemporalOptInEnv, "") // not opted in
	if got := LoadLocalTemporalEnvHelpers(dir); got != nil {
		t.Fatalf("expected nil without opt-in, got %v", got)
	}
}

func TestLoadLocalTemporalEnvHelpers_GateOnReadsFile(t *testing.T) {
	dir := t.TempDir()
	writeTemporalAllowlist(t, dir, "env_helpers:\n  - FetchActivityName\n  - GetActivity\n  - \"\"\n")
	t.Setenv(LocalTemporalOptInEnv, "1")
	got := LoadLocalTemporalEnvHelpers(dir)
	sort.Strings(got)
	want := []string{"FetchActivityName", "GetActivity"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("got %v, want %v (blank entries dropped)", got, want)
	}
}

func TestLoadLocalTemporalEnvHelpers_GateOnNoFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(LocalTemporalOptInEnv, "true")
	if got := LoadLocalTemporalEnvHelpers(dir); got != nil {
		t.Fatalf("expected nil when file absent, got %v", got)
	}
}

func TestLoadLocalTemporalEnvHelpers_GateOnMalformed(t *testing.T) {
	dir := t.TempDir()
	writeTemporalAllowlist(t, dir, "env_helpers: : not yaml :::\n")
	t.Setenv(LocalTemporalOptInEnv, "1")
	if got := LoadLocalTemporalEnvHelpers(dir); got != nil {
		t.Fatalf("expected nil on malformed file (fail-soft), got %v", got)
	}
}
