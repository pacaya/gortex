package config

// Repo-local, git-ignored Temporal allow-list.
//
// PURPOSE: let a corporate agent extend the Temporal env-helper allow-list with
// project-specific helper names WITHOUT committing those names into the source
// tree (and so without leaking them upstream). The names live in a git-ignored
// `.gortex/temporal-allowlist.yaml`; matching one promotes a dispatch from the
// generic "env"-name heuristic (hidden, speculative) to the allow-list tier
// (visible, inferred).
//
// RATIONALE: mirrors the repo-local `.gortex/providers.json` opt-in
// (GORTEX_ALLOW_LOCAL_PROVIDERS): a checked-out repo could otherwise inject
// names that change how the indexer attributes dispatch, so the file is read
// ONLY behind an explicit env gate. Fail-soft throughout — a missing gate,
// missing file, or malformed file simply yields no extra names.
//
// KEYWORDS: temporal, allow-list, env-helper, repo-local, opt-in, git-ignored

import (
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// LocalTemporalOptInEnv gates loading the repo-local Temporal allow-list.
const LocalTemporalOptInEnv = "GORTEX_ALLOW_LOCAL_TEMPORAL"

// localTemporalAllowlistPath is the repo-local allow-list file, under the
// per-repo `.gortex/` directory (which should be git-ignored).
func localTemporalAllowlistPath(repoPath string) string {
	return filepath.Join(repoPath, ".gortex", "temporal-allowlist.yaml")
}

// temporalAllowlistFile is the on-disk shape. Only env_helpers is consumed
// today; the struct leaves room for future categories (suffixes, register
// helpers) without a format break.
type temporalAllowlistFile struct {
	EnvHelpers []string `yaml:"env_helpers"`
}

// LoadLocalTemporalEnvHelpers returns the corporate env-helper names declared
// in the repo-local `.gortex/temporal-allowlist.yaml`, or nil. Fail-soft: the
// gate being off, a missing file, or a malformed file all yield nil — the
// built-in allow-list and the generic heuristic still apply. `repoPath` is the
// indexed repository's root.
func LoadLocalTemporalEnvHelpers(repoPath string) []string {
	if !localTemporalOptedIn() {
		return nil
	}
	raw, err := os.ReadFile(localTemporalAllowlistPath(repoPath))
	if err != nil {
		return nil
	}
	var f temporalAllowlistFile
	if err := yaml.Unmarshal(raw, &f); err != nil {
		return nil
	}
	out := make([]string, 0, len(f.EnvHelpers))
	for _, n := range f.EnvHelpers {
		if n = strings.TrimSpace(n); n != "" {
			out = append(out, n)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// localTemporalOptedIn reports whether the repo-local allow-list opt-in env var
// is set to a truthy value.
func localTemporalOptedIn() bool {
	v := strings.TrimSpace(os.Getenv(LocalTemporalOptInEnv))
	return v == "1" || strings.EqualFold(v, "true")
}
