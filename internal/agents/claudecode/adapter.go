package claudecode

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zzet/gortex/internal/agents"
)

// Name is the stable identifier for this adapter, matching the
// --agents=<name> CLI flag.
const Name = "claude-code"

// DocsURL is the page we point users at when something about our
// Claude Code integration surprises them. Used in the --json report.
const DocsURL = "https://docs.claude.com/en/docs/claude-code/overview"

// Adapter implements agents.Adapter for Claude Code.
type Adapter struct{}

// New returns the Claude Code adapter. Callers register it via
// `agents.Registry.Register(claudecode.New())`.
func New() *Adapter { return &Adapter{} }

func (a *Adapter) Name() string    { return Name }
func (a *Adapter) DocsURL() string { return DocsURL }

// Detect always returns true. Claude Code is the "home" agent for
// `gortex init` — a project may not be opened in Claude Code today
// but we always want the integration files on disk so the team's
// next contributor is set up. Other adapters gate on detection
// because their artifacts make no sense without the IDE installed.
func (a *Adapter) Detect(env agents.Env) (bool, error) {
	return true, nil
}

// Plan reports the full set of files the adapter would touch for
// the given Env. Mode branches between project and global mode;
// InstallHooks elides hook files without affecting anything else.
func (a *Adapter) Plan(env agents.Env) (*agents.Plan, error) {
	p := &agents.Plan{}
	if env.Mode == agents.ModeGlobal {
		// Global mode only touches user-level files. The daemon
		// spans every project once registered, so we don't write
		// per-repo artifacts here.
		p.Files = append(p.Files, agents.FileAction{Path: userClaudeJSONPath(env.Home), Action: agents.ActionWouldMerge, Keys: []string{"mcpServers"}})
		if env.InstallHooks {
			p.Files = append(p.Files, agents.FileAction{Path: userSettingsLocalPath(env.Home), Action: agents.ActionWouldMerge, Keys: []string{"hooks"}})
		}
		return p, nil
	}

	// Project mode — the canonical shape.
	p.Files = append(p.Files, agents.FileAction{Path: filepath.Join(env.Root, ".mcp.json"), Action: agents.ActionWouldCreate, Keys: []string{"mcpServers"}})
	for name := range SlashCommands {
		p.Files = append(p.Files, agents.FileAction{Path: filepath.Join(env.Root, ".claude", "commands", name), Action: agents.ActionWouldCreate})
	}
	p.Files = append(p.Files, agents.FileAction{Path: filepath.Join(env.Root, ".claude", "settings.json"), Action: agents.ActionWouldMerge, Keys: []string{"permissions"}})
	if env.InstallHooks {
		p.Files = append(p.Files, agents.FileAction{Path: filepath.Join(env.Root, ".claude", "settings.local.json"), Action: agents.ActionWouldMerge, Keys: []string{"hooks"}})
	}
	p.Files = append(p.Files, agents.FileAction{Path: filepath.Join(env.Root, "CLAUDE.md"), Action: agents.ActionWouldMerge, Keys: []string{"gortex-block"}})
	if env.Home != "" {
		for name := range GlobalSkills {
			p.Files = append(p.Files, agents.FileAction{Path: filepath.Join(env.Home, ".claude", "skills", name, "SKILL.md"), Action: agents.ActionWouldCreate})
		}
	}
	return p, nil
}

