package anthropic

import (
	"strings"

	"github.com/zzet/gortex/internal/llm"
)

// Option configures optional Anthropic behaviours on a Provider at
// construction. Provider.New takes a variadic list of these, so the
// base signature (a RemoteConfig) stays unchanged.
type Option func(*Provider)

// WithPromptCaching enables Anthropic prompt caching with the given TTL
// ("5m" or "1h"; "5m" is assumed when empty). When enabled, the system
// prompt and the structured-output tool are marked as cache
// breakpoints. A false `enabled` is a no-op.
func WithPromptCaching(enabled bool, ttl string) Option {
	return func(p *Provider) {
		if !enabled {
			return
		}
		p.caching = true
		p.cacheTTL = strings.TrimSpace(ttl)
	}
}

// WithThinking configures extended thinking. mode is off / auto / manual
// / adaptive; budget is the manual-mode token budget; display is the
// adaptive-mode visibility ("summarized" / "omitted"). An empty or "off"
// mode is a no-op.
func WithThinking(mode string, budget int, display string) Option {
	return func(p *Provider) {
		p.thinkingMode = strings.ToLower(strings.TrimSpace(mode))
		p.thinkingBudget = budget
		p.thinkingDisplay = strings.TrimSpace(display)
	}
}

// defaultThinkingBudget is the manual-mode budget used when thinking is
// enabled without an explicit one. The Anthropic minimum is 1024.
const defaultThinkingBudget = 4096

// cacheControl builds the ephemeral cache_control marker honouring the
// configured TTL.
func (p *Provider) cacheControl() map[string]any {
	cc := map[string]any{"type": "ephemeral"}
	if p.cacheTTL != "" {
		cc["ttl"] = p.cacheTTL
	}
	return cc
}

// thinkingConfig returns the `thinking` request object and the minimum
// max_tokens the request must carry for it, or (nil, 0) when thinking
// is off or not applicable.
//
// Extended thinking is incompatible with a forced tool_choice, so it is
// only ever applied to freeform requests — the structured-output passes
// (which pin tool_choice to the respond tool) run without it. The
// resolved mode is gated by model capability: "auto" picks adaptive on a
// capable model, else manual; an explicit "adaptive" on an incapable
// model degrades to manual rather than erroring.
func (p *Provider) thinkingConfig(shape llm.JSONShape) (map[string]any, int) {
	if shape != llm.ShapeFreeform {
		return nil, 0
	}
	mode := p.thinkingMode
	switch mode {
	case "", "off":
		return nil, 0
	case "auto":
		if supportsAdaptiveThinking(p.model) {
			mode = "adaptive"
		} else {
			mode = "manual"
		}
	}

	if mode == "adaptive" && supportsAdaptiveThinking(p.model) {
		cfg := map[string]any{"type": "adaptive"}
		if p.thinkingDisplay != "" {
			cfg["display"] = p.thinkingDisplay
		}
		// Adaptive thinking still needs headroom in max_tokens.
		return cfg, 4096
	}

	// manual (also the fallback for adaptive on an incapable model)
	budget := p.thinkingBudget
	if budget <= 0 {
		budget = defaultThinkingBudget
	}
	if budget < 1024 {
		budget = 1024
	}
	// max_tokens must exceed the thinking budget.
	return map[string]any{"type": "enabled", "budget_tokens": budget}, budget + 1024
}
