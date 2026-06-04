package resolver

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// outCallTarget returns the To of the single call/reference out-edge of
// nodeID (the tests below give each caller exactly one).
func outCallTarget(g graph.Store, nodeID string) string {
	for _, e := range g.GetOutEdges(nodeID) {
		if e.Kind == graph.EdgeCalls || e.Kind == graph.EdgeReferences {
			return e.To
		}
	}
	return ""
}

func TestSynthesizeExternalCallsForFiles(t *testing.T) {
	g := graph.New()
	// app.go calls an external dependency.
	g.AddNode(&graph.Node{ID: "app.go::run", Kind: graph.KindFunction, Name: "run", FilePath: "app.go", Language: "go"})
	g.AddEdge(&graph.Edge{From: "app.go::run", To: "dep::github.com/stripe/stripe-go::Charge", Kind: graph.EdgeCalls, FilePath: "app.go", Line: 10})
	// other.go also calls an external dependency — it must be untouched
	// by a file-scoped pass over app.go alone.
	g.AddNode(&graph.Node{ID: "other.go::f", Kind: graph.KindFunction, Name: "f", FilePath: "other.go", Language: "go"})
	g.AddEdge(&graph.Edge{From: "other.go::f", To: "dep::github.com/aws/aws-sdk-go::New", Kind: graph.EdgeCalls, FilePath: "other.go", Line: 5})

	n := SynthesizeExternalCallsForFiles(g, true, []string{"app.go"})
	assert.Equal(t, 1, n, "only app.go's external call is synthesized")

	want := externalCallNodeID("dep", "github.com/stripe/stripe-go")
	assert.Equal(t, want, outCallTarget(g, "app.go::run"), "app.go's edge retargeted onto the synthetic node")
	require.NotNil(t, g.GetNode(want), "synthetic external node materialised")

	assert.Equal(t, "dep::github.com/aws/aws-sdk-go::New", outCallTarget(g, "other.go::f"),
		"a file outside the scope keeps its raw external terminal")
}

func TestSynthesizeExternalCallsForFiles_StdlibFiltered(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "app.go::run", Kind: graph.KindFunction, Name: "run", FilePath: "app.go", Language: "go"})
	g.AddEdge(&graph.Edge{From: "app.go::run", To: "stdlib::fmt::Sprintf", Kind: graph.EdgeCalls, FilePath: "app.go", Line: 3})
	// A stdlib hop is noise — not synthesized.
	assert.Equal(t, 0, SynthesizeExternalCallsForFiles(g, true, []string{"app.go"}))
	assert.Equal(t, "stdlib::fmt::Sprintf", outCallTarget(g, "app.go::run"))
}

func TestSynthesizeExternalCallsForFiles_GatedAndEmpty(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "app.go::run", Kind: graph.KindFunction, Name: "run", FilePath: "app.go", Language: "go"})
	g.AddEdge(&graph.Edge{From: "app.go::run", To: "dep::github.com/x/y::Z", Kind: graph.EdgeCalls, FilePath: "app.go", Line: 1})

	assert.Equal(t, 0, SynthesizeExternalCallsForFiles(g, false, []string{"app.go"}), "disabled is a no-op")
	assert.Equal(t, 0, SynthesizeExternalCallsForFiles(g, true, nil), "no files is a no-op")
	// Untouched in both cases.
	assert.Equal(t, "dep::github.com/x/y::Z", outCallTarget(g, "app.go::run"))
}

// TestSynthesizeExternalCalls_Equivalence pins that the file-scoped pass
// over every file produces the same result as the full pass on a graph
// where all external calls live in known files.
func TestSynthesizeExternalCalls_Equivalence(t *testing.T) {
	build := func() graph.Store {
		g := graph.New()
		g.AddNode(&graph.Node{ID: "a.go::f", Kind: graph.KindFunction, Name: "f", FilePath: "a.go", Language: "go"})
		g.AddEdge(&graph.Edge{From: "a.go::f", To: "dep::github.com/x/y::Z", Kind: graph.EdgeCalls, FilePath: "a.go", Line: 1})
		g.AddNode(&graph.Node{ID: "b.go::g", Kind: graph.KindFunction, Name: "g", FilePath: "b.go", Language: "go"})
		g.AddEdge(&graph.Edge{From: "b.go::g", To: "external::svc.internal/api", Kind: graph.EdgeReferences, FilePath: "b.go", Line: 2})
		return g
	}
	full := build()
	scoped := build()
	nf := SynthesizeExternalCalls(full, true)
	ns := SynthesizeExternalCallsForFiles(scoped, true, []string{"a.go", "b.go"})
	assert.Equal(t, nf, ns)
	assert.Equal(t, outCallTarget(full, "a.go::f"), outCallTarget(scoped, "a.go::f"))
	assert.Equal(t, outCallTarget(full, "b.go::g"), outCallTarget(scoped, "b.go::g"))
}