// Apply performs the actual writes. Errors mid-way do not abort the
// whole adapter — we log each failure and continue, matching the
// pre-refactor behaviour where Kiro/Cursor/etc. setup failures only
// emit warnings. Claude Code's .mcp.json, CLAUDE.md, and hook install
// are the exception: they're the core integration and propagate
// failures upward.
func (a *Adapter) Apply(env agents.Env, opts agents.ApplyOpts) (*agents.Result, error) {
	w := env.Stderr
	res := &agents.Result{Name: Name, Detected: true, DocsURL: DocsURL}

	if env.Mode == agents.ModeGlobal {
		if err := a.applyGlobal(env, opts, res); err != nil {
			return res, err
		}
		res.Configured = true
		return res, nil
	}

	// 1. Project .mcp.json — create if absent, skip otherwise.
	mcpAction, err := agents.WriteIfNotExists(w, filepath.Join(env.Root, ".mcp.json"), ProjectMCPJSON, opts)
	if err != nil {
		return res, fmt.Errorf(".mcp.json: %w", err)
	}
	res.Files = append(res.Files, mcpAction)

	// 2. Slash commands — each file created if absent.
	for name, content := range SlashCommands {
		action, err := agents.WriteIfNotExists(w, filepath.Join(env.Root, ".claude", "commands", name), content, opts)
		if err != nil {
			return res, fmt.Errorf(".claude/commands/%s: %w", name, err)
		}
		res.Files = append(res.Files, action)
	}

	// 3. MCP permissions in .claude/settings.json — merge, not create.
	permAction, err := installPermissions(w, filepath.Join(env.Root, ".claude", "settings.json"), opts)
	if err != nil {
		logWarn(w, "could not install permissions: %v", err)
	}
	res.Files = append(res.Files, permAction)

	// 4. Hooks in .claude/settings.local.json — merge with healing.
	if env.InstallHooks {
		hookAction, err := InstallHook(w, filepath.Join(env.Root, ".claude", "settings.local.json"), opts)
		if err != nil {
			logWarn(w, "could not install hook: %v", err)
		}
		res.Files = append(res.Files, hookAction)
	} else {
		logf(w, "[gortex init] skipping hook installation (--no-hooks)")
	}

	// 5. CLAUDE.md append — project-level instructions block.
	claudeMdPath := filepath.Join(env.Root, "CLAUDE.md")
	block := ClaudeMdBlock
	if env.AnalyzeRepo && env.AnalyzedOverview != "" {
		block = env.AnalyzedOverview + "\n" + ClaudeMdBlock
	}
	claudeAction, err := appendClaudeMdBlock(w, claudeMdPath, block, opts)
	if err != nil {
		return res, fmt.Errorf("CLAUDE.md: %w", err)
	}
	res.Files = append(res.Files, claudeAction)

	// 6. Global skills — user-level ~/.claude/skills/gortex-*.
	if env.Home != "" {
		skillActions, err := installGlobalSkills(w, env.Home, opts)
		if err != nil {
			logWarn(w, "could not install global skills: %v", err)
		}
		res.Files = append(res.Files, skillActions...)
	}

	res.Configured = true
	return res, nil
}

// applyGlobal handles Mode=ModeGlobal writes. The daemon path means
// we don't create per-repo artifacts at all: the user gets a single
// user-level MCP stanza and (optionally) user-level hooks that
// apply to every project they open.
func (a *Adapter) applyGlobal(env agents.Env, opts agents.ApplyOpts, res *agents.Result) error {
	w := env.Stderr
	if env.Home == "" {
		return fmt.Errorf("global mode requires a resolved home directory")
	}

	// 1. ~/.claude.json — MCP stanza pointing at `gortex serve`.
	mcpPath := userClaudeJSONPath(env.Home)
	action, err := upsertGlobalMCPConfig(w, mcpPath, opts)
	if err != nil {
		return fmt.Errorf("global MCP config: %w", err)
	}
	res.Files = append(res.Files, action)

	// 2. ~/.claude/settings.local.json — user-level hooks.
	if env.InstallHooks {
		hookAction, err := InstallHook(w, userSettingsLocalPath(env.Home), opts)
		if err != nil {
			return fmt.Errorf("global hooks: %w", err)
		}
		res.Files = append(res.Files, hookAction)
	}
	return nil
}

// installPermissions merges an {"permissions": {"allow":
// ["mcp__gortex__*"]}} stanza into settings.json. Preserves any
// user-added entries; short-circuits when a gortex rule is already
// present.
func installPermissions(w io.Writer, settingsPath string, opts agents.ApplyOpts) (agents.FileAction, error) {
	return agents.MergeJSON(w, settingsPath, func(settings map[string]any, _ bool) (bool, error) {
		// Bail early if a gortex rule is already present.
		if perms, ok := settings["permissions"].(map[string]any); ok {
			if allow, ok := perms["allow"].([]any); ok {
				for _, entry := range allow {
					if s, ok := entry.(string); ok && strings.Contains(s, "mcp__gortex__") {
						return false, nil
					}
				}
			}
		}
		if _, ok := settings["permissions"]; !ok {
			settings["permissions"] = make(map[string]any)
		}
		perms := settings["permissions"].(map[string]any)
		if _, ok := perms["allow"]; !ok {
			perms["allow"] = []any{}
		}
		allow := perms["allow"].([]any)
		perms["allow"] = append(allow, "mcp__gortex__*")
		return true, nil
	}, opts)
}

