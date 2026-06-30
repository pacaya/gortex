// Package pi implements the Gortex init integration for the Pi coding
// agent (earendil-works/pi, a.k.a. pi-mono).
//
// Pi is unlike every other adapter in this tree for two reasons:
//
//  1. Pi has no MCP support — by design. Instead we ship a TypeScript
//     extension that registers Gortex's graph tools natively (each
//     shelling `gortex call <tool>`) and re-creates the Claude-Code
//     read-discipline enforcement by bridging Pi lifecycle events to
//     `gortex hook --agent=pi`.
//
//  2. It is therefore has to ship executable code (the embedded
//     `extension/index.ts`) and use `go:embed` in the agents tree.
//     The extension carries four templated sentinels the adapter
//     fills in at write time — see renderExtension.
//
// Read-discipline rules are injected by the extension at runtime, not
// written to an instructions file. AGENTS.md is touched only for the
// community-routing block, and only with --skills.
//
// File layout written by the adapter:
//
//	ModeProject: <root>/.pi/extensions/gortex/index.ts   (+ AGENTS.md routing block when --skills)
//	ModeGlobal:  <home>/.pi/agent/extensions/gortex/index.ts

package pi

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	_ "embed"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/agents/internalutil"
)

const Name = "pi"
const DocsURL = "https://github.com/earendil-works/pi/blob/main/packages/coding-agent/docs/extensions.md"

//go:embed extension/index.ts
var extensionSource string

// Templated sentinels in extension/index.ts. Each is replaced with a JSON
// literal so values containing spaces or backslashes survive intact.
const (
	sentinelBin         = "{{GORTEX_BIN}}"
	sentinelArgv        = "{{GORTEX_HOOK_ARGV}}"
	sentinelEnforce     = "{{GORTEX_ENFORCE}}"
	sentinelToolsPreset = "{{GORTEX_TOOLS_PRESET}}"
)

// defaultToolsPreset is the eager tool preset baked into the extension. It
// mirrors the daemon's own default surface (corePresetTools, in defer
// mode); GORTEX_TOOLS in the environment overrides it at runtime, the same
// override the daemon honours. Kept a constant (not read from the
// environment at render time) so the rendered extension is deterministic.
const defaultToolsPreset = "core"

type Adapter struct{}

func New() *Adapter                { return &Adapter{} }
func (a *Adapter) Name() string    { return Name }
func (a *Adapter) DocsURL() string { return DocsURL }

// Detect reports whether Pi is in use: a project-local `.pi/` dir, a
// user-level `~/.pi/`, or the `pi` CLI on PATH.
func (a *Adapter) Detect(env agents.Env) (bool, error) {
	if env.Mode == agents.ModeProject && env.Root != "" {
		if _, err := os.Stat(filepath.Join(env.Root, ".pi")); err == nil {
			return true, nil
		}
	}
	if env.Home != "" {
		if _, err := os.Stat(filepath.Join(env.Home, ".pi")); err == nil {
			return true, nil
		}
	}
	for _, bin := range []string{"pi", "pi-coding-agent"} {
		if p, err := exec.LookPath(bin); err == nil && p != "" {
			return true, nil
		}
	}
	return false, nil
}

// extensionPath returns where the extension file lands for the given mode.
func extensionPath(env agents.Env) string {
	if env.Mode == agents.ModeGlobal {
		return filepath.Join(env.Home, ".pi", "agent", "extensions", "gortex", "index.ts")
	}
	return filepath.Join(env.Root, ".pi", "extensions", "gortex", "index.ts")
}

func (a *Adapter) Plan(env agents.Env) (*agents.Plan, error) {
	p := &agents.Plan{}
	p.Files = append(p.Files, agents.FileAction{
		Path:   extensionPath(env),
		Action: agents.ActionWouldCreate,
	})
	// AGENTS.md gets the community-routing block, and only with skills
	// (project mode). Read-discipline rules ride the extension, not this file.
	if env.Mode != agents.ModeGlobal && env.Root != "" && env.SkillsRouting != "" {
		p.Files = append(p.Files, agents.FileAction{
			Path:   filepath.Join(env.Root, "AGENTS.md"),
			Action: agents.ActionWouldMerge, Keys: []string{"communities-block"},
		})
	}
	return p, nil
}

