package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/zzet/gortex/internal/daemon"
)

// runInitGlobal installs user-level Claude Code configuration so every
// project Claude Code opens picks up Gortex via the shared daemon. Skips
// per-repo file creation — that's what the default `gortex init` does.
//
// Three things land on disk:
//
//  1. ~/.claude.json — MCP server stanza pointing at `gortex serve`. The
//     serve command auto-detects the daemon via stdio when Claude Code
//     spawns it, so no extra flags are needed in the config.
//  2. ~/.claude/settings.local.json — PreToolUse / PreCompact / Stop /
//     Task hooks at the user level. Applies to every project Claude Code
//     opens, not just this repo.
//  3. ~/.config/gortex/config.yaml — empty scaffold if absent. The
//     daemon's tracked-repo list lives here.
//
// --start spawns the daemon detached after setup. --track runs
// `gortex track .` once the daemon is up so the current cwd is
// immediately queryable.
func runInitGlobal(cmd *cobra.Command, root string) error {
	abs, err := filepath.Abs(root)
	if err != nil {
		return fmt.Errorf("resolve path: %w", err)
	}

	steps := []string{}

	// Step 1 — user-level MCP config.
	mcpPath, err := userClaudeJSON()
	if err != nil {
		return err
	}
	if err := upsertGlobalMCPConfig(mcpPath); err != nil {
		return err
	}
	steps = append(steps, fmt.Sprintf("wrote MCP config: %s", mcpPath))

	// Step 2 — user-level hooks (skipped when --no-hooks or wizard said no).
	if initInstallHooks {
		hookPath, err := userSettingsLocal()
		if err != nil {
			return err
		}
		if err := installHook(hookPath); err != nil {
			return err
		}
		steps = append(steps, fmt.Sprintf("installed hooks: %s", hookPath))
	} else {
		steps = append(steps, "skipped hooks (--no-hooks)")
	}

	// Step 3 — global config scaffold.
	if err := ensureGlobalConfigExists(); err != nil {
		return err
	}

	// Step 4 (optional) — start the daemon.
	if initStartDaemon {
		if daemon.IsRunning() {
			steps = append(steps, "daemon already running (skipped --start)")
		} else {
			if err := spawnDetachedDaemon(); err != nil {
				return fmt.Errorf("start daemon: %w", err)
			}
			steps = append(steps, "daemon started (detached)")
		}
	}

	// Step 5 (optional) — track the current repo via the daemon.
	if initTrackRepo {
		if !daemon.IsRunning() {
			steps = append(steps, "⚠ skipping --track: daemon is not running (try `gortex daemon start`)")
		} else {
			resp, err := trackViaDaemon(abs)
			if err != nil {
				return fmt.Errorf("track %s: %w", abs, err)
			}
			steps = append(steps, fmt.Sprintf("tracked %s (%s)", abs, resp))
		}
	}

	w := cmd.ErrOrStderr()
	_, _ = fmt.Fprintf(w,"[gortex init --global] done:\n")
	for _, s := range steps {
		_, _ = fmt.Fprintf(w,"  • %s\n", s)
	}
	if !initStartDaemon {
		_, _ = fmt.Fprintf(w,"\nNext: start the daemon → `gortex daemon start --detach`\n")
	}
	if !initTrackRepo && initStartDaemon {
		_, _ = fmt.Fprintf(w,"Then: track this repo → `gortex track .` (or open Claude Code here and follow the suggestion)\n")
	}
	return nil
}

// userClaudeJSON returns the path to ~/.claude.json with directory
// creation as needed. Claude Code's user-level MCP config lives here
// and applies to every project the user opens.
func userClaudeJSON() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home: %w", err)
	}
	return filepath.Join(home, ".claude.json"), nil
}

// userSettingsLocal returns the path to ~/.claude/settings.local.json
// — the user-level hook config file that installHook() knows how to
// manage.
func userSettingsLocal() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home: %w", err)
	}
	dir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("ensure ~/.claude: %w", err)
	}
	return filepath.Join(dir, "settings.local.json"), nil
}

// upsertGlobalMCPConfig writes or merges a `gortex` stanza into
// ~/.claude.json. Preserves any other mcpServers the user may have
// configured; adds `gortex` under mcpServers without touching siblings.
//
// The chosen command — `gortex serve` with no args — lets the binary
// auto-detect daemon / embedded mode per stdio invocation.
func upsertGlobalMCPConfig(path string) error {
	existing := make(map[string]any)
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &existing); err != nil {
			// Treat a malformed file as empty — we'd rather overwrite
			// garbage than refuse to set up the user. Back it up first.
			_ = os.Rename(path, path+".bak-"+fmt.Sprint(time.Now().Unix()))
			existing = make(map[string]any)
		}
	}
	servers, _ := existing["mcpServers"].(map[string]any)
	if servers == nil {
		servers = make(map[string]any)
	}
	exe, err := os.Executable()
	if err != nil {
		// If we can't resolve our own path for some reason, at least let
		// the config point at whatever `gortex` is on PATH. Still works
		// for typical installs.
		exe = "gortex"
	}
	servers["gortex"] = map[string]any{
		"command": exe,
		"args":    []string{"serve"},
		"env": map[string]any{
			// Env vars placeholder for future daemon-aware settings.
		},
	}
	existing["mcpServers"] = servers

	out, err := json.MarshalIndent(existing, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal mcp config: %w", err)
	}
	return os.WriteFile(path, out, 0o644)
}

// ensureGlobalConfigExists creates an empty ~/.config/gortex/config.yaml
// when none is present. The daemon needs a writable path on first Track;
// creating it now surfaces any permission problems at init time instead
// of on the first use.
func ensureGlobalConfigExists() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home: %w", err)
	}
	dir := filepath.Join(home, ".config", "gortex")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("ensure config dir: %w", err)
	}
	path := filepath.Join(dir, "config.yaml")
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	// A stub that yaml.v3 accepts as a valid GlobalConfig.
	return os.WriteFile(path, []byte("active_project: \"\"\nrepos: []\nprojects: {}\n"), 0o600)
}

// trackViaDaemon opens a control-mode client and issues a Track for the
// given absolute path. Returns a human-readable status for display.
func trackViaDaemon(absPath string) (string, error) {
	c, err := daemon.Dial(daemon.Handshake{Mode: daemon.ModeControl, ClientName: "cli"})
	if err != nil {
		return "", err
	}
	defer func() { _ = c.Close() }()
	resp, err := c.Control(daemon.ControlTrack, daemon.TrackParams{Path: absPath})
	if err != nil {
		return "", err
	}
	if !resp.OK {
		return "", fmt.Errorf("%s: %s", resp.ErrorCode, resp.ErrorMsg)
	}
	// The Result is a small JSON object; show a compact version.
	return string(resp.Result), nil
}

// silenceInitGlobalImports ensures the compiler keeps imports we use
// conditionally (cobra for the receiver type). Kept as a package-level
// to avoid surprising "imported but not used" bugs when the file grows.
var _ = cobra.Command{}
