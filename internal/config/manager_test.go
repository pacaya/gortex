package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
	"pgregory.net/rapid"
)

// --- Unit Tests ---

func TestNewConfigManager_MissingGlobalConfig(t *testing.T) {
	// A non-existent global config path should not error (returns empty config).
	cm, err := NewConfigManager("/tmp/nonexistent-gortex-test-cm/config.yaml")
	require.NoError(t, err)
	require.NotNil(t, cm)
	assert.NotNil(t, cm.Global())
	assert.Empty(t, cm.Global().Repos)
}

func TestNewConfigManager_ValidGlobalConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	content := `
active_project: my-proj
repos:
  - path: /home/user/repo1
    name: repo1
projects:
  my-proj:
    repos:
      - path: /home/user/repo1
        name: repo1
        ref: work
`
	require.NoError(t, os.WriteFile(path, []byte(content), 0644))

	cm, err := NewConfigManager(path)
	require.NoError(t, err)
	assert.Equal(t, "my-proj", cm.Global().ActiveProject)
	assert.Len(t, cm.Global().Repos, 1)
}

func TestGetRepoConfig_NoWorkspaceConfig(t *testing.T) {
	cm, err := NewConfigManager("/tmp/nonexistent-gortex-test-cm/config.yaml")
	require.NoError(t, err)

	cfg := cm.GetRepoConfig("unknown-repo")
	require.NotNil(t, cfg)
	// Should return defaults.
	assert.Equal(t, Default().Index.Exclude, cfg.Index.Exclude)
}

func TestGetRepoConfig_WithWorkspaceConfig(t *testing.T) {
	cm, err := NewConfigManager("/tmp/nonexistent-gortex-test-cm/config.yaml")
	require.NoError(t, err)

	// Create a temp repo with .gortex.yaml.
	repoDir := t.TempDir()
	wsContent := `
index:
  exclude:
    - "custom/**"
guards:
  rules:
    - name: test-rule
      kind: co-change
      source: "src"
      target: "test"
      message: "test changes required"
`
	require.NoError(t, os.WriteFile(filepath.Join(repoDir, ".gortex.yaml"), []byte(wsContent), 0644))

	cm.LoadWorkspaceConfig("my-repo", repoDir)

	cfg := cm.GetRepoConfig("my-repo")
	require.NotNil(t, cfg)
	assert.Equal(t, []string{"custom/**"}, cfg.Index.Exclude)
	assert.Len(t, cfg.Guards.Rules, 1)
	assert.Equal(t, "test-rule", cfg.Guards.Rules[0].Name)
}

func TestEffectiveExclude_WorkspaceOverridesGlobal(t *testing.T) {
	cm, err := NewConfigManager("/tmp/nonexistent-gortex-test-cm/config.yaml")
	require.NoError(t, err)

	// Create a repo with workspace config that has custom excludes.
	repoDir := t.TempDir()
	wsContent := `
index:
  exclude:
    - "ws-vendor/**"
    - "ws-build/**"
`
	require.NoError(t, os.WriteFile(filepath.Join(repoDir, ".gortex.yaml"), []byte(wsContent), 0644))
	cm.LoadWorkspaceConfig("repo-a", repoDir)

	// Workspace config should win.
	excludes := cm.EffectiveExclude("repo-a")
	assert.Equal(t, []string{"ws-vendor/**", "ws-build/**"}, excludes)
}

func TestEffectiveExclude_FallsBackToGlobalDefaults(t *testing.T) {
	cm, err := NewConfigManager("/tmp/nonexistent-gortex-test-cm/config.yaml")
	require.NoError(t, err)

	// No workspace config loaded for this repo — should get defaults.
	excludes := cm.EffectiveExclude("unknown-repo")
	assert.Equal(t, Default().Index.Exclude, excludes)
}

func TestEffectiveExclude_EmptyWorkspaceExcludeFallsBack(t *testing.T) {
	cm, err := NewConfigManager("/tmp/nonexistent-gortex-test-cm/config.yaml")
	require.NoError(t, err)

	// Workspace config exists but has empty exclude list.
	repoDir := t.TempDir()
	wsContent := `
index:
  workers: 4
`
	require.NoError(t, os.WriteFile(filepath.Join(repoDir, ".gortex.yaml"), []byte(wsContent), 0644))
	cm.LoadWorkspaceConfig("repo-b", repoDir)

	// Empty workspace exclude → fall back to defaults.
	excludes := cm.EffectiveExclude("repo-b")
	assert.Equal(t, Default().Index.Exclude, excludes)
}

