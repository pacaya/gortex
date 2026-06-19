package main

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/zzet/gortex/internal/platform"
	"github.com/zzet/gortex/internal/telemetry"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// exitCodeError carries a specific process exit code out of a command's RunE
// without printing the usual "Error:" banner. A command sets it (with
// SilenceErrors) to communicate a machine-readable outcome — e.g. `affected`
// uses it so a CI script can branch on whether any test was affected.
type exitCodeError struct {
	code int
	msg  string
}

func (e *exitCodeError) Error() string { return e.msg }
func (e *exitCodeError) ExitCode() int { return e.code }

var (
	cfgFile    string
	logLevel   string
	noProgress bool
)

var rootCmd = &cobra.Command{
	Use:   "gortex",
	Short: "Code intelligence engine — indexes repos into a queryable knowledge graph",
	// Runs before every subcommand (cobra walks to the nearest
	// PersistentPreRun; no subcommand defines its own). Fold any state
	// left by older versions in the split ~/.config / ~/.cache / flat
	// ~/.gortex layout into the unified ~/.gortex tree before a command
	// opens the store or reads config. Best-effort + idempotent, so it's
	// cheap on every run and silent after the first.
	PersistentPreRun: func(cmd *cobra.Command, args []string) {
		platform.MigrateToUnifiedHome(func(format string, a ...any) {
			fmt.Fprintf(os.Stderr, format+"\n", a...)
		})
	},
	// Runs after every subcommand (no subcommand defines its own
	// PersistentPostRun, so cobra falls through to the root's). Records the
	// command that just ran for opt-in anonymous usage telemetry. Fail-silent
	// and consent-gated, so a default run touches no disk and never affects
	// the command's exit.
	PersistentPostRun: func(cmd *cobra.Command, args []string) {
		recordCLIUsage(cmd, telemetry.NewStore(platform.TelemetryDir()), os.Getenv)
	},
}

// cliCommandDim renders a cobra command's path below the root as a dim-safe
// token — "gortex daemon start" → "daemon.start", "gortex review" → "review".
// Returns "" for the bare root (nothing meaningful ran).
func cliCommandDim(cmd *cobra.Command) string {
	path := strings.TrimSpace(strings.TrimPrefix(cmd.CommandPath(), cmd.Root().Name()))
	return strings.ReplaceAll(path, " ", ".")
}

// recordCLIUsage counts the CLI command that just ran under the cli_command
// metric, gated on consent. It is fail-silent and builds the recorder only when
// consent is enabled, so a default (telemetry-off) invocation never opens the
// telemetry directory. getenv is injected for tests.
func recordCLIUsage(cmd *cobra.Command, store *telemetry.Store, getenv func(string) string) {
	consent := telemetry.ResolveConsent(telemetry.LoadConsentConfig(platform.TelemetryDir()), getenv)
	if !consent.Enabled {
		return
	}
	// A detached daemon re-spawns this binary with GORTEX_DAEMON_CHILD=1 to run
	// `daemon start`; counting that re-spawn would double-record the user's one
	// `gortex daemon start`. Skip it.
	if getenv("GORTEX_DAEMON_CHILD") == "1" {
		return
	}
	dim := cliCommandDim(cmd)
	// Exclude the telemetry subcommand itself — recording usage of the
	// telemetry on/off/status controls is self-referential noise.
	if dim == "" || dim == "telemetry" || strings.HasPrefix(dim, "telemetry.") {
		return
	}
	rec := telemetry.NewRecorder(consent, store)
	rec.Record("cli_command", dim)
	rec.Flush()
}

func init() {
	rootCmd.PersistentFlags().StringVar(&cfgFile, "config", "", "config file (default .gortex.yaml)")
	rootCmd.PersistentFlags().StringVar(&logLevel, "log-level", "info", "log level: debug|info|warn|error")
	rootCmd.PersistentFlags().BoolVar(&noProgress, "no-progress", false, "disable the animated progress spinner (also honored: NO_COLOR, TERM=dumb, non-TTY stderr)")
}

func newLogger() *zap.Logger {
	level := zapcore.InfoLevel
	switch logLevel {
	case "debug":
		level = zapcore.DebugLevel
	case "warn":
		level = zapcore.WarnLevel
	case "error":
		level = zapcore.ErrorLevel
	}
	cfg := zap.NewProductionConfig()
	cfg.Level = zap.NewAtomicLevelAt(level)
	cfg.OutputPaths = []string{"stderr"}
	cfg.ErrorOutputPaths = []string{"stderr"}
	logger, err := cfg.Build()
	if err != nil {
		// Fallback.
		logger = zap.NewNop()
	}
	return logger
}

func execute() {
	assignCommandGroups()
	if err := rootCmd.Execute(); err != nil {
		var ec *exitCodeError
		if errors.As(err, &ec) {
			if ec.msg != "" {
				_, _ = fmt.Fprintln(os.Stderr, ec.msg)
			}
			os.Exit(ec.code)
		}
		os.Exit(1)
	}
}

// assignCommandGroups organizes `gortex --help` into intent groups instead
// of one flat command list, with the MCP server (how editors and agents
// connect) called out first and the CLI commands grouped by what they do.
// Called once before Execute, after every command's init() has registered
// it. Commands left unmapped (internal/utility verbs) fall under cobra's
// "Additional Commands" heading.
func assignCommandGroups() {
	if rootCmd.ContainsGroup("serve") {
		return // idempotent — already grouped
	}
	rootCmd.AddGroup(
		&cobra.Group{ID: "serve", Title: "MCP server — connect editors & agents:"},
		&cobra.Group{ID: "engine", Title: "Daemon & repositories:"},
		&cobra.Group{ID: "query", Title: "Query & explore the graph:"},
		&cobra.Group{ID: "index", Title: "Index & enrich:"},
		&cobra.Group{ID: "setup", Title: "Setup & configuration:"},
	)
	groupOf := map[string]string{
		"mcp":    "serve",
		"daemon": "engine", "track": "engine", "untrack": "engine",
		"repos": "engine", "status": "engine", "proxy": "engine", "workspace": "engine",
		"query": "query", "context": "query", "audit": "query", "wiki": "query",
		"docs": "query", "export": "query", "wakeup": "query", "prs": "query",
		"review": "query",
		"index":  "index", "enrich": "index", "db": "index",
		"init": "setup", "install": "setup", "uninstall": "setup", "agents": "setup",
		"hook": "setup", "githook": "setup", "config": "setup",
		"provider": "setup", "plugin": "setup", "cloud": "setup",
	}
	for _, c := range rootCmd.Commands() {
		if id, ok := groupOf[c.Name()]; ok {
			c.GroupID = id
		}
	}
}