func (a *Adapter) Apply(env agents.Env, opts agents.ApplyOpts) (*agents.Result, error) {
	res := &agents.Result{Name: Name, DocsURL: DocsURL}
	detected, _ := a.Detect(env)
	res.Detected = detected
	if !detected && !opts.ForceDetect {
		internalutil.Logf(env.Stderr, "[gortex init] skip Pi setup (pi not detected)")
		return res, nil
	}
	if env.Mode == agents.ModeGlobal && env.Home == "" {
		return res, fmt.Errorf("pi: global mode requires a resolved home directory")
	}
	internalutil.Logf(env.Stderr, "[gortex init] setting up Pi integration...")

	// 1. The extension (Gortex owns it end-to-end → overwrite on re-run).
	extAction, err := agents.WriteOwnedFile(env.Stderr, extensionPath(env), renderExtension(env), opts)
	if err != nil {
		return res, err
	}
	res.Files = append(res.Files, extAction)

	// 2. Community routing → AGENTS.md (project mode, skills enabled only).
	// Read-discipline rules ride the extension, so AGENTS.md is left alone
	// unless there's a routing block to merge.
	if env.Mode != agents.ModeGlobal && env.Root != "" && env.SkillsRouting != "" {
		agentsMd := filepath.Join(env.Root, "AGENTS.md")
		routingAction, err := agents.UpsertMarkedBlock(env.Stderr, agentsMd, env.SkillsRouting,
			agents.CommunitiesStartMarker, agents.CommunitiesEndMarker, opts)
		if err != nil {
			return res, err
		}
		res.Files = append(res.Files, routingAction)
	}

	res.Configured = true
	return res, nil
}

// renderExtension fills the embedded TypeScript template with the resolved
// gortex binary path, the hook argv, and the enforcement flag.
func renderExtension(env agents.Env) string {
	bin := resolveGortexBin(env)
	argv := []string{bin, "hook", "--agent=pi"}
	if mode := normalizeMode(env.HookMode); mode != "" && mode != "deny" {
		argv = append(argv, "--mode="+mode)
	}

	src := extensionSource
	src = substituteSentinel(src, sentinelBin, jsonString(bin))
	src = substituteSentinel(src, sentinelArgv, jsonValue(argv))
	src = substituteSentinel(src, sentinelEnforce, jsonValue(env.InstallHooks))
	src = substituteSentinel(src, sentinelToolsPreset, jsonString(defaultToolsPreset))
	return src
}

// resolveGortexBin prefers an explicit HookCommand binary, then a `gortex`
// on PATH, then the bare name. We only need the binary path here — the
// hook subcommand/args are appended by renderExtension.
func resolveGortexBin(env agents.Env) string {
	if env.HookCommand != "" {
		if fields := strings.Fields(env.HookCommand); len(fields) > 0 {
			return fields[0]
		}
	}
	if p, err := exec.LookPath("gortex"); err == nil && p != "" {
		return p
	}
	return "gortex"
}

// normalizeMode mirrors the hook posture strings the daemon accepts; any
// unknown value (including empty) collapses to the deny default.
func normalizeMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "enrich":
		return "enrich"
	case "consult-unlock":
		return "consult-unlock"
	case "nudge", "adaptive-nudge":
		return "nudge"
	default:
		return "deny"
	}
}

// substituteSentinel replaces a {{NAME}} placeholder with value, tolerant
// of inner whitespace. The embedded extension is plain TypeScript and may
// be run through a formatter (Prettier reflows `{{NAME}}` to `{{ NAME }}`),
// so an exact-string match would silently miss a formatted sentinel and
// ship an un-substituted template. The replacement is literal so a value
// containing `$` is not treated as a regexp expansion.
func substituteSentinel(src, sentinel, value string) string {
	name := strings.TrimSuffix(strings.TrimPrefix(sentinel, "{{"), "}}")
	re := regexp.MustCompile(`\{\{\s*` + regexp.QuoteMeta(name) + `\s*\}\}`)
	return re.ReplaceAllLiteralString(src, value)
}

// jsonString / jsonValue emit JSON literals for templating into the TS
// source. jsonString is for a bare string; jsonValue for any value.
func jsonString(s string) string { return jsonValue(s) }

func jsonValue(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "null"
	}
	return string(b)
}
