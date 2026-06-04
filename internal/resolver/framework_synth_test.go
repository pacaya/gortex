package resolver

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

func TestStampSynthesized(t *testing.T) {
	e := &graph.Edge{From: "a", To: "b", Kind: graph.EdgeCalls}
	StampSynthesized(e, SynthEventChannel)
	assert.Equal(t, SynthEventChannel, e.Meta[MetaSynthesizedBy])
	assert.Equal(t, ProvenanceHeuristic, e.Meta[MetaProvenance])

	// Does not clobber an explicit provenance already set.
	e2 := &graph.Edge{From: "a", To: "b", Kind: graph.EdgeCalls, Meta: map[string]any{MetaProvenance: "verified"}}
	StampSynthesized(e2, SynthGRPCStub)
	assert.Equal(t, "verified", e2.Meta[MetaProvenance])
	assert.Equal(t, SynthGRPCStub, e2.Meta[MetaSynthesizedBy])

	UnstampSynthesized(e)
	_, hasBy := e.Meta[MetaSynthesizedBy]
	_, hasProv := e.Meta[MetaProvenance]
	assert.False(t, hasBy)
	assert.False(t, hasProv)

	// nil-safe.
	StampSynthesized(nil, SynthGRPCStub)
	UnstampSynthesized(nil)
}

func TestRunFrameworkSynthesizers_Report(t *testing.T) {
	b := newEventChannelTestGraph()
	b.emit("p.go::p", "eventemitter", "e", "p.go", 1)
	b.listen("c.go::c", "eventemitter", "e", "c.go", 1)

	rep := RunFrameworkSynthesizers(b.g)
	assert.Equal(t, 1, rep.Total, "the one event-channel pair is the only synthesized edge")

	byName := map[string]int{}
	for _, p := range rep.Per {
		byName[p.Name] = p.Edges
	}
	// Every registered synthesizer reports a row, even at zero.
	require.Contains(t, byName, SynthGRPCStub)
	require.Contains(t, byName, SynthTemporalStub)
	require.Contains(t, byName, SynthEventChannel)
	assert.Equal(t, 0, byName[SynthGRPCStub])
	assert.Equal(t, 0, byName[SynthTemporalStub])
	assert.Equal(t, 1, byName[SynthEventChannel])
}

func TestRunFrameworkSynthesizers_NilGraph(t *testing.T) {
	rep := RunFrameworkSynthesizers(nil)
	assert.Equal(t, 0, rep.Total)
	assert.Nil(t, rep.Per)
}
