package semantic

import "sync"

// Config holds configuration for the semantic enrichment layer.
type Config struct {
	Enabled           bool             `mapstructure:"enabled" yaml:"enabled"`
	TimeoutSeconds    int              `mapstructure:"timeout_seconds" yaml:"timeout_seconds,omitempty"`
	EnrichOnWatch     bool             `mapstructure:"enrich_on_watch" yaml:"enrich_on_watch,omitempty"`
	WatchDebounceMs   int              `mapstructure:"watch_debounce_ms" yaml:"watch_debounce_ms,omitempty"`
	RefuteUnconfirmed bool             `mapstructure:"refute_unconfirmed" yaml:"refute_unconfirmed,omitempty"`
	Providers         []ProviderConfig `mapstructure:"providers" yaml:"providers,omitempty"`
}

// ProviderConfig holds configuration for a single semantic provider.
type ProviderConfig struct {
	Name        string   `mapstructure:"name" yaml:"name"`
	Command     string   `mapstructure:"command" yaml:"command,omitempty"`
	Args        []string `mapstructure:"args" yaml:"args,omitempty"`
	Languages   []string `mapstructure:"languages" yaml:"languages"`
	Priority    int      `mapstructure:"priority" yaml:"priority,omitempty"`
	Enabled     bool     `mapstructure:"enabled" yaml:"enabled"`
	Mode        string   `mapstructure:"mode" yaml:"mode,omitempty"` // "typecheck" or "callgraph" for go-types
	Daemon      bool     `mapstructure:"daemon" yaml:"daemon,omitempty"`
	MaxParallel int      `mapstructure:"max_parallel" yaml:"max_parallel,omitempty"`
	// Env adds KEY=VALUE environment entries to the provider's LSP
	// subprocess (e.g. JAVA_HOME for jdtls).
	Env []string `mapstructure:"env" yaml:"env,omitempty"`
}

// DefaultConfig returns a default semantic config with auto-detection enabled.
//
// The order matters: per-language priority sorts ascending, so go-types
// (priority 1) wins for Go even when scip-go and gopls are also
// available. Every known LSP server is enumerated via
// RegisterDefaultProviders so that, when its binary is on PATH, the
// daemon spins it up automatically without users editing
// `.gortex.yaml`.
func DefaultConfig() Config {
	cfg := Config{
		Enabled:           true,
		TimeoutSeconds:    120,
		EnrichOnWatch:     false,
		WatchDebounceMs:   500,
		RefuteUnconfirmed: false,
		Providers: []ProviderConfig{
			{
				Name:      "go-types",
				Languages: []string{"go"},
				Priority:  1,
				Enabled:   true,
				Mode:      "typecheck",
			},
			{
				Name:      "scip-go",
				Command:   "scip-go",
				Languages: []string{"go"},
				Priority:  2,
				Enabled:   true,
			},
			{
				Name:      "scip-typescript",
				Command:   "scip-typescript",
				Args:      []string{"index", "--infer-tsconfig"},
				Languages: []string{"typescript", "javascript"},
				Priority:  1,
				Enabled:   true,
			},
			{
				Name:      "scip-python",
				Command:   "scip-python",
				Languages: []string{"python"},
				Priority:  1,
				Enabled:   true,
			},
		},
	}
	cfg.Providers = append(cfg.Providers, defaultLSPProviders()...)
	return cfg
}

// defaultLSPProviders returns LSP-flavored ProviderConfig entries
// contributed by sub-packages via RegisterDefaultProviders. The
// `internal/semantic/lsp` package registers its `Servers` list at
// init time. The indirection avoids a circular import — lsp depends
// on semantic for EnrichResult etc.
func defaultLSPProviders() []ProviderConfig {
	defaultRegMu.RLock()
	defer defaultRegMu.RUnlock()
	out := make([]ProviderConfig, 0)
	for _, fn := range defaultRegistrations {
		out = append(out, fn()...)
	}
	return out
}

// RegisterDefaultProviders lets sub-packages contribute provider entries
// to DefaultConfig. Each registered function is called when DefaultConfig
// is invoked. Registration order is preserved.
func RegisterDefaultProviders(fn func() []ProviderConfig) {
	if fn == nil {
		return
	}
	defaultRegMu.Lock()
	defaultRegistrations = append(defaultRegistrations, fn)
	defaultRegMu.Unlock()
}

var (
	defaultRegMu         sync.RWMutex
	defaultRegistrations []func() []ProviderConfig
)
