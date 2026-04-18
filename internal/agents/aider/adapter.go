// Package aider implements the Gortex init integration for Aider.
// Aider's CLI does not speak MCP natively today; we support it
// through two lightweight artifacts:
//
//   - .aiderignore additions pointing at Gortex cache directories
//     so Aider doesn't re-index them on every chat.
//   - A brief pointer block in .aider.conf.yml documenting how to
//     call Gortex as an external tool from an Aider session.
//
// This adapter is intentionally thin — when Aider adds native MCP
// support (or when the community aider-mcp-server bridge stabilises)
// the integration can be expanded.
//
// Docs: https://aider.chat/docs/config/aider_conf.html
package aider

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/agents/internalutil"
)

const Name = "aider"
const DocsURL = "https://aider.chat/docs/config/aider_conf.html"

type Adapter struct{}

func New() *Adapter                { return &Adapter{} }
func (a *Adapter) Name() string    { return Name }
func (a *Adapter) DocsURL() string { return DocsURL }

// aiderIgnoreLines is the set of cache paths Aider should never
// ingest as source. Keeping them out of the chat avoids wasting
// tokens on Gortex's own binary index and Bleve scorer data.
var aiderIgnoreLines = []string{
	"# Added by `gortex init` — Gortex cache artifacts are not source",
	".gortex/",
	"*.gortex-cache",
}

func (a *Adapter) Detect(env agents.Env) (bool, error) {
	if p, err := exec.LookPath("aider"); err == nil && p != "" {
		return true, nil
	}
	for _, p := range []string{
		filepath.Join(env.Root, ".aider.conf.yml"),
		filepath.Join(env.Root, ".aiderignore"),
	} {
		if _, err := os.Stat(p); err == nil {
			return true, nil
		}
	}
	if env.Home != "" {
		if _, err := os.Stat(filepath.Join(env.Home, ".aider.conf.yml")); err == nil {
			return true, nil
		}
	}
	return false, nil
}

func (a *Adapter) Plan(env agents.Env) (*agents.Plan, error) {
	return &agents.Plan{Files: []agents.FileAction{{
		Path:   filepath.Join(env.Root, ".aiderignore"),
		Action: agents.ActionWouldMerge,
		Keys:   []string{"gortex-ignore-block"},
	}}}, nil
}

func (a *Adapter) Apply(env agents.Env, opts agents.ApplyOpts) (*agents.Result, error) {
	res := &agents.Result{Name: Name, DocsURL: DocsURL}
	detected, _ := a.Detect(env)
	res.Detected = detected
	if !detected {
		internalutil.Logf(env.Stderr, "[gortex init] skip Aider setup (aider not detected)")
		return res, nil
	}
	internalutil.Logf(env.Stderr, "[gortex init] setting up Aider integration...")

	action, err := appendAiderIgnoreBlock(env.Stderr, filepath.Join(env.Root, ".aiderignore"), opts)
	if err != nil {
		return res, err
	}
	res.Files = append(res.Files, action)
	res.Configured = true
	return res, nil
}

// appendAiderIgnoreBlock appends our lines to .aiderignore unless
// they're already present. We key idempotency on the comment
// sentinel — if a user ran init once already, they see it on line
// one of our block.
func appendAiderIgnoreBlock(w interface{ Write(p []byte) (n int, err error) }, path string, opts agents.ApplyOpts) (agents.FileAction, error) {
	existing, readErr := os.ReadFile(path)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return agents.FileAction{}, readErr
	}
	existed := readErr == nil
	sentinel := aiderIgnoreLines[0]
	if existed && strings.Contains(string(existing), sentinel) {
		return agents.FileAction{Path: path, Action: agents.ActionSkip, Reason: "block-present"}, nil
	}
	if opts.DryRun {
		action := agents.ActionWouldMerge
		if !existed {
			action = agents.ActionWouldCreate
		}
		return agents.FileAction{Path: path, Action: action}, nil
	}

	prefix := ""
	if existed && len(existing) > 0 && !strings.HasSuffix(string(existing), "\n") {
		prefix = "\n"
	}
	block := prefix + strings.Join(aiderIgnoreLines, "\n") + "\n"

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return agents.FileAction{}, err
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(block); err != nil {
		return agents.FileAction{}, err
	}
	_, _ = fmt.Fprintf(w, "[gortex init] appended Gortex ignore block to %s\n", path)
	action := agents.ActionMerge
	if !existed {
		action = agents.ActionCreate
	}
	return agents.FileAction{Path: path, Action: action}, nil
}
