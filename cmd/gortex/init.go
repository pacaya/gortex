package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/agents/aider"
	"github.com/zzet/gortex/internal/agents/antigravity"
	"github.com/zzet/gortex/internal/agents/claudecode"
	"github.com/zzet/gortex/internal/agents/cline"
	"github.com/zzet/gortex/internal/agents/codex"
	"github.com/zzet/gortex/internal/agents/continuedev"
	"github.com/zzet/gortex/internal/agents/cursor"
	"github.com/zzet/gortex/internal/agents/gemini"
	"github.com/zzet/gortex/internal/agents/kilocode"
	"github.com/zzet/gortex/internal/agents/kiro"
	"github.com/zzet/gortex/internal/agents/opencode"
	"github.com/zzet/gortex/internal/agents/openclaw"
	"github.com/zzet/gortex/internal/agents/vscode"
	"github.com/zzet/gortex/internal/agents/windsurf"
	"github.com/zzet/gortex/internal/agents/zed"
	"github.com/zzet/gortex/internal/claudemd"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/query"
)

// Per-flag globals. Behaviour flags (--hooks, --global, etc.) stay
// separate from UX flags (--yes, --json, --dry-run) so the wizard
// and the orchestrator read them without cross-pollution.
var (
	initAnalyze      bool
	initHooksOnly    bool
	initInstallHooks = true
	initNoHooks      bool
	initGlobal       bool
	initStartDaemon  bool
	initTrackRepo    bool

	// Step 2 — non-interactive contract.
	initYes        bool
	initAgents     string
	initAgentsSkip string
	initJSON       bool
	initDryRun     bool
	initForce      bool
)

var initCmd = &cobra.Command{
	Use:   "init [path]",
	Short: "Set up Gortex integration for every detected AI coding assistant",
	Args:  cobra.MaximumNArgs(1),
	RunE:  runInit,
}

func init() {
	initCmd.Flags().BoolVar(&initAnalyze, "analyze", false, "index the repo first to generate a richer CLAUDE.md with codebase overview")
	initCmd.Flags().BoolVar(&initInstallHooks, "hooks", true, "install Claude Code hooks (PreToolUse + PreCompact + Stop); use --no-hooks to skip")
	initCmd.Flags().BoolVar(&initNoHooks, "no-hooks", false, "skip installing Claude Code hooks (inverse of --hooks)")
	initCmd.Flags().BoolVar(&initHooksOnly, "hooks-only", false, "only install/update Claude Code hooks in .claude/settings.local.json, skip everything else")
	initCmd.Flags().BoolVar(&initGlobal, "global", false, "install a user-wide config (~/.claude.json) that points every project at the daemon; skip per-repo file creation")
	initCmd.Flags().BoolVar(&initStartDaemon, "start", false, "start the daemon immediately after --global setup (detached)")
	initCmd.Flags().BoolVar(&initTrackRepo, "track", false, "track the current repo via the daemon after --global setup")

	// Non-interactive / CI / scripted flags (Step 2 of the init plan).
	initCmd.Flags().BoolVarP(&initYes, "yes", "y", false, "skip the interactive wizard and use defaults (implied when stdin is not a TTY)")
	initCmd.Flags().StringVar(&initAgents, "agents", "", "comma-separated list of agents to configure ('auto' means every registered adapter)")
	initCmd.Flags().StringVar(&initAgentsSkip, "agents-skip", "", "comma-separated list of agents to skip (composable with --agents)")
	initCmd.Flags().BoolVar(&initJSON, "json", false, "emit a structured JSON report on stdout (human-readable banner moves to stderr)")
	initCmd.Flags().BoolVar(&initDryRun, "dry-run", false, "plan writes without modifying disk (implies --json is useful)")
	initCmd.Flags().BoolVar(&initForce, "force", false, "overwrite keys we would otherwise preserve during a merge")

	rootCmd.AddCommand(initCmd)
}

// buildRegistry wires up every registered adapter. Registration
// order is also the execution order — Claude Code first because it's
// the primary integration, then the other per-IDE adapters in
// alphabetical order to keep the --json report stable.
func buildRegistry() *agents.Registry {
	r := agents.NewRegistry()
	r.Register(claudecode.New())
	r.Register(aider.New())
	r.Register(antigravity.New())
	r.Register(cline.New())
	r.Register(codex.New())
	r.Register(continuedev.New())
	r.Register(cursor.New())
	r.Register(gemini.New())
	r.Register(kilocode.New())
	r.Register(kiro.New())
	r.Register(opencode.New())
	r.Register(openclaw.New())
	r.Register(vscode.New())
	r.Register(windsurf.New())
	r.Register(zed.New())
	return r
}

