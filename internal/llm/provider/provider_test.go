package provider

import (
	"os"
	"testing"

	"github.com/zzet/gortex/internal/llm"
)

func TestNew_UnknownProvider(t *testing.T) {
	if _, err := New(llm.Config{Provider: "bogus"}.ApplyDefaults()); err == nil {
		t.Fatal("expected error for an unknown provider")
	}
}

func TestNew_AnthropicMissingKey(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	if _, err := New(llm.Config{Provider: "anthropic"}.ApplyDefaults()); err == nil {
		t.Fatal("expected error when ANTHROPIC_API_KEY is unset")
	}
}

func TestNew_AnthropicOK(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	p, err := New(llm.Config{Provider: "anthropic"}.ApplyDefaults())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer func() { _ = p.Close() }()
	if p.Name() != "anthropic" {
		t.Errorf("Name()=%q want anthropic", p.Name())
	}
}

func TestNew_OpenAIOK(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "test-key")
	p, err := New(llm.Config{Provider: "openai"}.ApplyDefaults())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer func() { _ = p.Close() }()
	if p.Name() != "openai" {
		t.Errorf("Name()=%q want openai", p.Name())
	}
}

func TestNew_OllamaMissingModel(t *testing.T) {
	if _, err := New(llm.Config{Provider: "ollama"}.ApplyDefaults()); err == nil {
		t.Fatal("expected error when llm.ollama.model is unset")
	}
}

func TestNew_OllamaOK(t *testing.T) {
	cfg := llm.Config{Provider: "ollama", Ollama: llm.OllamaConfig{Model: "qwen2.5-coder:7b"}}.ApplyDefaults()
	p, err := New(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer func() { _ = p.Close() }()
	if p.Name() != "ollama" {
		t.Errorf("Name()=%q want ollama", p.Name())
	}
}

func TestNew_ClaudeCLIMissingBinary(t *testing.T) {
	cfg := llm.Config{Provider: "claudecli", ClaudeCLI: llm.ClaudeCLIConfig{Binary: "definitely-not-on-path-claude-xyz"}}.ApplyDefaults()
	if _, err := New(cfg); err == nil {
		t.Fatal("expected error when claudecli binary is not on PATH")
	}
}

func TestNew_ClaudeCLIOK(t *testing.T) {
	// Use a real binary that exists on every Unix to satisfy the
	// PATH lookup — the factory only verifies presence, it doesn't
	// invoke the binary.
	bin := "/bin/echo"
	if _, err := os.Stat(bin); err != nil {
		t.Skip("/bin/echo not available — skipping claudecli factory test")
	}
	cfg := llm.Config{Provider: "claudecli", ClaudeCLI: llm.ClaudeCLIConfig{Binary: bin}}.ApplyDefaults()
	p, err := New(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer func() { _ = p.Close() }()
	if p.Name() != "claudecli" {
		t.Errorf("Name()=%q want claudecli", p.Name())
	}
}
