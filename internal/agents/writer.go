package agents

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
)

// writer.go centralises every write `gortex init` performs. Going
// through one helper lets us:
//
//  1. Make writes atomic — temp file + rename. Partial failures
//     can't leave a half-written MCP config that breaks the editor.
//  2. Respect ApplyOpts.DryRun uniformly. No adapter needs its own
//     "would this run?" branch.
//  3. Report what happened in a structured FileAction so `--json`
//     and the doctor subcommand speak the same vocabulary.
//  4. Power golden-fixture tests — the test harness points the
//     "root" at a temp dir, runs Apply, and diffs the written tree
//     against testdata/ golden files.

// WriteIfNotExists writes content to path when it doesn't exist.
// Used for static artifacts (slash-command markdown, Kiro steering,
// KI metadata) where merging isn't meaningful. When the file is
// already present we emit ActionSkip with Reason="exists" rather
// than silently overwriting.
//
// Under DryRun: no disk write. Returns ActionWouldCreate for a
// missing file, ActionSkip for an existing one.
//
// Directories are created as needed with 0o755.
func WriteIfNotExists(w io.Writer, path, content string, opts ApplyOpts) (FileAction, error) {
	if _, err := os.Stat(path); err == nil {
		logf(w, "[gortex init] skip %s (already exists)", path)
		return FileAction{Path: path, Action: ActionSkip, Reason: "exists"}, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return FileAction{Path: path, Action: ActionSkip, Reason: err.Error()}, fmt.Errorf("stat %s: %w", path, err)
	}

	if opts.DryRun {
		return FileAction{Path: path, Action: ActionWouldCreate}, nil
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return FileAction{}, fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}
	if err := AtomicWriteFile(path, []byte(content), 0o644); err != nil {
		return FileAction{}, err
	}
	logf(w, "[gortex init] created %s", path)
	return FileAction{Path: path, Action: ActionCreate}, nil
}

// AtomicWriteFile writes data to path via a temp file in the same
// directory followed by a rename. Guarantees that a concurrent reader
// either sees the old file or the fully-written new file — never a
// half-written state.
//
// The temp file uses a deterministic prefix so a crash leaves
// "<name>.gortex.tmp-<pid>.<rand>" files that are easy to identify
// and clean up manually.
func AtomicWriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	f, err := os.CreateTemp(dir, filepath.Base(path)+".gortex.tmp-*")
	if err != nil {
		return fmt.Errorf("create temp in %s: %w", dir, err)
	}
	tmpPath := f.Name()
	// Best-effort cleanup on failure. We deliberately ignore the
	// error: if the rename succeeds the file no longer exists, and
	// if something else goes wrong the user can remove the temp by
	// hand.
	cleanup := func() { _ = os.Remove(tmpPath) }

	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		cleanup()
		return fmt.Errorf("write %s: %w", tmpPath, err)
	}
	if err := f.Chmod(perm); err != nil {
		_ = f.Close()
		cleanup()
		return fmt.Errorf("chmod %s: %w", tmpPath, err)
	}
	if err := f.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		cleanup()
		return fmt.Errorf("rename %s -> %s: %w", tmpPath, path, err)
	}
	return nil
}

// MergeJSON reads path (if present), parses it as a JSON object,
// passes the parsed map to mutate, and writes the result back
// atomically when mutate reports a change. A nil or malformed file
// is treated as empty — a backup is written alongside the original
// before we overwrite garbage, so a user with a broken config doesn't
// lose their edits.
//
// mutate returns:
//   - changed=true if the map was modified and should be written
//   - changed=false if no change is needed (idempotent re-run); we
//     still return a FileAction describing the skip for --json
//
// Keys is collected from the top-level map keys after mutation —
// useful for the --json report but not semantically load-bearing.
func MergeJSON(w io.Writer, path string, mutate func(root map[string]any, existed bool) (changed bool, err error), opts ApplyOpts) (FileAction, error) {
	existed := false
	root := make(map[string]any)
	var backupPath string

	if data, err := os.ReadFile(path); err == nil {
		existed = true
		if err := json.Unmarshal(data, &root); err != nil {
			// Don't silently overwrite the user's file even if it's
			// malformed — keep a timestamped backup for recovery.
			backupPath = path + ".bak"
			// Intentionally ignore: backup is best-effort.
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

	out, err := json.MarshalIndent(root, "", "  ")
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
		logf(w, "[gortex init] %s was malformed; backup saved to %s", path, backupPath)
	}
	logf(w, "[gortex init] %s %s", actionVerb(action), path)
	return FileAction{Path: path, Action: action, Keys: keys}, nil
}

// actionVerb renders an ActionKind for human-readable log lines.
// Kept distinct from the on-the-wire string so we can tweak messaging
// without breaking JSON consumers.
func actionVerb(a ActionKind) string {
	switch a {
	case ActionCreate:
		return "created"
	case ActionMerge:
		return "merged into"
	case ActionSkip:
		return "skipped"
	case ActionWouldCreate:
		return "would create"
	case ActionWouldMerge:
		return "would merge into"
	}
	return string(a)
}

func sortedMapKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// logf writes a newline-terminated message when w is non-nil. Shared
// helper so adapters don't each need to guard for a nil stderr in
// tests.
func logf(w io.Writer, format string, args ...any) {
	if w == nil {
		return
	}
	_, _ = fmt.Fprintf(w, format+"\n", args...)
}
