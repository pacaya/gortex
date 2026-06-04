package indexer

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

func TestExternalCallSynthesisEnabled_EnvOverride(t *testing.T) {
	g := graph.New()
	idx := newTestIndexer(g)
	bptr := func(b bool) *bool { return &b }

	// Default-on: key unset (nil) resolves to enabled.
	require.True(t, idx.externalCallSynthesisEnabled())

	// Explicit opt-out via config.
	idx.config.SynthesizeExternalCalls = bptr(false)
	require.False(t, idx.externalCallSynthesisEnabled())

	// Explicit opt-in via config.
	idx.config.SynthesizeExternalCalls = bptr(true)
	require.True(t, idx.externalCallSynthesisEnabled())

	t.Setenv("GORTEX_SYNTH_EXTERNAL_CALLS", "0")
	require.False(t, idx.externalCallSynthesisEnabled()) // env overrides config-on

	t.Setenv("GORTEX_SYNTH_EXTERNAL_CALLS", "1")
	idx.config.SynthesizeExternalCalls = bptr(false)
	require.True(t, idx.externalCallSynthesisEnabled()) // env overrides config-off
}