func TestEffectiveGuardRules_WorkspaceOverridesGlobal(t *testing.T) {
	cm, err := NewConfigManager("/tmp/nonexistent-gortex-test-cm/config.yaml")
	require.NoError(t, err)

	repoDir := t.TempDir()
	wsContent := `
guards:
  rules:
    - name: ws-rule
      kind: boundary
      source: "pkg/a"
      target: "pkg/b"
      message: "boundary violation"
`
	require.NoError(t, os.WriteFile(filepath.Join(repoDir, ".gortex.yaml"), []byte(wsContent), 0644))
	cm.LoadWorkspaceConfig("repo-c", repoDir)

	rules := cm.EffectiveGuardRules("repo-c")
	assert.Len(t, rules, 1)
	assert.Equal(t, "ws-rule", rules[0].Name)
}

func TestEffectiveGuardRules_FallsBackToGlobalDefaults(t *testing.T) {
	cm, err := NewConfigManager("/tmp/nonexistent-gortex-test-cm/config.yaml")
	require.NoError(t, err)

	// Default config has no guard rules.
	rules := cm.EffectiveGuardRules("unknown-repo")
	assert.Empty(t, rules)
}

func TestActiveRepos_WithActiveProject(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	content := `
active_project: my-proj
repos:
  - path: /top-level/repo
    name: top-repo
projects:
  my-proj:
    repos:
      - path: /proj/repo1
        name: proj-repo1
      - path: /proj/repo2
        name: proj-repo2
`
	require.NoError(t, os.WriteFile(path, []byte(content), 0644))

	cm, err := NewConfigManager(path)
	require.NoError(t, err)

	repos := cm.ActiveRepos()
	assert.Len(t, repos, 2)
	assert.Equal(t, "proj-repo1", repos[0].Name)
	assert.Equal(t, "proj-repo2", repos[1].Name)
}

func TestActiveRepos_NoActiveProject(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	content := `
repos:
  - path: /top/repo1
    name: repo1
  - path: /top/repo2
    name: repo2
`
	require.NoError(t, os.WriteFile(path, []byte(content), 0644))

	cm, err := NewConfigManager(path)
	require.NoError(t, err)

	repos := cm.ActiveRepos()
	assert.Len(t, repos, 2)
	assert.Equal(t, "repo1", repos[0].Name)
}

func TestActiveRepos_InvalidActiveProjectFallsBack(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	content := `
active_project: nonexistent
repos:
  - path: /fallback/repo
    name: fallback
`
	require.NoError(t, os.WriteFile(path, []byte(content), 0644))

	cm, err := NewConfigManager(path)
	require.NoError(t, err)

	repos := cm.ActiveRepos()
	assert.Len(t, repos, 1)
	assert.Equal(t, "fallback", repos[0].Name)
}

func TestLoadWorkspaceConfig_MissingFile(t *testing.T) {
	cm, err := NewConfigManager("/tmp/nonexistent-gortex-test-cm/config.yaml")
	require.NoError(t, err)

	// Loading from a dir without .gortex.yaml should not cache anything.
	cm.LoadWorkspaceConfig("repo-x", t.TempDir())

	cfg := cm.getWorkspaceConfig("repo-x")
	assert.Nil(t, cfg)
}

func TestLoadWorkspaceConfig_MalformedYAML(t *testing.T) {
	cm, err := NewConfigManager("/tmp/nonexistent-gortex-test-cm/config.yaml")
	require.NoError(t, err)

	repoDir := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(repoDir, ".gortex.yaml"),
		[]byte(":::invalid yaml content"),
		0644,
	))

	// Should log warning and not cache.
	cm.LoadWorkspaceConfig("bad-repo", repoDir)

	cfg := cm.getWorkspaceConfig("bad-repo")
	assert.Nil(t, cfg)
}

func TestNewConfigManager_EmptyPath(t *testing.T) {
	// Empty path uses default — may or may not exist, but should not panic.
	cm, err := NewConfigManager("")
	// We can't guarantee the default path exists, but it should not error
	// if the file is simply missing.
	if err == nil {
		assert.NotNil(t, cm)
	}
}

// --- Property-Based Tests ---

// workspaceYAML is a helper struct for marshaling workspace config to YAML.
// We use explicit yaml tags since the Config struct uses mapstructure tags.
type workspaceYAML struct {
	Index  indexYAML  `yaml:"index,omitempty"`
	Guards guardsYAML `yaml:"guards,omitempty"`
}

type indexYAML struct {
	Exclude []string `yaml:"exclude,omitempty"`
}

type guardsYAML struct {
	Rules []guardRuleYAML `yaml:"rules,omitempty"`
}

type guardRuleYAML struct {
	Name    string `yaml:"name"`
	Kind    string `yaml:"kind"`
	Source  string `yaml:"source"`
	Target  string `yaml:"target"`
	Message string `yaml:"message"`
}

// genExcludePatterns generates a random list of exclude patterns (sometimes empty).
func genExcludePatterns() *rapid.Generator[[]string] {
	return rapid.Custom(func(t *rapid.T) []string {
		isEmpty := rapid.Bool().Draw(t, "emptyExclude")
		if isEmpty {
			return nil
		}
		n := rapid.IntRange(1, 5).Draw(t, "numPatterns")
		patterns := make([]string, n)
		for i := range n {
			patterns[i] = rapid.StringMatching(`[a-z]{1,10}/\*\*`).Draw(t, "pattern")
		}
		return patterns
	})
}

