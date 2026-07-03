package resolver

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/zzet/gortex/internal/graph"
)

// An ambiguous member call whose same-name candidates are overrides sharing a
// common supertype fans out to every override at the speculative tier, matching
// the language server's call-hierarchy semantics. Unrelated same-name methods
// are left ambiguous.
func TestResolveJavaOverrideDispatch(t *testing.T) {
	g := graph.New()
	baseF := "src/main/java/org/example/model/NamedEntity.java"
	ownerF := "src/main/java/org/example/owner/Owner.java"
	sysF := "src/main/java/org/example/system/PropertiesLogger.java"
	widgetF := "src/main/java/org/example/ui/Widget.java"

	for _, f := range []string{baseF, ownerF, sysF, widgetF} {
		g.AddNode(&graph.Node{ID: f, Kind: graph.KindFile, Name: f, FilePath: f, Language: "java"})
	}
	// Hierarchy recorded via scope_parent (how the Java extractor records a
	// superclass — regular `extends` is not a graph EdgeExtends): both Owner
	// and NamedEntity extend BaseEntity, so they share it as a common ancestor.
	g.AddNode(&graph.Node{ID: baseF + "::NamedEntity", Kind: graph.KindType, Name: "NamedEntity", FilePath: baseF, Language: "java", Meta: map[string]any{"scope_parent": "BaseEntity"}})
	g.AddNode(&graph.Node{ID: ownerF + "::Owner", Kind: graph.KindType, Name: "Owner", FilePath: ownerF, Language: "java", Meta: map[string]any{"scope_parent": "Person"}})
	g.AddNode(&graph.Node{ID: ownerF + "::Person", Kind: graph.KindType, Name: "Person", FilePath: ownerF, Language: "java", Meta: map[string]any{"scope_parent": "BaseEntity"}})
	// Both override toString.
	g.AddNode(&graph.Node{ID: baseF + "::NamedEntity.toString", Kind: graph.KindMethod, Name: "toString", FilePath: baseF, Language: "java", Meta: map[string]any{"receiver": "NamedEntity"}})
	g.AddNode(&graph.Node{ID: ownerF + "::Owner.toString", Kind: graph.KindMethod, Name: "toString", FilePath: ownerF, Language: "java", Meta: map[string]any{"receiver": "Owner"}})
	// An unrelated Widget.render in a separate hierarchy — must NOT join.
	g.AddNode(&graph.Node{ID: widgetF + "::Widget", Kind: graph.KindType, Name: "Widget", FilePath: widgetF, Language: "java"})
	g.AddNode(&graph.Node{ID: widgetF + "::Widget.render", Kind: graph.KindMethod, Name: "render", FilePath: widgetF, Language: "java", Meta: map[string]any{"receiver": "Widget"}})

	caller := sysF + "::PropertiesLogger.printProperties"
	g.AddNode(&graph.Node{ID: caller, Kind: graph.KindMethod, Name: "printProperties", FilePath: sysF, Language: "java", Meta: map[string]any{"receiver": "PropertiesLogger"}})
	// sourceProperty.toString() at two call sites, receiver type unknown.
	g.AddEdge(&graph.Edge{From: caller, To: "unresolved::*.toString", Kind: graph.EdgeCalls, FilePath: sysF, Line: 125})
	g.AddEdge(&graph.Edge{From: caller, To: "unresolved::*.toString", Kind: graph.EdgeCalls, FilePath: sysF, Line: 127})
	// A lone-candidate call that must NOT fan out (only render exists).
	g.AddEdge(&graph.Edge{From: caller, To: "unresolved::*.render", Kind: graph.EdgeCalls, FilePath: sysF, Line: 130})

	r := New(g)
	r.ResolveAll()

	// Every override receives the call at both sites, at the speculative
	// override tier (present in the graph, gated out of default responses).
	for _, target := range []string{baseF + "::NamedEntity.toString", ownerF + "::Owner.toString"} {
		got := map[int]bool{}
		for _, in := range g.GetInEdges(target) {
			if in.Kind == graph.EdgeCalls && in.From == caller {
				assert.Equal(t, "override", in.Meta["dispatch"], "fan-out edge must be marked dispatch=override")
				assert.True(t, in.IsSpeculative(), "override fan-out edge must land at the speculative tier")
				got[in.Line] = true
			}
		}
		assert.True(t, got[125], "toString override %s must receive call site 125", target)
		assert.True(t, got[127], "toString override %s must receive call site 127", target)
	}

	// No `unresolved::*.toString` edge remains — the ambiguity is resolved.
	for _, e := range g.GetOutEdges(caller) {
		assert.NotEqual(t, "unresolved::*.toString", e.To, "no toString call should remain ambiguous after fan-out")
	}
}
