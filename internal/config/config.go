package config

import (
	"runtime"
	"strings"

	"github.com/spf13/viper"
)

type GuardRule struct {
	Name    string `mapstructure:"name"`
	Kind    string `mapstructure:"kind"`    // "co-change" | "boundary"
	Source  string `mapstructure:"source"`  // package/path prefix
	Target  string `mapstructure:"target"`  // package/path prefix
	Message string `mapstructure:"message"` // human-readable explanation
}

type GuardsConfig struct {
	Rules []GuardRule `mapstructure:"rules"`
}

// WorkspaceConfig holds workspace-level settings for multi-repo support.
type WorkspaceConfig struct {
	AutoDetect bool `mapstructure:"auto_detect" yaml:"auto_detect"`
}

type Config struct {
	Index     IndexConfig     `mapstructure:"index"`
	Watch     WatchConfig     `mapstructure:"watch"`
	Query     QueryConfig     `mapstructure:"query"`
	MCP       MCPConfig       `mapstructure:"mcp"`
	Guards    GuardsConfig    `mapstructure:"guards"`
	Workspace WorkspaceConfig `mapstructure:"workspace" yaml:"workspace,omitempty"`
}

type IndexConfig struct {
	Languages []string `mapstructure:"languages"`
	Exclude   []string `mapstructure:"exclude"`
	Workers   int      `mapstructure:"workers"`
}

type WatchConfig struct {
	Enabled    bool     `mapstructure:"enabled"`
	Paths      []string `mapstructure:"paths"`
	DebounceMs int      `mapstructure:"debounce_ms"`
	Exclude    []string `mapstructure:"exclude"`
}

type QueryConfig struct {
	DefaultDepth int `mapstructure:"default_depth"`
	MaxDepth     int `mapstructure:"max_depth"`
}

type MCPConfig struct {
	Transport string `mapstructure:"transport"`
	Port      int    `mapstructure:"port"`
}

// Default returns a Config with sensible defaults.
func Default() *Config {
	return &Config{
		Index: IndexConfig{
			Exclude: []string{
				"vendor/**", "node_modules/**", ".git/**",
				"dist/**", "build/**",
			},
			Workers: runtime.NumCPU(),
		},
		Watch: WatchConfig{
			Enabled:    false,
			Paths:      []string{"."},
			DebounceMs: 150,
			Exclude: []string{
				"**/*.tmp", "**/*.swp", "**/.git/**", "**/node_modules/**",
			},
		},
		Query: QueryConfig{
			DefaultDepth: 3,
			MaxDepth:     10,
		},
		MCP: MCPConfig{
			Transport: "stdio",
			Port:      8765,
		},
		Workspace: WorkspaceConfig{
			AutoDetect: false,
		},
	}
}

// Load reads config from file, environment, and returns a merged Config.
// configPath may be empty; in that case only default locations are searched.
func Load(configPath string) (*Config, error) {
	v := viper.New()
	v.SetConfigName(".gortex")
	v.SetConfigType("yaml")

	if configPath != "" {
		v.SetConfigFile(configPath)
	} else {
		v.AddConfigPath(".")
		v.AddConfigPath("$HOME/.config/gortex")
	}

	v.SetEnvPrefix("GORTEX")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	cfg := Default()

	if err := v.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); !ok {
			return nil, err
		}
		// No config file found — use defaults + env.
	}

	if err := v.Unmarshal(cfg); err != nil {
		return nil, err
	}

	return cfg, nil
}
