package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"go.uber.org/zap"
	"gopkg.in/yaml.v3"
)

// ConfigManager merges GlobalConfig + per-repo WorkspaceConfig.
// It loads the GlobalConfig once at construction and caches workspace
// configs (per-repo .gortex.yaml) on demand with a sync.RWMutex.
type ConfigManager struct {
	global    *GlobalConfig
	workspace map[string]*Config // repoPrefix → workspace config
	mu        sync.RWMutex
	logger    *zap.Logger
}

// NewConfigManager creates a ConfigManager by loading the GlobalConfig
// from the given path. If globalPath is empty, the default path is used.
// A missing GlobalConfig file is not an error (returns empty config).
func NewConfigManager(globalPath string) (*ConfigManager, error) {
	var gc *GlobalConfig
	var err error
	if globalPath != "" {
		gc, err = LoadGlobal(globalPath)
	} else {
		gc, err = LoadGlobal()
	}
	if err != nil {
		return nil, fmt.Errorf("loading global config: %w", err)
	}

	return &ConfigManager{
		global:    gc,
		workspace: make(map[string]*Config),
		logger:    zap.NewNop(),
	}, nil
}

// SetLogger sets the logger for the ConfigManager.
func (cm *ConfigManager) SetLogger(logger *zap.Logger) {
	if logger != nil {
		cm.logger = logger
	}
}

// Global returns the underlying GlobalConfig.
func (cm *ConfigManager) Global() *GlobalConfig {
	return cm.global
}

// LoadWorkspaceConfig loads a .gortex.yaml from the given repo root
// and caches it under the given repoPrefix. If the file is missing,
// no entry is cached (global defaults will apply). If the file is
// malformed, a warning is logged and no entry is cached.
func (cm *ConfigManager) LoadWorkspaceConfig(repoPrefix, repoPath string) {
	configPath := filepath.Join(repoPath, ".gortex.yaml")

	data, err := os.ReadFile(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			// No workspace config — global defaults will apply.
			return
		}
		cm.logger.Warn("failed to read workspace config",
			zap.String("repo", repoPrefix),
			zap.String("path", configPath),
			zap.Error(err))
		return
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		// Malformed workspace config — log warning, return global defaults.
		cm.logger.Warn("malformed workspace config, using global defaults",
			zap.String("repo", repoPrefix),
			zap.String("path", configPath),
			zap.Error(err))
		return
	}

	cm.mu.Lock()
	defer cm.mu.Unlock()
	cm.workspace[repoPrefix] = &cfg
}

// getWorkspaceConfig returns the cached workspace config for a repo, or nil.
func (cm *ConfigManager) getWorkspaceConfig(repoPrefix string) *Config {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	return cm.workspace[repoPrefix]
}

// GetRepoConfig returns the merged config for a repository.
// If a workspace config exists for the repo, it is returned.
// Otherwise, global defaults are returned.
func (cm *ConfigManager) GetRepoConfig(repoPrefix string) *Config {
	ws := cm.getWorkspaceConfig(repoPrefix)
	if ws != nil {
		return ws
	}
	return Default()
}

// EffectiveExclude returns the effective index.exclude patterns for a repo.
// Workspace config wins when present; otherwise global defaults apply.
func (cm *ConfigManager) EffectiveExclude(repoPrefix string) []string {
	ws := cm.getWorkspaceConfig(repoPrefix)
	if ws != nil && len(ws.Index.Exclude) > 0 {
		return ws.Index.Exclude
	}
	return Default().Index.Exclude
}

// EffectiveGuardRules returns the effective guard rules for a repo.
// Workspace config wins when present; otherwise global defaults apply.
func (cm *ConfigManager) EffectiveGuardRules(repoPrefix string) []GuardRule {
	ws := cm.getWorkspaceConfig(repoPrefix)
	if ws != nil && len(ws.Guards.Rules) > 0 {
		return ws.Guards.Rules
	}
	return Default().Guards.Rules
}

// ActiveRepos returns the repos for the active project, or the top-level
// repos if no active project is set.
func (cm *ConfigManager) ActiveRepos() []RepoEntry {
	if cm.global.ActiveProject != "" {
		repos, err := cm.global.ResolveRepos(cm.global.ActiveProject)
		if err == nil {
			return repos
		}
		// If the active project is invalid, fall through to top-level repos.
		cm.logger.Warn("active project not found, falling back to top-level repos",
			zap.String("project", cm.global.ActiveProject),
			zap.Error(err))
	}
	return cm.global.Repos
}
