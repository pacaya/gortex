package svc

import (
	"context"
	"testing"

	"github.com/zzet/gortex/internal/llm"
)

// fakeAgentProvider is a minimal llm.Provider that always answers a
// tool-call request by calling final_answer — enough to drive one
// RunAgent turn to completion without a real model.
type fakeAgentProvider struct{ calls int }

func (p *fakeAgentProvider) Name() string { return "fake" }
func (p *fakeAgentProvider) Close() error { return nil }
func (p *fakeAgentProvider) Complete(_ context.Context, _ llm.CompletionRequest) (llm.CompletionResponse, error) {
	p.calls++
	return llm.CompletionResponse{Text: `{"tool":"final_answer","args":{"text":"done"}}`}, nil
}

func TestProviderForModel_BaseAndRouted(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	cfg := llm.Config{Provider: "anthropic", Anthropic: llm.AnthropicConfig{RemoteConfig: llm.RemoteConfig{Model: "claude-sonnet-4-6"}}}.ApplyDefaults()
	s := NewService(cfg, llm.MockBackend{})
	if !s.Enabled() {
		t.Fatal("service should be enabled with an API key set")
	}
	defer func() { _ = s.Close() }()

	// Empty model id, or the active model, returns the base provider.
	if p, err := s.providerForModel(""); err != nil || p != s.provider {
		t.Errorf("providerForModel(\"\") = (%v, %v), want the base provider", p, err)
	}
	if p, err := s.providerForModel("claude-sonnet-4-6"); err != nil || p != s.provider {
		t.Errorf("providerForModel(active model) must return the base provider")
	}

	// A different model id constructs and caches a distinct provider.
	routed, err := s.providerForModel("claude-haiku-4-5")
	if err != nil {
		t.Fatalf("providerForModel(routed): %v", err)
	}
	if routed == s.provider {
		t.Error("a routed model must yield a distinct provider, not the base")
	}
	again, err := s.providerForModel("claude-haiku-4-5")
	if err != nil || again != routed {
		t.Errorf("providerForModel must cache: got %v (err %v), want the first routed provider", again, err)
	}
}

func TestProviderForModel_ClosedByClose(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	cfg := llm.Config{Provider: "anthropic", Anthropic: llm.AnthropicConfig{RemoteConfig: llm.RemoteConfig{Model: "claude-sonnet-4-6"}}}.ApplyDefaults()
	s := NewService(cfg, llm.MockBackend{})
	if _, err := s.providerForModel("claude-haiku-4-5"); err != nil {
		t.Fatalf("providerForModel: %v", err)
	}
	if len(s.routedProviders) != 1 {
		t.Fatalf("routedProviders has %d entries, want 1", len(s.routedProviders))
	}
	if err := s.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if len(s.routedProviders) != 0 {
		t.Errorf("Close must clear routedProviders, %d left", len(s.routedProviders))
	}
}

func TestRunAgent_RoutingClassifiesComplexity(t *testing.T) {
	fake := &fakeAgentProvider{}
	s := newFakeService(fake)
	s.cfg.Routing.Enabled = true // tier models empty → both route to the base provider

	complex, err := s.RunAgent(context.Background(), llm.RunAgentOptions{
		Question: "trace the request across systems",
	})
	if err != nil {
		t.Fatalf("RunAgent: %v", err)
	}
	if complex.Complexity != "complex" {
		t.Errorf("Complexity = %q, want complex (\"trace ... across\")", complex.Complexity)
	}

	simple, err := s.RunAgent(context.Background(), llm.RunAgentOptions{
		Question: "who calls NewServer",
		Scope:    llm.Scope{Repo: "gortex"},
	})
	if err != nil {
		t.Fatalf("RunAgent: %v", err)
	}
	if simple.Complexity != "simple" {
		t.Errorf("Complexity = %q, want simple", simple.Complexity)
	}
}

func TestRunAgent_NoRoutingWhenDisabled(t *testing.T) {
	fake := &fakeAgentProvider{}
	s := newFakeService(fake)
	// Routing left disabled.

	ans, err := s.RunAgent(context.Background(), llm.RunAgentOptions{
		Question: "trace the request across every system everywhere",
	})
	if err != nil {
		t.Fatalf("RunAgent: %v", err)
	}
	if ans.Complexity != "" {
		t.Errorf("Complexity = %q, want empty when routing is disabled", ans.Complexity)
	}
}
