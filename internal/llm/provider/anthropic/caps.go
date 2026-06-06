package anthropic

import "strings"

// caps.go is the per-model capability matrix for the optional knobs
// (reasoning effort here; extended thinking lives alongside it). Both
// are gated by model so an opt-in knob is never sent to a model that
// would reject it.

// effortFamilyPrefixes are the model families that accept an
// output_config.effort of low / medium / high.
var effortFamilyPrefixes = []string{
	"claude-opus-4-5",
	"claude-opus-4-6",
	"claude-opus-4-7",
	"claude-opus-4-8",
	"claude-sonnet-4-6",
	"claude-mythos-preview",
}

// maxEffortPrefixes additionally accept effort "max".
var maxEffortPrefixes = []string{
	"claude-opus-4-6",
	"claude-opus-4-7",
	"claude-opus-4-8",
	"claude-sonnet-4-6",
	"claude-mythos-preview",
}

// xhighEffortPrefixes additionally accept effort "xhigh".
var xhighEffortPrefixes = []string{
	"claude-opus-4-7",
	"claude-opus-4-8",
}

// supportsEffortLevel reports whether model accepts the given effort
// level. Unknown models and unknown levels return false, so effort is
// only ever sent when the pairing is known-good.
func supportsEffortLevel(model, level string) bool {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "low", "medium", "high":
		return matchesFamily(model, effortFamilyPrefixes)
	case "max":
		return matchesFamily(model, maxEffortPrefixes)
	case "xhigh":
		return matchesFamily(model, xhighEffortPrefixes)
	default:
		return false
	}
}

// adaptiveThinkingPrefixes are the model families that support adaptive
// extended thinking (thinking.type = "adaptive"). Older families only
// support the manual budget form.
var adaptiveThinkingPrefixes = []string{
	"claude-opus-4-6",
	"claude-opus-4-7",
	"claude-opus-4-8",
	"claude-sonnet-4-6",
	"claude-mythos-preview",
}

// supportsAdaptiveThinking reports whether model accepts adaptive
// thinking. Unknown models fall back to the manual budget form.
func supportsAdaptiveThinking(model string) bool {
	return matchesFamily(model, adaptiveThinkingPrefixes)
}

// matchesFamily reports whether model starts with one of the given
// family prefixes (case-insensitive).
func matchesFamily(model string, prefixes []string) bool {
	m := strings.ToLower(strings.TrimSpace(model))
	for _, p := range prefixes {
		if strings.HasPrefix(m, p) {
			return true
		}
	}
	return false
}
