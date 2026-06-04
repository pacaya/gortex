package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestExternalCallSynthesis_AbsentIsDefaultOn asserts that a config with
// no `synthesize_external_calls` key leaves the pointer nil — which the
// tri-state resolver reads as "external-call qualification ON" — so every
// external call gets a stable cross-repo identity node by default
// (affordable because synthesis is incremental on the hot path).
func TestExternalCallSynthesis_AbsentIsDefaultOn(t *testing.T) {
	cfg, err := Load(writeConfig(t, "index:\n  workers: 2\n"))
	require.NoError(t, err)

	assert.Nil(t, cfg.Index.SynthesizeExternalCalls,
		"an absent key must leave the pointer nil (not false) so default-on applies")
	assert.True(t, cfg.Index.ExternalCallSynthesisEnabledOrDefault(),
		"a nil flag must resolve to ON")
}

// TestExternalCallSynthesis_ExplicitlyDisabled asserts an explicit
// opt-out is distinguishable from "absent" and turns synthesis off.
func TestExternalCallSynthesis_ExplicitlyDisabled(t *testing.T) {
	cfg, err := Load(writeConfig(t, "index:\n  synthesize_external_calls: false\n"))
	require.NoError(t, err)

	require.NotNil(t, cfg.Index.SynthesizeExternalCalls, "an explicit value must round-trip as a non-nil pointer")
	assert.False(t, *cfg.Index.SynthesizeExternalCalls)
	assert.False(t, cfg.Index.ExternalCallSynthesisEnabledOrDefault())
}

// TestExternalCallSynthesis_ExplicitlyEnabled asserts `true` round-trips.
func TestExternalCallSynthesis_ExplicitlyEnabled(t *testing.T) {
	cfg, err := Load(writeConfig(t, "index:\n  synthesize_external_calls: true\n"))
	require.NoError(t, err)

	require.NotNil(t, cfg.Index.SynthesizeExternalCalls)
	assert.True(t, *cfg.Index.SynthesizeExternalCalls)
	assert.True(t, cfg.Index.ExternalCallSynthesisEnabledOrDefault())
}
