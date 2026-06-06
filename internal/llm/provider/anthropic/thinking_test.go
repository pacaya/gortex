package anthropic

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zzet/gortex/internal/llm"
)

// captureWithOpts runs one Complete (with the given model, request, and
// options) against a mock that accepts both freeform and structured
// shapes, and returns the decoded request body.
func captureWithOpts(t *testing.T, model string, req llm.CompletionRequest, opts ...Option) map[string]any {
	t.Helper()
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		// A reply that satisfies both freeform (text block) and
		// structured (respond tool_use) extraction.
		_, _ = io.WriteString(w, `{"content":[{"type":"text","text":"ok"},{"type":"tool_use","name":"respond","input":{"terms":["x"]}}]}`)
	}))
	defer srv.Close()

	t.Setenv("ANTHROPIC_API_KEY", "k")
	p, err := New(llm.RemoteConfig{Model: model, BaseURL: srv.URL}, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Close() }()
	if _, err := p.Complete(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	return got
}

func freeform(content string) llm.CompletionRequest {
	return llm.CompletionRequest{Messages: []llm.Message{
		{Role: llm.RoleSystem, Content: "you are terse"},
		{Role: llm.RoleUser, Content: content},
	}}
}

func structured() llm.CompletionRequest {
	return llm.CompletionRequest{
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: "you are terse"},
			{Role: llm.RoleUser, Content: "auth"},
		},
		Shape: llm.ShapeExpandTerms,
	}
}

func TestCaching_MarksSystemAndToolWhenEnabled(t *testing.T) {
	got := captureWithOpts(t, "claude-sonnet-4-6", structured(), WithPromptCaching(true, "1h"))

	// system must be an array of blocks, the last carrying cache_control.
	sysBlocks, ok := got["system"].([]any)
	if !ok || len(sysBlocks) == 0 {
		t.Fatalf("system should be a content-block array when caching, got %T %v", got["system"], got["system"])
	}
	block := sysBlocks[0].(map[string]any)
	cc, ok := block["cache_control"].(map[string]any)
	if !ok {
		t.Fatalf("system block missing cache_control: %v", block)
	}
	if cc["type"] != "ephemeral" || cc["ttl"] != "1h" {
		t.Errorf("cache_control=%v want ephemeral/1h", cc)
	}

	// The forced tool must also carry cache_control.
	tools := got["tools"].([]any)
	tool := tools[0].(map[string]any)
	if _, ok := tool["cache_control"].(map[string]any); !ok {
		t.Errorf("tool should carry cache_control when caching, got %v", tool)
	}
}

func TestCaching_OffByDefault(t *testing.T) {
	got := captureWithOpts(t, "claude-sonnet-4-6", structured())
	if _, isArray := got["system"].([]any); isArray {
		t.Error("system must be a plain string when caching is off")
	}
	if _, isStr := got["system"].(string); !isStr {
		t.Errorf("system=%T want string", got["system"])
	}
	tools := got["tools"].([]any)
	tool := tools[0].(map[string]any)
	if _, ok := tool["cache_control"]; ok {
		t.Error("tool must not carry cache_control when caching is off")
	}
}

func TestThinking_OffByDefault(t *testing.T) {
	got := captureWithOpts(t, "claude-opus-4-8", freeform("hi"))
	if _, ok := got["thinking"]; ok {
		t.Error("thinking must be off by default")
	}
}

func TestThinking_ManualFreeformBumpsMaxTokens(t *testing.T) {
	got := captureWithOpts(t, "claude-opus-4-8", freeform("hi"), WithThinking("manual", 8000, ""))
	think, ok := got["thinking"].(map[string]any)
	if !ok {
		t.Fatalf("expected a thinking object, got %v", got["thinking"])
	}
	if think["type"] != "enabled" || think["budget_tokens"] != float64(8000) {
		t.Errorf("thinking=%v want enabled/8000", think)
	}
	if got["max_tokens"].(float64) < 8000 {
		t.Errorf("max_tokens=%v must exceed the thinking budget", got["max_tokens"])
	}
}

func TestThinking_AdaptiveOnCapableModel(t *testing.T) {
	got := captureWithOpts(t, "claude-opus-4-8", freeform("hi"), WithThinking("adaptive", 0, "summarized"))
	think := got["thinking"].(map[string]any)
	if think["type"] != "adaptive" || think["display"] != "summarized" {
		t.Errorf("thinking=%v want adaptive/summarized", think)
	}
}

func TestThinking_AutoResolves(t *testing.T) {
	// Capable model -> adaptive.
	got := captureWithOpts(t, "claude-sonnet-4-6", freeform("hi"), WithThinking("auto", 0, ""))
	if got["thinking"].(map[string]any)["type"] != "adaptive" {
		t.Errorf("auto on a capable model should pick adaptive, got %v", got["thinking"])
	}
	// Older model -> manual.
	got = captureWithOpts(t, "claude-3-5-haiku-latest", freeform("hi"), WithThinking("auto", 0, ""))
	if got["thinking"].(map[string]any)["type"] != "enabled" {
		t.Errorf("auto on an incapable model should fall back to manual, got %v", got["thinking"])
	}
}

func TestThinking_AdaptiveOnIncapableFallsBackToManual(t *testing.T) {
	got := captureWithOpts(t, "claude-3-opus-20240229", freeform("hi"), WithThinking("adaptive", 0, ""))
	think := got["thinking"].(map[string]any)
	if think["type"] != "enabled" {
		t.Errorf("explicit adaptive on an incapable model should degrade to manual, got %v", think)
	}
}

func TestThinking_SkippedForStructuredRequests(t *testing.T) {
	got := captureWithOpts(t, "claude-opus-4-8", structured(), WithThinking("manual", 8000, ""))
	if _, ok := got["thinking"]; ok {
		t.Error("thinking is incompatible with the forced-tool structured path and must be suppressed")
	}
}
