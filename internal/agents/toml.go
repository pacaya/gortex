package agents

import (
	"errors"
	"fmt"
	"io"
	"os"

	toml "github.com/pelletier/go-toml/v2"
)

// MergeTOML is the TOML cousin of MergeJSON: read an existing
// TOML file (if any), pass the decoded map to mutate, and write
// back atomically when mutate reports a change. Used by the Codex
// CLI adapter — Codex config lives in ~/.codex/config.toml and no
// other adapter currently needs TOML support.
//
// Malformed TOML is preserved as a .bak sibling before we
// overwrite, same policy as MergeJSON.
func MergeTOML(w io.Writer, path string, mutate func(root map[string]any, existed bool) (changed bool, err error), opts ApplyOpts) (FileAction, error) {
	existed := false
	root := make(map[string]any)
	var backupPath string

	if data, err := os.ReadFile(path); err == nil {
		existed = true
		if err := toml.Unmarshal(data, &root); err != nil {
			backupPath = path + ".bak"
			_ = os.WriteFile(backupPath, data, 0o644)
			root = make(map[string]any)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return FileAction{}, fmt.Errorf("read %s: %w", path, err)
	}

	changed, err := mutate(root, existed)
	if err != nil {
		return FileAction{}, err
	}
	if !changed {
		return FileAction{Path: path, Action: ActionSkip, Reason: "already-configured"}, nil
	}

	keys := sortedMapKeys(root)

	if opts.DryRun {
		action := ActionWouldCreate
		if existed {
			action = ActionWouldMerge
		}
		return FileAction{Path: path, Action: action, Keys: keys}, nil
	}

	out, err := toml.Marshal(root)
	if err != nil {
		return FileAction{}, fmt.Errorf("marshal %s: %w", path, err)
	}
	if err := AtomicWriteFile(path, out, 0o644); err != nil {
		return FileAction{}, err
	}

	action := ActionCreate
	if existed {
		action = ActionMerge
	}
	if backupPath != "" {
		logf(w, "[gortex init] %s was malformed TOML; backup saved to %s", path, backupPath)
	}
	logf(w, "[gortex init] %s %s", actionVerb(action), path)
	return FileAction{Path: path, Action: action, Keys: keys}, nil
}
