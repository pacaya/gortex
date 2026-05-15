// Package claudecli is the Claude Code CLI llm.Provider.
//
// It is pure Go — available in every build, no `-tags llama` needed.
// Inference is delegated to the user's locally installed `claude`
// binary, which reuses the user's Claude Code subscription instead of
// requiring an Anthropic API key. Each Complete call spawns one
// `claude -p` subprocess: the conversation is flattened to text, the
// system prompt is forwarded via --append-system-prompt, and the
// prompt text is fed on stdin so very large contexts don't trip
// ARG_MAX.
//
// Structured output (the expand / rerank / verify shapes and the
// agent tool-call shape) is obtained by appending a JSON-Schema
// instruction to the system prompt and parsing the first valid JSON
// object out of the response — the CLI has no native structured-
// output mechanism. The agent tool-loop itself uses the *emulated*
// protocol: tool calls and results travel as plain text turns, so a
// single llm.Message shape works across all five providers.
package claudecli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/zzet/gortex/internal/llm"
)

// defaultTimeout caps one Complete call when the user hasn't set
// claudecli.timeout_seconds in config. Claude Code CLI startup plus
// one model round-trip is comfortably under 120s for the small
// prompts the assist/agent loop emits.
const defaultTimeout = 120 * time.Second

// Provider implements llm.Provider against the `claude` CLI.
type Provider struct {
	binary  string
	model   string
	extra   []string
	timeout time.Duration
}

var _ llm.Provider = (*Provider)(nil)

// New constructs the Claude CLI provider. It verifies the binary is
// reachable on $PATH (or as an absolute path) so misconfiguration
// surfaces at startup, not on the first Complete call.
func New(cfg llm.ClaudeCLIConfig) (llm.Provider, error) {
	bin := strings.TrimSpace(cfg.Binary)
	if bin == "" {
		bin = "claude"
	}
	resolved, err := exec.LookPath(bin)
	if err != nil {
		return nil, fmt.Errorf("claudecli: binary %q not found on PATH: %w", bin, err)
	}
	timeout := defaultTimeout
	if cfg.TimeoutSeconds > 0 {
		timeout = time.Duration(cfg.TimeoutSeconds) * time.Second
	}
	return &Provider{
		binary:  resolved,
		model:   strings.TrimSpace(cfg.Model),
		extra:   append([]string(nil), cfg.Args...),
		timeout: timeout,
	}, nil
}

// Name implements llm.Provider.
func (p *Provider) Name() string { return "claudecli" }

// Close is a no-op — every Complete spawns a fresh subprocess; there
// is no long-lived connection or model handle to release.
func (p *Provider) Close() error { return nil }

// Complete implements llm.Provider. It runs one `claude -p`
// subprocess: the system messages are joined and forwarded via
// --append-system-prompt, every other message is flattened into a
// chat-style prompt that is piped on stdin, and stdout is captured
// as the model's text. For structured shapes the schema is injected
// into the system prompt and the first balanced JSON object is
// extracted from the response.
func (p *Provider) Complete(ctx context.Context, req llm.CompletionRequest) (llm.CompletionResponse, error) {
	system, prompt := flatten(req.Messages)
	structured := req.Shape != llm.ShapeFreeform
	if structured {
		system = appendSchemaInstruction(system, req.Shape, req.Tools)
	}

	args := []string{"--print", "--output-format", "text"}
	if p.model != "" {
		args = append(args, "--model", p.model)
	}
	if req.MaxTokens > 0 {
		// max-turns caps the agent loop inside Claude Code, not the
		// per-response token budget — but pinning it to 1 keeps the
		// CLI single-shot, which is what every llm.Provider caller
		// already assumes. The token cap itself is best-effort: the
		// CLI exposes no equivalent flag, so we lean on the model's
		// own behaviour given a short system prompt.
		args = append(args, "--max-turns", "1")
	} else {
		args = append(args, "--max-turns", "1")
	}
	if system != "" {
		args = append(args, "--append-system-prompt", system)
	}
	args = append(args, p.extra...)

	runCtx := ctx
	if p.timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, p.timeout)
		defer cancel()
	}

	cmd := exec.CommandContext(runCtx, p.binary, args...)
	cmd.Stdin = strings.NewReader(prompt)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		// Distinguish a context-timeout from an exec failure so the
		// agent loop can log something meaningful.
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			return llm.CompletionResponse{}, fmt.Errorf("claudecli: timed out after %s: %s", p.timeout, snippet(stderr.Bytes()))
		}
		if msg := snippet(stderr.Bytes()); msg != "" {
			return llm.CompletionResponse{}, fmt.Errorf("claudecli: %w: %s", err, msg)
		}
		return llm.CompletionResponse{}, fmt.Errorf("claudecli: %w", err)
	}

	text := strings.TrimSpace(stdout.String())
	if text == "" {
		return llm.CompletionResponse{}, errors.New("claudecli: empty response from CLI")
	}
	if structured {
		extracted, ok := extractJSON(text)
		if !ok {
			return llm.CompletionResponse{}, fmt.Errorf("claudecli: response carried no JSON: %s", snippet([]byte(text)))
		}
		text = extracted
	}
	return llm.CompletionResponse{Text: text}, nil
}

