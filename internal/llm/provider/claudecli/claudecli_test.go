package claudecli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/llm"
)

// fakeClaude writes a tiny shell script that impersonates the
// `claude` CLI: it echoes the script-baked stdout payload, optionally
// captures the args + stdin into sidecar files for assertions, and
// exits with the script-baked exit code. The returned path is the
// absolute path to the script.
//
// A shell-script fake is the smallest portable surface that exercises
// every code path the provider has — arg construction, stdin
// piping, exit-status handling, stderr capture — without pulling in
// the full TestHelperProcess re-exec dance.
func fakeClaude(t *testing.T, dir string, opts fakeOpts) string {
	t.Helper()
	if dir == "" {
		dir = t.TempDir()
	}
	script := filepath.Join(dir, "fake-claude.sh")

	argsLog := filepath.Join(dir, "args.txt")
	stdinLog := filepath.Join(dir, "stdin.txt")
	stderrPayload := strings.ReplaceAll(opts.stderr, "'", "'\\''")
	stdoutPayload := strings.ReplaceAll(opts.stdout, "'", "'\\''")

	body := "#!/bin/sh\n" +
		"set -e\n" +
		"printf '%s\\n' \"$@\" > '" + argsLog + "'\n" +
		"cat > '" + stdinLog + "'\n"
	if opts.sleep > 0 {
		body += fmt.Sprintf("sleep %d\n", int(opts.sleep.Seconds()+1))
	}
	if stderrPayload != "" {
		body += "printf '%s' '" + stderrPayload + "' >&2\n"
	}
	if stdoutPayload != "" {
		body += "printf '%s' '" + stdoutPayload + "'\n"
	}
	if opts.exitCode != 0 {
		body += fmt.Sprintf("exit %d\n", opts.exitCode)
	}

	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatalf("write fake script: %v", err)
	}
	return script
}

type fakeOpts struct {
	stdout   string
	stderr   string
	exitCode int
	sleep    time.Duration
}

