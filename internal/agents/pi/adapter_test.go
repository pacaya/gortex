package pi

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/agents/agentstest"
)

// readExtension returns the rendered extension file content for the env.
func writeAndRead(t *testing.T, env agents.Env) string {
	t.Helper()
	a := New()
	if _, err := a.Apply(env, agents.ApplyOpts{ForceDetect: true}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	data, err := os.ReadFile(extensionPath(env))
	if err != nil {
		t.Fatalf("read extension: %v", err)
	}
	return string(data)
}

func TestPiDetect(t *testing.T) {
	t.Run("project .pi dir", func(t *testing.T) {
		env, _ := agentstest.NewEnv(t)
		// NewEnv's Home is a fresh temp dir with no .pi, so detection
		// hinges on the project marker (PATH may or may not have pi).
		if err := os.MkdirAll(filepath.Join(env.Root, ".pi"), 0o755); err != nil {
			t.Fatal(err)
		}
		ok, err := New().Detect(env)
		if err != nil || !ok {
			t.Fatalf("expected detect=true with .pi/, got %v (err %v)", ok, err)
		}
	})

	t.Run("home .pi dir", func(t *testing.T) {
		env, _ := agentstest.NewEnv(t)
		if err := os.MkdirAll(filepath.Join(env.Home, ".pi"), 0o755); err != nil {
			t.Fatal(err)
		}
		ok, err := New().Detect(env)
		if err != nil || !ok {
			t.Fatalf("expected detect=true with ~/.pi/, got %v (err %v)", ok, err)
		}
	})
}

func TestPiApplyWritesExtensionAndRouting(t *testing.T) {
	env, _ := agentstest.NewEnv(t) // NewEnv seeds SkillsRouting → routing block written.
	if err := os.MkdirAll(filepath.Join(env.Root, ".pi"), 0o755); err != nil {
		t.Fatal(err)
	}

	a := New()
	res, err := a.Apply(env, agents.ApplyOpts{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !res.Detected || !res.Configured {
		t.Fatalf("expected detected+configured, got %+v", res)
	}

	// Extension written.
	ext := filepath.Join(env.Root, ".pi", "extensions", "gortex", "index.ts")
	data, err := os.ReadFile(ext)
	if err != nil {
		t.Fatalf("extension not written: %v", err)
	}
	src := string(data)

	// Sentinels must be fully substituted — no template left behind.
	for _, sentinel := range []string{sentinelBin, sentinelArgv, sentinelEnforce, sentinelToolsPreset} {
		if strings.Contains(src, sentinel) {
			t.Errorf("unsubstituted sentinel %q remains in extension", sentinel)
		}
	}
	// NewEnv sets InstallHooks=true → ENFORCE true.
	if !strings.Contains(src, "const ENFORCE: boolean = true;") {
		t.Errorf("expected ENFORCE true with InstallHooks=true")
	}
	// argv must carry the pi agent flag.
	if !strings.Contains(src, `"--agent=pi"`) {
		t.Errorf("expected --agent=pi in hook argv; got source without it")
	}

	// AGENTS.md carries the community-routing block — but NOT the
	// read-discipline rules: those are injected by the extension at runtime,
	// not persisted to an instructions file (mirrors opencode).
	agentsMd, err := os.ReadFile(filepath.Join(env.Root, "AGENTS.md"))
	if err != nil {
		t.Fatalf("AGENTS.md not written: %v", err)
	}
	if !strings.Contains(string(agentsMd), agents.CommunitiesStartMarker) {
		t.Errorf("AGENTS.md missing communities block")
	}
	if strings.Contains(string(agentsMd), agents.InstructionsSentinel) {
		t.Errorf("AGENTS.md must NOT carry the read-discipline rules block — the extension injects them at runtime")
	}

	// Idempotent re-run.
	agentstest.AssertIdempotent(t, a, env)
}

// Without skills routing, the adapter writes the extension only and never
// touches AGENTS.md — the read-discipline rules ride the extension's
// `context` hook, so there's nothing to persist to an instructions file.
func TestPiApplyNoSkillsLeavesAgentsMdUntouched(t *testing.T) {
	env, _ := agentstest.NewEnv(t)
	env.SkillsRouting = ""
	if err := os.MkdirAll(filepath.Join(env.Root, ".pi"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := New().Apply(env, agents.ApplyOpts{}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if _, err := os.Stat(filepath.Join(env.Root, "AGENTS.md")); err == nil {
		t.Errorf("AGENTS.md should not be written when skills routing is empty")
	}
}

func TestPiNoHooksDisablesEnforcement(t *testing.T) {
	env, _ := agentstest.NewEnv(t)
	env.InstallHooks = false
	if err := os.MkdirAll(filepath.Join(env.Root, ".pi"), 0o755); err != nil {
		t.Fatal(err)
	}
	src := writeAndRead(t, env)
	if !strings.Contains(src, "const ENFORCE: boolean = false;") {
		t.Errorf("expected ENFORCE false with InstallHooks=false")
	}
	// Tools are still registered regardless of enforcement.
	if !strings.Contains(src, "registerGortexTools") {
		t.Errorf("tool registration should remain present with --no-hooks")
	}
}

func TestPiEnrichModeAppendsModeFlag(t *testing.T) {
	env, _ := agentstest.NewEnv(t)
	env.HookMode = "enrich"
	if err := os.MkdirAll(filepath.Join(env.Root, ".pi"), 0o755); err != nil {
		t.Fatal(err)
	}
	var sawEnrich bool
	for _, arg := range parseArgv(t, writeAndRead(t, env)) {
		if arg == "--mode=enrich" {
			sawEnrich = true
		}
	}
	if !sawEnrich {
		t.Errorf("expected --mode=enrich in hook argv for enrich posture")
	}

	// Default (deny) posture must NOT append a --mode flag to the argv.
	// (Check the parsed argv, not the raw source — the template's doc
	// comment legitimately mentions "--mode=<mode>".)
	env2, _ := agentstest.NewEnv(t)
	if err := os.MkdirAll(filepath.Join(env2.Root, ".pi"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, arg := range parseArgv(t, writeAndRead(t, env2)) {
		if strings.HasPrefix(arg, "--mode=") {
			t.Errorf("deny posture should not append a --mode flag, got argv arg %q", arg)
		}
	}
}

func TestPiGlobalMode(t *testing.T) {
	env, _ := agentstest.NewEnv(t)
	env.Mode = agents.ModeGlobal
	if err := os.MkdirAll(filepath.Join(env.Home, ".pi"), 0o755); err != nil {
		t.Fatal(err)
	}
	a := New()
	res, err := a.Apply(env, agents.ApplyOpts{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !res.Configured {
		t.Fatal("expected configured in global mode")
	}
	// Global extension lands under ~/.pi/agent/extensions; no AGENTS.md.
	if _, err := os.ReadFile(filepath.Join(env.Home, ".pi", "agent", "extensions", "gortex", "index.ts")); err != nil {
		t.Fatalf("global extension not written: %v", err)
	}
	if _, err := os.Stat(filepath.Join(env.Root, "AGENTS.md")); err == nil {
		t.Errorf("global mode should not write repo AGENTS.md")
	}
}

func TestPiApplyDryRun(t *testing.T) {
	env, _ := agentstest.NewEnv(t)
	if err := os.MkdirAll(filepath.Join(env.Root, ".pi"), 0o755); err != nil {
		t.Fatal(err)
	}
	res, err := New().Apply(env, agents.ApplyOpts{DryRun: true})
	if err != nil {
		t.Fatalf("apply dry-run: %v", err)
	}
	// Nothing written to disk under dry-run.
	if _, err := os.Stat(extensionPath(env)); err == nil {
		t.Errorf("dry-run must not write the extension file")
	}
	if len(res.Files) == 0 {
		t.Errorf("dry-run should still report planned actions")
	}
}

// parseArgv extracts and JSON-decodes the HOOK_ARGV literal templated into
// the rendered extension. It doubles as a guard that the argv sentinel
// round-trips into a valid string array (so the TS `HOOK_ARGV[0]` access
// is sound).
func parseArgv(t *testing.T, src string) []string {
	t.Helper()
	const marker = "const HOOK_ARGV: string[] = "
	i := strings.Index(src, marker)
	if i < 0 {
		t.Fatal("HOOK_ARGV line not found")
	}
	rest := src[i+len(marker):]
	end := strings.Index(rest, ";")
	if end < 0 {
		t.Fatal("HOOK_ARGV line not terminated")
	}
	var argv []string
	if err := json.Unmarshal([]byte(rest[:end]), &argv); err != nil {
		t.Fatalf("HOOK_ARGV is not valid JSON array: %v", err)
	}
	return argv
}

func TestPiRendersSearchToolAndPreset(t *testing.T) {
	env, _ := agentstest.NewEnv(t)
	// Render-time default must not depend on the caller's environment.
	t.Setenv("GORTEX_TOOLS", "")
	src := renderExtension(env)

	// B: the on-demand discovery meta-tool that mirrors tools_search.
	if !strings.Contains(src, "registerSearchTool(pi)") {
		t.Error("expected registerSearchTool to be wired into the entry point")
	}
	if !strings.Contains(src, `"tools", "search", query`) {
		t.Error("expected the meta-tool to shell `gortex tools search`")
	}

	// Every Gortex tool (incl. the meta-tool) is namespaced under gortex_ so
	// it can't silently clobber a user's own tool of the same name.
	if !strings.Contains(src, `const TOOL_PREFIX = "gortex_"`) {
		t.Error("expected a gortex_ tool-name prefix")
	}
	if !strings.Contains(src, `piToolName("tools_search")`) {
		t.Error("expected the search meta-tool name to be derived via the gortex_ prefix")
	}

	// C: the eager preset is configurable; the baked default is core and
	// the runtime honours GORTEX_TOOLS.
	if !strings.Contains(src, `const TOOLS_PRESET: string = ((process.env && process.env.GORTEX_TOOLS) || "core").trim();`) {
		t.Error("expected TOOLS_PRESET to default to \"core\" and honour GORTEX_TOOLS at runtime")
	}
	if strings.Contains(src, `"--preset", "core", "--format"`) {
		t.Error("discovery must no longer hardcode --preset core; it should route through presetListArgs()")
	}
}

// TestPiRegistersToolsPerSession guards the /new regression: Pi resets the
// session tool registry on every session_start, so tools must be
// (re)registered there — registering only at factory load loses them after
// the first session. The persistent name guard must be cleared first or
// re-registration is suppressed into the new (empty) registry.
func TestPiRegistersToolsPerSession(t *testing.T) {
	env, _ := agentstest.NewEnv(t)
	src := renderExtension(env)

	// Registration happens inside a session_start handler.
	sIdx := strings.Index(src, `pi.on("session_start"`)
	if sIdx < 0 {
		t.Fatal("expected a session_start handler")
	}
	// Bound the search to the handler body so we assert these calls live
	// *inside* session_start, not merely somewhere in the file.
	body := src[sIdx:]
	if end := strings.Index(body, "pi.on(\"before_agent_start\""); end > 0 {
		body = body[:end]
	}
	for _, want := range []string{"gortexToolNames.clear()", "registerGortexTools(pi)", "registerSearchTool(pi)", "ensureDaemon()"} {
		if !strings.Contains(body, want) {
			t.Errorf("expected %q inside the session_start handler (per-session re-registration)", want)
		}
	}
}

// TestPiInjectsOrientationViaContextHook guards the cache-safe injection
// contract: the orientation rides a `context`-hook tail user message, never
// a systemPrompt mutation (which would bust prefix prompt caching).
func TestPiInjectsOrientationViaContextHook(t *testing.T) {
	env, _ := agentstest.NewEnv(t)
	src := renderExtension(env)

	// A `context` hook must exist and push a user message.
	if !strings.Contains(src, `pi.on("context"`) {
		t.Error("expected a context hook to inject the orientation")
	}
	if !strings.Contains(src, `event.messages.push`) {
		t.Error("expected the context hook to append a message to event.messages")
	}
	// The decision field is `orientation`, matching the Go PiDecision.
	if !strings.Contains(src, "decision.orientation") {
		t.Error("expected the extension to read decision.orientation")
	}
	// before_agent_start must NOT fold the orientation into systemPrompt —
	// that path is what we removed for cache safety.
	if strings.Contains(src, "systemPrompt: (event") {
		t.Error("orientation must not be appended to systemPrompt (breaks prompt caching)")
	}

	// Orientation injection is unconditional; only tool_call enforcement is
	// gated by ENFORCE. Assert the `if (!ENFORCE) return;` guard sits AFTER
	// the before_agent_start + context registrations and BEFORE tool_call —
	// so --no-hooks still teaches the model how to use Gortex.
	guard := strings.Index(src, "if (!ENFORCE) return;")
	beforeAgent := strings.Index(src, `pi.on("before_agent_start"`)
	context := strings.Index(src, `pi.on("context"`)
	toolCall := strings.Index(src, `pi.on("tool_call"`)
	if guard < 0 || beforeAgent < 0 || context < 0 || toolCall < 0 {
		t.Fatalf("missing a required handler/guard (guard=%d before_agent_start=%d context=%d tool_call=%d)",
			guard, beforeAgent, context, toolCall)
	}
	if beforeAgent >= guard || context >= guard || guard >= toolCall {
		t.Errorf("ENFORCE guard misplaced: orientation hooks must precede it and tool_call must follow it "+
			"(before_agent_start=%d context=%d guard=%d tool_call=%d)", beforeAgent, context, guard, toolCall)
	}
}

func TestRenderedExtensionParsesArgv(t *testing.T) {
	env, _ := agentstest.NewEnv(t)
	argv := parseArgv(t, renderExtension(env))
	if len(argv) < 3 || argv[1] != "hook" || argv[2] != "--agent=pi" {
		t.Errorf("unexpected argv: %v", argv)
	}
}