func runInit(cmd *cobra.Command, args []string) error {
	root := "."
	if len(args) > 0 {
		root = args[0]
	}

	// --no-hooks is the explicit negation for --hooks; fold it so
	// the orchestrator only has to check initInstallHooks.
	if initNoHooks {
		initInstallHooks = false
	}

	// Interactive wizard. Runs only when no mode flag is pre-set,
	// stdin is a TTY, and --yes wasn't passed. --hooks-only, --global,
	// and --yes all pre-answer the mode question.
	if !initYes && !initGlobal && !initHooksOnly && !cmd.Flags().Changed("global") && isInteractive() {
		hooksPreset := cmd.Flags().Changed("hooks") || cmd.Flags().Changed("no-hooks")
		if choice, ran := runInteractiveInit(os.Stdin, cmd.ErrOrStderr(), hooksPreset); ran {
			initGlobal = choice.Global
			initTrackRepo = choice.Track
			initStartDaemon = choice.Start
			if !hooksPreset {
				initInstallHooks = choice.Hooks
			}
		}
	}

	// Resolve root to an absolute path — every adapter expects Root
	// to be absolute so joined paths don't pick up the wrong cwd.
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return err
	}

	// --hooks-only short-circuit: just install/heal Claude Code
	// hooks and exit. Everything else is a no-op.
	if initHooksOnly {
		settingsPath := filepath.Join(absRoot, ".claude", "settings.local.json")
		if err := os.MkdirAll(filepath.Dir(settingsPath), 0o755); err != nil {
			return err
		}
		action, err := claudecode.InstallHook(cmd.ErrOrStderr(), settingsPath, agents.ApplyOpts{DryRun: initDryRun, Force: initForce})
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "[gortex init --hooks-only] %s %s\n", action.Action, action.Path)
		return nil
	}

	// The remaining code is the full orchestration path used by both
	// per-repo and --global modes. Build the Env once.
	home, _ := os.UserHomeDir()
	mode := agents.ModeProject
	if initGlobal {
		mode = agents.ModeGlobal
	}

	env := agents.Env{
		Root:         absRoot,
		Home:         home,
		HookCommand:  claudecode.ResolveHookCommand(cmd.ErrOrStderr()),
		Mode:         mode,
		InstallHooks: initInstallHooks,
		AnalyzeRepo:  initAnalyze,
		Stderr:       cmd.ErrOrStderr(),
	}

	// --analyze piggybacks the indexer to generate a dynamic CLAUDE.md
	// preamble. Only meaningful in project mode; skip in --global.
	if initAnalyze && mode == agents.ModeProject {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "[gortex init] indexing %s...\n", absRoot)
		overview, err := generateOverview(absRoot)
		if err != nil {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "[gortex init] indexing failed: %v — using static block\n", err)
		} else {
			env.AnalyzedOverview = overview
		}
	}

	// Build the registry and pick adapters per --agents / --agents-skip.
	registry := buildRegistry()
	selected, err := registry.Filter(initAgents, initAgentsSkip)
	if err != nil {
		return err
	}

	opts := agents.ApplyOpts{DryRun: initDryRun, Force: initForce}
	results := make([]*agents.Result, 0, len(selected))
	for _, a := range selected {
		r, err := a.Apply(env, opts)
		if err != nil {
			// Claude Code is load-bearing: propagate its failures
			// up so the user sees them. Other adapters emit
			// warnings so one broken editor install doesn't abort
			// the whole init run.
			if a.Name() == claudecode.Name {
				return fmt.Errorf("%s: %w", a.Name(), err)
			}
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "[gortex init] warning: %s setup failed: %v\n", a.Name(), err)
		}
		if r != nil {
			results = append(results, r)
		}
	}

	// Always update Gortex's own global config (~/.config/gortex/config.yaml)
	// so the daemon knows about this repo next time it starts.
	if !initDryRun && mode == agents.ModeProject {
		if err := ensureGlobalConfig(absRoot); err != nil {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "[gortex init] warning: could not update global config: %v\n", err)
		}
	}

	// Global mode has two extra orchestration steps the adapters
	// can't express (daemon control plane): --start spawns the
	// daemon, --track registers this repo with it.
	if mode == agents.ModeGlobal && !initDryRun {
		if err := ensureGlobalConfigExists(); err != nil {
			return err
		}
		if err := runGlobalFollowUps(cmd, absRoot); err != nil {
			return err
		}
	}

	// Emit the report. --json to stdout for machine consumers;
	// human summary to stderr for people.
	if initJSON {
		if err := emitJSONReport(cmd.OutOrStdout(), results, opts); err != nil {
			return err
		}
	}
	emitHumanSummary(cmd.ErrOrStderr(), results, opts, mode)
	return nil
}