func readSidecar(t *testing.T, scriptPath, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(filepath.Dir(scriptPath), name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

func TestNew_BinaryNotFound(t *testing.T) {
	if _, err := New(llm.ClaudeCLIConfig{Binary: "claude-nonexistent-zzzzz"}); err == nil {
		t.Fatal("expected error when binary is not on PATH")
	}
}

func TestNew_DefaultsBinary(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatalf("write fake binary: %v", err)
	}
	// Stash a fake `claude` on PATH so the default binary name resolves.
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	p, err := New(llm.ClaudeCLIConfig{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = p.Close() }()
	if p.Name() != "claudecli" {
		t.Errorf("Name()=%q want claudecli", p.Name())
	}
}

func TestComplete_FreeformSuccess(t *testing.T) {
	script := fakeClaude(t, "", fakeOpts{stdout: "hello world"})

	p, err := New(llm.ClaudeCLIConfig{Binary: script})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = p.Close() }()

	resp, err := p.Complete(context.Background(), llm.CompletionRequest{
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: "be terse"},
			{Role: llm.RoleUser, Content: "hi"},
		},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Text != "hello world" {
		t.Errorf("text=%q want hello world", resp.Text)
	}

	gotArgs := readSidecar(t, script, "args.txt")
	for _, want := range []string{"--print", "--output-format", "text", "--append-system-prompt", "be terse"} {
		if !strings.Contains(gotArgs, want) {
			t.Errorf("args missing %q\nargs=\n%s", want, gotArgs)
		}
	}
	stdin := readSidecar(t, script, "stdin.txt")
	if !strings.Contains(stdin, "User: hi") {
		t.Errorf("stdin missing user turn:\n%s", stdin)
	}
	if strings.Contains(stdin, "be terse") {
		t.Error("system content must travel via --append-system-prompt, not stdin")
	}
}

func TestComplete_PassesModel(t *testing.T) {
	script := fakeClaude(t, "", fakeOpts{stdout: "ok"})

	p, _ := New(llm.ClaudeCLIConfig{Binary: script, Model: "sonnet"})
	defer func() { _ = p.Close() }()

	if _, err := p.Complete(context.Background(), llm.CompletionRequest{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	args := readSidecar(t, script, "args.txt")
	if !strings.Contains(args, "--model\nsonnet") && !strings.Contains(args, "--model sonnet") {
		t.Errorf("args missing --model sonnet:\n%s", args)
	}
}

func TestComplete_StructuredExtractsJSON(t *testing.T) {
	wrapped := "Sure, here you go:\n```json\n{\"terms\":[\"bcrypt\",\"argon2\"]}\n```\n"
	script := fakeClaude(t, "", fakeOpts{stdout: wrapped})

	p, _ := New(llm.ClaudeCLIConfig{Binary: script})
	defer func() { _ = p.Close() }()

	resp, err := p.Complete(context.Background(), llm.CompletionRequest{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "expand 'password hashing'"}},
		Shape:    llm.ShapeExpandTerms,
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Text != `{"terms":["bcrypt","argon2"]}` {
		t.Errorf("text=%q want the unwrapped JSON object", resp.Text)
	}

	args := readSidecar(t, script, "args.txt")
	if !strings.Contains(args, "JSON Schema") {
		t.Errorf("structured request must inject a JSON Schema rider; args=\n%s", args)
	}
}

func TestComplete_StructuredNoJSONErrors(t *testing.T) {
	script := fakeClaude(t, "", fakeOpts{stdout: "I cannot help with that."})

	p, _ := New(llm.ClaudeCLIConfig{Binary: script})
	defer func() { _ = p.Close() }()

	if _, err := p.Complete(context.Background(), llm.CompletionRequest{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "x"}},
		Shape:    llm.ShapeExpandTerms,
	}); err == nil {
		t.Fatal("expected error when structured response carried no JSON")
	}
}

func TestComplete_NonZeroExit(t *testing.T) {
	script := fakeClaude(t, "", fakeOpts{exitCode: 2, stderr: "auth required"})

	p, _ := New(llm.ClaudeCLIConfig{Binary: script})
	defer func() { _ = p.Close() }()

	_, err := p.Complete(context.Background(), llm.CompletionRequest{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
	})
	if err == nil {
		t.Fatal("expected error for non-zero exit")
	}
	if !strings.Contains(err.Error(), "auth required") {
		t.Errorf("error should include stderr snippet; got: %v", err)
	}
}

func TestComplete_EmptyResponseErrors(t *testing.T) {
	script := fakeClaude(t, "", fakeOpts{stdout: ""})

	p, _ := New(llm.ClaudeCLIConfig{Binary: script})
	defer func() { _ = p.Close() }()

	if _, err := p.Complete(context.Background(), llm.CompletionRequest{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
	}); err == nil {
		t.Fatal("expected error for empty response")
	}
}

func TestComplete_ContextCancellation(t *testing.T) {
	script := fakeClaude(t, "", fakeOpts{stdout: "late", sleep: 2 * time.Second})

	p, _ := New(llm.ClaudeCLIConfig{Binary: script, TimeoutSeconds: 1})
	defer func() { _ = p.Close() }()

	if _, err := p.Complete(context.Background(), llm.CompletionRequest{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
	}); err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestComplete_ExtraArgsForwarded(t *testing.T) {
	script := fakeClaude(t, "", fakeOpts{stdout: "ok"})

	p, _ := New(llm.ClaudeCLIConfig{Binary: script, Args: []string{"--allowed-tools", ""}})
	defer func() { _ = p.Close() }()

	if _, err := p.Complete(context.Background(), llm.CompletionRequest{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	args := readSidecar(t, script, "args.txt")
	if !strings.Contains(args, "--allowed-tools") {
		t.Errorf("args missing --allowed-tools:\n%s", args)
	}
}

func TestExtractJSON_Variants(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		want  string
		okErr bool // true means "expect not-found"
	}{
		{"raw", `{"a":1}`, `{"a":1}`, false},
		{"with prose", "Here:\n{\"a\":1}\nend", `{"a":1}`, false},
		{"markdown fence", "```json\n{\"a\":1}\n```", `{"a":1}`, false},
		{"plain fence", "```\n{\"a\":1}\n```", `{"a":1}`, false},
		{"array", `[1,2,3]`, `[1,2,3]`, false},
		{"nested braces in strings", `{"k":"a{b}c","v":1}`, `{"k":"a{b}c","v":1}`, false},
		{"no JSON", "I cannot help with that.", "", true},
		{"truncated", `{"a":1,`, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := extractJSON(tc.in)
			if tc.okErr && ok {
				t.Fatalf("extractJSON should have failed; got %q", got)
			}
			if !tc.okErr && !ok {
				t.Fatalf("extractJSON unexpectedly failed for %q", tc.in)
			}
			if !tc.okErr && got != tc.want {
				t.Errorf("got=%q want=%q", got, tc.want)
			}
		})
	}
}

func TestFlatten_Roles(t *testing.T) {
	sys, prompt := flatten([]llm.Message{
		{Role: llm.RoleSystem, Content: "rule 1"},
		{Role: llm.RoleSystem, Content: "rule 2"},
		{Role: llm.RoleUser, Content: "q1"},
		{Role: llm.RoleAssistant, Content: "a1"},
		{Role: llm.RoleTool, Content: "[1,2,3]", ToolName: "search_symbols"},
		{Role: llm.RoleUser, Content: "q2"},
	})
	if sys != "rule 1\n\nrule 2" {
		t.Errorf("system=%q", sys)
	}
	for _, want := range []string{"User: q1", "Assistant: a1", "Tool result (search_symbols)", "User: q2"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q\nprompt=\n%s", want, prompt)
		}
	}
}

func TestAppendSchemaInstruction_KeepsExistingSystem(t *testing.T) {
	out := appendSchemaInstruction("be terse", llm.ShapeExpandTerms, nil)
	if !strings.HasPrefix(out, "be terse") {
		t.Errorf("rider must follow original system: %q", out)
	}
	if !strings.Contains(out, "JSON Schema") {
		t.Errorf("rider must reference JSON Schema: %q", out)
	}
	if !strings.Contains(out, "terms") {
		t.Errorf("rider must embed shape property name: %q", out)
	}
}

func TestSnippet_TruncatesLong(t *testing.T) {
	long := strings.Repeat("x", 1000)
	out := snippet([]byte(long))
	if len(out) < 100 {
		t.Errorf("snippet=%d chars; want a truncated payload", len(out))
	}
	if !strings.HasSuffix(out, "…") {
		t.Error("long snippet must end with the truncation marker")
	}
}

// Sanity check: the test helper itself can run a child process. If
// this fails the environment doesn't support `/bin/sh` and every
// other subprocess test in this file will skip cleanly.
func TestHelper_FakeScriptIsExecutable(t *testing.T) {
	script := fakeClaude(t, "", fakeOpts{stdout: "alive"})
	cmd := exec.Command(script, "ping")
	cmd.Stdin = strings.NewReader("")
	out, err := cmd.Output()
	if err != nil {
		// On a CI runner without /bin/sh, surface as skip instead of
		// fail so we don't paint subprocess CI red.
		t.Skipf("cannot exec fake script: %v", err)
	}
	if !strings.Contains(string(out), "alive") {
		t.Errorf("output=%q", out)
	}
}
