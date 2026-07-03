package resolver

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser/languages"
)

// buildCSharpResolverGraph extracts each C# fixture with the real extractor and
// loads its nodes/edges into a fresh graph — the same unresolved shape a live
// index produces, ready for New(g).ResolveAll().
func buildCSharpResolverGraph(t *testing.T, files map[string]string) graph.Store {
	t.Helper()
	g := graph.New()
	e := languages.NewCSharpExtractor()
	for path, src := range files {
		r, err := e.Extract(path, []byte(src))
		require.NoError(t, err, "csharp extract %s", path)
		for _, n := range r.Nodes {
			g.AddNode(n)
		}
		for _, ed := range r.Edges {
			g.AddEdge(ed)
		}
	}
	return g
}

// TestResolveCSharpInterfaceDispatch_EndToEnd drives the full path: the
// extractor emits the interface member + the through-interface call, ResolveAll
// binds the call to the interface member, and the synthesizer fans it out to
// both concrete implementations at the speculative tier.
func TestResolveCSharpInterfaceDispatch_EndToEnd(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"IConverter.cs": `namespace App {
    public interface IConverter {
        string Convert(int n);
    }
}`,
		"English.cs": `namespace App {
    public class EnglishConverter : IConverter {
        public string Convert(int n) { return "en"; }
    }
}`,
		"Ukrainian.cs": `namespace App {
    public class UkrainianConverter : IConverter {
        public string Convert(int n) { return "uk"; }
    }
}`,
		"Runner.cs": `namespace App {
    public class Runner {
        public void Run(IConverter c) {
            IConverter conv = c;
            conv.Convert(1);
        }
    }
}`,
	})
	New(g).ResolveAll()

	callerID := "Runner.cs::Runner.Run"
	ifaceMember := "IConverter.cs::IConverter.Convert"

	// (a) the through-interface call binds to the interface member.
	require.Contains(t, callTargetsFrom(g, callerID), ifaceMember,
		"through-interface call should bind to the interface member node")

	// (b) fan-out to both implementations, speculative tier.
	n := ResolveCSharpInterfaceDispatch(g)
	require.Equal(t, 2, n, "one fan-out edge per implementation")

	spec := map[string]*graph.Edge{}
	for _, e := range g.GetOutEdges(callerID) {
		if e.Kind == graph.EdgeCalls && e.IsSpeculative() {
			spec[e.To] = e
		}
	}
	for _, id := range []string{"English.cs::EnglishConverter.Convert", "Ukrainian.cs::UkrainianConverter.Convert"} {
		e := spec[id]
		require.NotNil(t, e, "expected speculative fan-out edge to %s", id)
		assert.Equal(t, graph.OriginSpeculative, e.Origin)
		assert.True(t, e.IsSpeculative())
		assert.Equal(t, SynthCSharpIfaceDispatch, e.Meta[MetaSynthesizedBy])
	}
}

// TestResolveCSharpInterfaceDispatch_FanoutTierAndCap uses a hand-built graph to
// pin the fan-out shape, provenance/tier, dedup, and the fan-out cap.
func TestResolveCSharpInterfaceDispatch_FanoutTierAndCap(t *testing.T) {
	newGraph := func(nImpls int) graph.Store {
		g := graph.New()
		g.AddNode(&graph.Node{ID: "I.cs::IC", Kind: graph.KindInterface, Name: "IC", FilePath: "I.cs", Language: "csharp"})
		g.AddNode(&graph.Node{ID: "I.cs::IC.Do", Kind: graph.KindMethod, Name: "Do", FilePath: "I.cs", Language: "csharp",
			Meta: map[string]any{"receiver": "IC", "iface_member": true}})
		g.AddEdge(&graph.Edge{From: "I.cs::IC.Do", To: "I.cs::IC", Kind: graph.EdgeMemberOf, FilePath: "I.cs"})
		for i := 0; i < nImpls; i++ {
			typ := fmt.Sprintf("Impl%d", i)
			file := typ + ".cs"
			g.AddNode(&graph.Node{ID: file + "::" + typ, Kind: graph.KindType, Name: typ, FilePath: file, Language: "csharp"})
			g.AddNode(&graph.Node{ID: file + "::" + typ + ".Do", Kind: graph.KindMethod, Name: "Do", FilePath: file, Language: "csharp",
				Meta: map[string]any{"receiver": typ}})
			g.AddEdge(&graph.Edge{From: file + "::" + typ + ".Do", To: file + "::" + typ, Kind: graph.EdgeMemberOf, FilePath: file})
			g.AddEdge(&graph.Edge{From: file + "::" + typ, To: "I.cs::IC", Kind: graph.EdgeImplements, FilePath: file})
		}
		g.AddNode(&graph.Node{ID: "C.cs::App.Run", Kind: graph.KindMethod, Name: "Run", FilePath: "C.cs", Language: "csharp",
			Meta: map[string]any{"receiver": "App"}})
		g.AddEdge(&graph.Edge{From: "C.cs::App.Run", To: "I.cs::IC.Do", Kind: graph.EdgeCalls, FilePath: "C.cs", Line: 3})
		return g
	}

	t.Run("fan-out tier + dedup", func(t *testing.T) {
		g := newGraph(2)
		require.Equal(t, 2, ResolveCSharpInterfaceDispatch(g))

		spec := map[string]*graph.Edge{}
		for _, e := range g.GetOutEdges("C.cs::App.Run") {
			if e.Kind == graph.EdgeCalls && e.IsSpeculative() {
				spec[e.To] = e
			}
		}
		require.Len(t, spec, 2)
		for _, id := range []string{"Impl0.cs::Impl0.Do", "Impl1.cs::Impl1.Do"} {
			e := spec[id]
			require.NotNil(t, e, id)
			assert.Equal(t, graph.OriginSpeculative, e.Origin)
			assert.Equal(t, SynthCSharpIfaceDispatch, e.Meta[MetaSynthesizedBy])
			assert.Equal(t, "C.cs", e.FilePath)
			assert.Equal(t, 3, e.Line)
		}

		// Idempotent: a second run dedups, leaving exactly two fan-out edges.
		ResolveCSharpInterfaceDispatch(g)
		got := 0
		for _, e := range g.GetOutEdges("C.cs::App.Run") {
			if e.Kind == graph.EdgeCalls && e.IsSpeculative() {
				got++
			}
		}
		assert.Equal(t, 2, got, "re-run must not duplicate fan-out edges")
	})

	t.Run("fan-out cap drops noise", func(t *testing.T) {
		g := newGraph(csharpIfaceDispatchCap + 1)
		assert.Equal(t, 0, ResolveCSharpInterfaceDispatch(g),
			"a fan-out wider than the cap is dropped as noise")
	})
}
