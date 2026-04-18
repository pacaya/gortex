package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/zzet/gortex/internal/daemon"
)

// runGlobalFollowUps performs the post-setup daemon control-plane
// operations for `gortex init --global`:
//
//   - --start spawns the daemon detached
//   - --track registers the current repo with the running daemon
//
// These don't fit the Adapter interface because they touch the
// daemon's RPC protocol, not on-disk config files.
func runGlobalFollowUps(cmd *cobra.Command, absRoot string) error {
	w := cmd.ErrOrStderr()

	if initStartDaemon {
		if daemon.IsRunning() {
			_, _ = fmt.Fprintln(w, "[gortex init --global] daemon already running (skipped --start)")
		} else {
			if err := spawnDetachedDaemon(); err != nil {
				return fmt.Errorf("start daemon: %w", err)
			}
			_, _ = fmt.Fprintln(w, "[gortex init --global] daemon started (detached)")
		}
	}

	if initTrackRepo {
		if !daemon.IsRunning() {
			_, _ = fmt.Fprintln(w, "[gortex init --global] ⚠ skipping --track: daemon is not running (try `gortex daemon start`)")
		} else {
			resp, err := trackViaDaemon(absRoot)
			if err != nil {
				return fmt.Errorf("track %s: %w", absRoot, err)
			}
			_, _ = fmt.Fprintf(w, "[gortex init --global] tracked %s (%s)\n", absRoot, resp)
		}
	}

	if !initStartDaemon {
		_, _ = fmt.Fprintln(w, "\nNext: start the daemon → `gortex daemon start --detach`")
	}
	if !initTrackRepo && initStartDaemon {
		_, _ = fmt.Fprintln(w, "Then: track this repo → `gortex track .` (or open Claude Code here and follow the suggestion)")
	}
	return nil
}

// ensureGlobalConfigExists creates an empty ~/.config/gortex/config.yaml
// when none is present. The daemon needs a writable path on first
// Track; creating it now surfaces any permission problems at init
// time instead of on the first use.
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
	return os.WriteFile(path, []byte(defaultGlobalConfigStub), 0o600)
}

// defaultGlobalConfigStub is the on-disk shape of a fresh global
// config. It documents the skip-embedding defaults inline so users
// don't have to dig through source to know what's being skipped.
const defaultGlobalConfigStub = `active_project: ""
repos: []
projects: {}

# Global ignore list. Layered under builtin (always applies) and above
# per-repo entries and workspace .gortex.yaml. Gitignore semantics;
# use "!pattern" in a later layer to re-include.
exclude: []

# Semantic search tuning.
semantic:
  # Node (language, kind) pairs skipped during vector-index construction.
  # They stay queryable by name/kind/filepath — only semantic search is
  # turned off. Reclaim ~hundreds of MiB on monorepos heavy in CSS
  # tokens or terraform resources.
  skip_embed:
    - language: css
      kinds: [variable, type]   # custom properties, class/id selectors
    - language: hcl
      kinds: [type, variable]   # terraform resources, locals, variables
    - language: yaml
      kinds: [variable]         # yaml keys
    - language: toml
      kinds: [variable]         # toml keys
    - language: bash
      kinds: [variable]         # shell variables
`

// trackViaDaemon opens a control-mode client and issues a Track for
// the given absolute path. Returns a human-readable status for
// display.
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
	return string(resp.Result), nil
}