// genGuardRules generates a random list of guard rules (sometimes empty).
func genGuardRules() *rapid.Generator[[]guardRuleYAML] {
	return rapid.Custom(func(t *rapid.T) []guardRuleYAML {
		isEmpty := rapid.Bool().Draw(t, "emptyRules")
		if isEmpty {
			return nil
		}
		n := rapid.IntRange(1, 3).Draw(t, "numRules")
		rules := make([]guardRuleYAML, n)
		for i := range n {
			rules[i] = guardRuleYAML{
				Name:    rapid.StringMatching(`[a-z]{3,10}-rule`).Draw(t, "ruleName"),
				Kind:    rapid.SampledFrom([]string{"co-change", "boundary"}).Draw(t, "kind"),
				Source:  rapid.StringMatching(`[a-z]{2,8}/[a-z]{2,8}`).Draw(t, "source"),
				Target:  rapid.StringMatching(`[a-z]{2,8}/[a-z]{2,8}`).Draw(t, "target"),
				Message: rapid.StringMatching(`[a-z ]{5,30}`).Draw(t, "message"),
			}
		}
		return rules
	})
}

// TestPropertyConfigOverrideSemantics verifies Property 16: Config override semantics.
//
// Feature: multi-repo-support, Property 16: Config override semantics
//
// For any setting defined in both the GlobalConfig and a repository's WorkspaceConfig,
// the effective value for that repository SHALL be the WorkspaceConfig value.
// When the WorkspaceConfig is absent or does not define the setting, the effective value
// SHALL be the GlobalConfig default.
//
func TestPropertyConfigOverrideSemantics(t *testing.T) {
	globalDefaults := Default()

	rapid.Check(t, func(rt *rapid.T) {
		// Generate random workspace config content.
		wsExclude := genExcludePatterns().Draw(rt, "wsExclude")
		wsRules := genGuardRules().Draw(rt, "wsRules")

		repoPrefix := rapid.StringMatching(`[a-z]{3,10}`).Draw(rt, "repoPrefix")

		// Create a temp directory with a .gortex.yaml containing the generated config.
		repoDir := t.TempDir()

		wsCfg := workspaceYAML{
			Index:  indexYAML{Exclude: wsExclude},
			Guards: guardsYAML{Rules: wsRules},
		}

		data, err := yaml.Marshal(&wsCfg)
		require.NoError(rt, err)

		err = os.WriteFile(filepath.Join(repoDir, ".gortex.yaml"), data, 0644)
		require.NoError(rt, err)

		// Create a ConfigManager (no global config file needed).
		cm, err := NewConfigManager("/tmp/nonexistent-gortex-pbt-" + repoPrefix + "/config.yaml")
		require.NoError(rt, err)

		// Load the workspace config.
		cm.LoadWorkspaceConfig(repoPrefix, repoDir)

		// --- Test EffectiveExclude ---
		effectiveExclude := cm.EffectiveExclude(repoPrefix)

		if len(wsExclude) > 0 {
			// Workspace has non-empty exclude → workspace wins.
			assert.Equal(rt, wsExclude, effectiveExclude,
				"workspace exclude should override global when present")
		} else {
			// Workspace has empty/no exclude → global defaults apply.
			assert.Equal(rt, globalDefaults.Index.Exclude, effectiveExclude,
				"global default exclude should apply when workspace is empty")
		}

		// --- Test EffectiveGuardRules ---
		effectiveRules := cm.EffectiveGuardRules(repoPrefix)

		if len(wsRules) > 0 {
			// Workspace has non-empty guard rules → workspace wins.
			assert.Len(rt, effectiveRules, len(wsRules),
				"workspace guard rules should override global when present")
			for i, rule := range effectiveRules {
				assert.Equal(rt, wsRules[i].Name, rule.Name)
				assert.Equal(rt, wsRules[i].Kind, rule.Kind)
				assert.Equal(rt, wsRules[i].Source, rule.Source)
				assert.Equal(rt, wsRules[i].Target, rule.Target)
				assert.Equal(rt, wsRules[i].Message, rule.Message)
			}
		} else {
			// Workspace has empty/no guard rules → global defaults apply.
			assert.Equal(rt, globalDefaults.Guards.Rules, effectiveRules,
				"global default guard rules should apply when workspace is empty")
		}

		// --- Test that an unknown repo (no workspace config) gets global defaults ---
		unknownPrefix := repoPrefix + "-unknown"
		assert.Equal(rt, globalDefaults.Index.Exclude, cm.EffectiveExclude(unknownPrefix),
			"repo without workspace config should get global default exclude")
		assert.Equal(rt, globalDefaults.Guards.Rules, cm.EffectiveGuardRules(unknownPrefix),
			"repo without workspace config should get global default guard rules")
	})
}