// appendClaudeMdBlock appends the Gortex instructions to CLAUDE.md,
// creating it if missing. If the block is already present (as
// detected by the ClaudeMdSentinel substring), we skip — we do not
// append a duplicate block. This function is not JSON so we can't
// use MergeJSON; we have to hand-roll idempotency.
func appendClaudeMdBlock(w io.Writer, path, block string, opts agents.ApplyOpts) (agents.FileAction, error) {
	existing, readErr := os.ReadFile(path)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return agents.FileAction{}, fmt.Errorf("read %s: %w", path, readErr)
	}
	existed := readErr == nil
	if existed && strings.Contains(string(existing), ClaudeMdSentinel) {
		if w != nil {
			_, _ = fmt.Fprintf(w, "[gortex init] skip %s (Gortex block already present)\n", path)
		}
		return agents.FileAction{Path: path, Action: agents.ActionSkip, Reason: "block-present"}, nil
	}

	if opts.DryRun {
		action := agents.ActionWouldMerge
		if !existed {
			action = agents.ActionWouldCreate
		}
		return agents.FileAction{Path: path, Action: action, Keys: []string{"gortex-block"}}, nil
	}

	prefix := ""
	if existed && len(existing) > 0 {
		prefix = "\n\n"
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return agents.FileAction{}, err
	}
	// os.O_APPEND guarantees the write lands at the end even if
	// another process concurrently appended — we're not atomic here
	// because that's the historical behaviour and CLAUDE.md is
	// plaintext a human edits anyway.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return agents.FileAction{}, err
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(prefix + block); err != nil {
		return agents.FileAction{}, err
	}
	if w != nil {
		_, _ = fmt.Fprintf(w, "[gortex init] appended Gortex block to %s\n", path)
	}
	action := agents.ActionMerge
	if !existed {
		action = agents.ActionCreate
	}
	return agents.FileAction{Path: path, Action: action, Keys: []string{"gortex-block"}}, nil
}

// installGlobalSkills writes ~/.claude/skills/gortex-*/SKILL.md for
// each skill defined in GlobalSkills, skipping any that already
// exist (users may have customised their copy).
func installGlobalSkills(w io.Writer, home string, opts agents.ApplyOpts) ([]agents.FileAction, error) {
	out := make([]agents.FileAction, 0, len(GlobalSkills))
	skillsDir := filepath.Join(home, ".claude", "skills")
	for name, content := range GlobalSkills {
		dir := filepath.Join(skillsDir, name)
		path := filepath.Join(dir, "SKILL.md")
		action, err := agents.WriteIfNotExists(w, path, content, opts)
		if err != nil {
			return out, err
		}
		out = append(out, action)
	}
	return out, nil
}

// upsertGlobalMCPConfig is the user-level (~/.claude.json) MCP stanza
// installer. Unlike the project-level .mcp.json we never write from
// scratch with a static string here — we always merge so we don't
// clobber a user's other MCP servers or their permissions. If the
// existing file is malformed JSON, it's backed up before we
// overwrite.
func upsertGlobalMCPConfig(w io.Writer, path string, opts agents.ApplyOpts) (agents.FileAction, error) {
	exe, err := os.Executable()
	if err != nil {
		// Fall back to bare "gortex" on PATH. Reasonable for
		// homebrew / go install deployments.
		exe = "gortex"
	}
	entry := map[string]any{
		"command": exe,
		"args":    []string{"serve"},
		"env":     map[string]any{},
	}

	// Try a direct merge first. MergeJSON handles malformed JSON
	// with a timestamped backup already.
	action, err := agents.MergeJSON(w, path, func(root map[string]any, existed bool) (bool, error) {
		_ = existed
		return agents.UpsertMCPServer(root, "gortex", entry, agents.ApplyOpts{Force: opts.Force}), nil
	}, opts)
	if err != nil {
		return agents.FileAction{}, err
	}

	// Historical behaviour: if the file existed but was malformed,
	// rename it to a .bak-<ts> to preserve the original. MergeJSON
	// uses a plain ".bak" name; we mirror the timestamp-suffix
	// convention for global mode because the user is more likely
	// to have edited ~/.claude.json than a project .mcp.json.
	if existing, statErr := os.Stat(path + ".bak"); statErr == nil && !existing.IsDir() {
		if err := os.Rename(path+".bak", fmt.Sprintf("%s.bak-%d", path, time.Now().Unix())); err != nil {
			logWarn(w, "could not timestamp malformed-config backup: %v", err)
		}
	}
	return action, nil
}

// Paths — user-level files.

func userClaudeJSONPath(home string) string {
	return filepath.Join(home, ".claude.json")
}

func userSettingsLocalPath(home string) string {
	return filepath.Join(home, ".claude", "settings.local.json")
}

func logf(w io.Writer, format string, args ...any) {
	if w == nil {
		return
	}
	_, _ = fmt.Fprintf(w, format+"\n", args...)
}

func logWarn(w io.Writer, format string, args ...any) {
	if w == nil {
		return
	}
	_, _ = fmt.Fprintf(w, "[gortex init] warning: "+format+"\n", args...)
}