// emitJSONReport writes a single JSON object to w. Shape:
//
//	{
//	  "dry_run": bool,
//	  "force":   bool,
//	  "agents":  [{name, detected, configured, docs_url, files: [...]}, ...]
//	}
//
// Callers machine-read this (CI smoke matrix, gortex init doctor).
func emitJSONReport(w io.Writer, results []*agents.Result, opts agents.ApplyOpts) error {
	payload := map[string]any{
		"dry_run": opts.DryRun,
		"force":   opts.Force,
		"agents":  results,
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(payload)
}

// emitHumanSummary prints the "what got configured" footer the old
// init.go shipped. It prints one line per adapter with the count of
// files created / merged / skipped — useful for a human reading the
// terminal without parsing JSON.
func emitHumanSummary(w io.Writer, results []*agents.Result, opts agents.ApplyOpts, mode agents.Mode) {
	_, _ = fmt.Fprintf(w, "\n[gortex init] done")
	if opts.DryRun {
		_, _ = fmt.Fprintf(w, " (dry-run — no files written)")
	}
	_, _ = fmt.Fprintf(w, ":\n")
	for _, r := range results {
		if r == nil {
			continue
		}
		detected := "detected"
		if !r.Detected {
			detected = "not detected"
		}
		_, _ = fmt.Fprintf(w, "  • %s — %s, %d file(s) ", r.Name, detected, len(r.Files))
		_, _ = fmt.Fprintf(w, "[%s]\n", countByAction(r.Files))
	}
	if mode == agents.ModeProject {
		_, _ = fmt.Fprintln(w, "\nCommit .mcp.json, .claude/commands/, .claude/settings.json, CLAUDE.md, and any detected agent configs so your team gets Gortex automatically.")
		_, _ = fmt.Fprintln(w, "Run `gortex serve --index . --watch` or let your IDE start it via MCP config.")
	}
}

// countByAction renders "create=3 merge=1 skip=2" style line.
func countByAction(files []agents.FileAction) string {
	var c, m, s, wc, wm int
	for _, f := range files {
		switch f.Action {
		case agents.ActionCreate:
			c++
		case agents.ActionMerge:
			m++
		case agents.ActionSkip:
			s++
		case agents.ActionWouldCreate:
			wc++
		case agents.ActionWouldMerge:
			wm++
		}
	}
	parts := []string{}
	if c > 0 {
		parts = append(parts, fmt.Sprintf("create=%d", c))
	}
	if m > 0 {
		parts = append(parts, fmt.Sprintf("merge=%d", m))
	}
	if s > 0 {
		parts = append(parts, fmt.Sprintf("skip=%d", s))
	}
	if wc > 0 {
		parts = append(parts, fmt.Sprintf("would-create=%d", wc))
	}
	if wm > 0 {
		parts = append(parts, fmt.Sprintf("would-merge=%d", wm))
	}
	return strings.Join(parts, " ")
}

// generateOverview runs a one-shot index to produce a dynamic
// CLAUDE.md preamble (used by --analyze). Kept in this file rather
// than inside the claudecode adapter because the indexer depends on
// many gortex-internal packages we'd rather not leak across
// internal/agents/.
func generateOverview(root string) (string, error) {
	logger := newLogger()
	defer func() { _ = logger.Sync() }()

	cfg, err := config.Load("")
	if err != nil {
		cfg = &config.Config{}
	}

	g := graph.New()
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)

	idx := indexer.New(g, reg, cfg.Index, logger)
	result, err := idx.Index(root)
	if err != nil {
		return "", err
	}

	_, _ = fmt.Fprintf(os.Stderr, "[gortex init] indexed %d files (%d nodes, %d edges) in %dms\n",
		result.FileCount, result.NodeCount, result.EdgeCount, result.DurationMs)

	eng := query.NewEngine(g)
	return claudemd.Generate(eng, 180), nil
}

// ensureGlobalConfig adds this repo to ~/.config/gortex/config.yaml
// so the daemon picks it up on its next restart. Skipped in
// --dry-run and --global modes.
func ensureGlobalConfig(root string) error {
	gc, err := config.LoadGlobal()
	if err != nil {
		return err
	}
	if err := gc.AddRepo(config.RepoEntry{Path: root}); err != nil {
		return err
	}
	if err := gc.Save(); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(os.Stderr, "[gortex init] updated global config at %s\n", gc.ConfigPath())
	return nil
}
