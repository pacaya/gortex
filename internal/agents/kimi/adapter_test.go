package kimi

import (
	"os"
	"path/filepath"
	"testing"

	toml "github.com/pelletier/go-toml/v2"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/agents/agentstest"
)

const testKimiHookCommand = "/tmp/test-gortex hook --agent=kimi"

func TestKimiProjectWritesOnlyProjectMCP(t *testing.T) {
	env := kimiTestEnv(t)
	a := New()

	res, err := a.Apply(env, agents.ApplyOpts{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	agentstest.AssertCountsByAction(t, res, map[agents.ActionKind]int{agents.ActionCreate: 1})

	cfg := agentstest.ReadJSON(t, projectMCPPath(env))
	if _, ok := cfg["mcpServers"].(map[string]any)["gortex"]; !ok {
		t.Fatalf("missing gortex MCP entry: %#v", cfg)
	}
	if _, err := os.Stat(filepath.Join(kimiConfigRoot(env), "config.toml")); !os.IsNotExist(err) {
		t.Fatalf("project mode should not write user config.toml, stat err=%v", err)
	}

	agentstest.AssertIdempotent(t, a, env)
}

func TestKimiGlobalWritesMCPAndHooks(t *testing.T) {
	env := kimiTestEnv(t)
	env.Mode = agents.ModeGlobal
	a := New()

	res, err := a.Apply(env, agents.ApplyOpts{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	agentstest.AssertCountsByAction(t, res, map[agents.ActionKind]int{agents.ActionCreate: 2})

	mcp := agentstest.ReadJSON(t, filepath.Join(kimiConfigRoot(env), "mcp.json"))
	if _, ok := mcp["mcpServers"].(map[string]any)["gortex"]; !ok {
		t.Fatalf("missing global gortex MCP entry: %#v", mcp)
	}

	hooks := readKimiHooks(t, env)
	if len(hooks) != 4 {
		t.Fatalf("hooks=%#v want 4", hooks)
	}
	assertKimiGortexHooks(t, hooks, 0)

	agentstest.AssertIdempotent(t, a, env)
}

func TestKimiGlobalNoHooksWritesOnlyMCP(t *testing.T) {
	env := kimiTestEnv(t)
	env.Mode = agents.ModeGlobal
	env.InstallHooks = false
	a := New()

	res, err := a.Apply(env, agents.ApplyOpts{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	agentstest.AssertCountsByAction(t, res, map[agents.ActionKind]int{agents.ActionCreate: 1})

	if _, err := os.Stat(filepath.Join(kimiConfigRoot(env), "config.toml")); !os.IsNotExist(err) {
		t.Fatalf("--no-hooks should not write config.toml, stat err=%v", err)
	}
}

func TestKimiHookMergePreservesExistingHooks(t *testing.T) {
	env := kimiTestEnv(t)
	env.Mode = agents.ModeGlobal
	path := filepath.Join(kimiConfigRoot(env), "config.toml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	seed := `default_model = "kimi-code/kimi-for-coding"

[[hooks]]
event = "Notification"
matcher = "task\\.completed"
command = "echo user"
timeout = 5
`
	if err := os.WriteFile(path, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := New().Apply(env, agents.ApplyOpts{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	agentstest.AssertCountsByAction(t, res, map[agents.ActionKind]int{agents.ActionCreate: 1, agents.ActionMerge: 1})

	cfg := readKimiConfig(t, env)
	if cfg["default_model"] != "kimi-code/kimi-for-coding" {
		t.Fatalf("default_model was not preserved: %#v", cfg)
	}
	hooks := readKimiHooks(t, env)
	if len(hooks) != 5 {
		t.Fatalf("hooks=%#v want user+gortex hooks", hooks)
	}
	if hooks[0]["event"] != "Notification" {
		t.Fatalf("user hook was not preserved first: %#v", hooks)
	}
	assertKimiGortexHooks(t, hooks, 1)
}

func TestKimiForceReplacesOnlyGortexHook(t *testing.T) {
	env := kimiTestEnv(t)
	env.Mode = agents.ModeGlobal
	path := filepath.Join(kimiConfigRoot(env), "config.toml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	seed := `[[hooks]]
event = "Notification"
command = "echo user"

[[hooks]]
event = "UserPromptSubmit"
command = "/tmp/old-gortex hook --agent=kimi"
timeout = 30

[[hooks]]
event = "PreToolUse"
command = "/tmp/old-gortex hook --agent=kimi"
timeout = 30
`
	if err := os.WriteFile(path, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := New().Apply(env, agents.ApplyOpts{Force: true}); err != nil {
		t.Fatalf("apply force: %v", err)
	}
	hooks := readKimiHooks(t, env)
	if len(hooks) != 5 {
		t.Fatalf("hooks=%#v want user+current gortex hooks", hooks)
	}
	if hooks[0]["command"] != "echo user" {
		t.Fatalf("user hook was not preserved: %#v", hooks)
	}
	assertKimiGortexHooks(t, hooks, 1)
}

func kimiTestEnv(t *testing.T) agents.Env {
	t.Helper()
	t.Setenv("KIMI_CODE_HOME", "")
	env, _ := agentstest.NewEnv(t)
	if err := os.MkdirAll(kimiConfigRoot(env), 0o755); err != nil {
		t.Fatal(err)
	}
	return env
}

func readKimiConfig(t *testing.T, env agents.Env) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(kimiConfigRoot(env), "config.toml"))
	if err != nil {
		t.Fatalf("read config.toml: %v", err)
	}
	var cfg map[string]any
	if err := toml.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("parse config.toml: %v", err)
	}
	return cfg
}

func readKimiHooks(t *testing.T, env agents.Env) []map[string]any {
	t.Helper()
	cfg := readKimiConfig(t, env)
	raw, ok := cfg["hooks"].([]map[string]any)
	if ok {
		return raw
	}
	anyList, ok := cfg["hooks"].([]any)
	if !ok {
		t.Fatalf("hooks has unexpected shape: %#v", cfg["hooks"])
	}
	out := make([]map[string]any, 0, len(anyList))
	for _, item := range anyList {
		m, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("hook has unexpected shape: %#v", item)
		}
		out = append(out, m)
	}
	return out
}

func assertKimiHook(t *testing.T, hook map[string]any, event string, timeout int) {
	t.Helper()
	if hook["event"] != event {
		t.Errorf("event=%v want %s", hook["event"], event)
	}
	if hook["command"] != testKimiHookCommand {
		t.Errorf("command=%v want %q", hook["command"], testKimiHookCommand)
	}
	if hook["timeout"] != int64(timeout) {
		t.Errorf("timeout=%v want %d", hook["timeout"], timeout)
	}
	if _, ok := hook["matcher"]; ok {
		t.Errorf("%s hook should not write matcher: %#v", event, hook)
	}
}

// assertKimiGortexHooks checks the four gortex-managed hooks appear, in order,
// starting at hooks[offset].
func assertKimiGortexHooks(t *testing.T, hooks []map[string]any, offset int) {
	t.Helper()
	assertKimiHook(t, hooks[offset+0], "UserPromptSubmit", kimiHookTimeoutSeconds)
	assertKimiHook(t, hooks[offset+1], "PreToolUse", kimiHookTimeoutSeconds)
	assertKimiHook(t, hooks[offset+2], "Stop", kimiStopHookTimeoutSeconds)
	assertKimiHook(t, hooks[offset+3], "SubagentStart", kimiSubagentHookTimeoutSeconds)
}