// flatten splits the conversation into a system block (every
// RoleSystem message joined with a blank line) and a chat-style
// prompt (every other message rendered as "User:" / "Assistant:" /
// "Tool result:" turns). The CLI takes the system part via
// --append-system-prompt and reads the prompt part from stdin. Using
// stdin avoids the ARG_MAX ceiling on long contexts.
func flatten(in []llm.Message) (system, prompt string) {
	var sys []string
	var b strings.Builder
	turns := 0
	for _, m := range in {
		switch m.Role {
		case llm.RoleSystem:
			if s := strings.TrimSpace(m.Content); s != "" {
				sys = append(sys, s)
			}
		case llm.RoleAssistant:
			if turns > 0 {
				b.WriteString("\n\n")
			}
			b.WriteString("Assistant: ")
			b.WriteString(m.Content)
			turns++
		case llm.RoleTool:
			if turns > 0 {
				b.WriteString("\n\n")
			}
			b.WriteString(renderToolResult(m))
			turns++
		default:
			if turns > 0 {
				b.WriteString("\n\n")
			}
			b.WriteString("User: ")
			b.WriteString(m.Content)
			turns++
		}
	}
	return strings.Join(sys, "\n\n"), b.String()
}

func renderToolResult(m llm.Message) string {
	if m.ToolName != "" {
		return "Tool result (" + m.ToolName + "):\n" + m.Content
	}
	return "Tool result:\n" + m.Content
}

// appendSchemaInstruction tacks a "respond with this JSON shape"
// rider onto the system prompt. The CLI has no native structured-
// output flag, so this — plus the JSON extractor on the response —
// is how the four list shapes get enforced.
func appendSchemaInstruction(system string, shape llm.JSONShape, tools []llm.ToolSpec) string {
	schema := llm.JSONSchemaFor(shape, tools)
	if schema == nil {
		return system
	}
	raw, err := json.Marshal(schema)
	if err != nil {
		// llm.JSONSchemaFor returns hand-built maps that always
		// marshal — if Marshal does fail, falling through with the
		// unmodified system prompt is safer than crashing the call.
		return system
	}
	rider := "Respond with a single JSON object that conforms exactly to this JSON Schema:\n" +
		string(raw) +
		"\nOutput ONLY the JSON object — no prose, no commentary, no markdown fences."
	if system == "" {
		return rider
	}
	return system + "\n\n" + rider
}

// extractJSON pulls the first balanced JSON object or array out of
// text. The CLI sometimes wraps responses in markdown fences or
// surrounds them with prose ("Sure, here you go:\n{...}"); this
// helper finds and verifies the JSON payload so the assist passes
// don't choke on chatty completions.
func extractJSON(text string) (string, bool) {
	if c, ok := tryUnmarshal(text); ok {
		return c, true
	}
	if stripped, changed := stripFences(text); changed {
		if c, ok := tryUnmarshal(stripped); ok {
			return c, true
		}
		text = stripped
	}
	// Scan for the first balanced JSON object/array.
	for i := 0; i < len(text); i++ {
		c := text[i]
		if c != '{' && c != '[' {
			continue
		}
		end, ok := balancedEnd(text, i)
		if !ok {
			continue
		}
		candidate := text[i : end+1]
		if c, ok := tryUnmarshal(candidate); ok {
			return c, true
		}
	}
	return "", false
}

// stripFences removes a single ``` or ```json wrapper.
func stripFences(text string) (string, bool) {
	t := strings.TrimSpace(text)
	if !strings.HasPrefix(t, "```") {
		return text, false
	}
	// Drop the opening fence line.
	nl := strings.IndexByte(t, '\n')
	if nl < 0 {
		return text, false
	}
	body := t[nl+1:]
	if i := strings.LastIndex(body, "```"); i >= 0 {
		body = body[:i]
	}
	return strings.TrimSpace(body), true
}

// tryUnmarshal returns the trimmed candidate if it parses as JSON.
func tryUnmarshal(s string) (string, bool) {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return "", false
	}
	var v any
	if err := json.Unmarshal([]byte(trimmed), &v); err != nil {
		return "", false
	}
	return trimmed, true
}

// balancedEnd returns the index of the closing brace/bracket that
// balances the opener at start. Tracks string literals so quoted
// braces don't throw the depth count off.
func balancedEnd(text string, start int) (int, bool) {
	open := text[start]
	var close byte
	switch open {
	case '{':
		close = '}'
	case '[':
		close = ']'
	default:
		return 0, false
	}
	depth := 0
	inString := false
	escape := false
	for i := start; i < len(text); i++ {
		c := text[i]
		if inString {
			if escape {
				escape = false
				continue
			}
			switch c {
			case '\\':
				escape = true
			case '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case open:
			depth++
		case close:
			depth--
			if depth == 0 {
				return i, true
			}
		}
	}
	return 0, false
}

// snippet truncates a stderr blob for inclusion in an error message.
// Operates on runes so multi-byte characters at the cut point stay
// intact.
func snippet(b []byte) string {
	const max = 300
	s := strings.TrimSpace(string(b))
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	// Walk runes, count to max.
	count := 0
	for i := range s {
		count++
		if count > max {
			return s[:i] + "…"
		}
	}
	return s
}
