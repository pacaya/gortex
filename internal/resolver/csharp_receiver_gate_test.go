package resolver

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

func findCallEdge(g graph.Store, from, to string) *graph.Edge {
	for _, e := range g.GetOutEdges(from) {
		if e.Kind == graph.EdgeCalls && e.To == to {
			return e
		}
	}
	return nil
}

// TestReceiverGate_DemotesUnrelatedAttribution: a `g.Convert()` on a Gadget
// (which has no Convert) locality-falls-back to an unrelated Ukrainian.Convert;
// the receiver-type gate demotes that misattribution to the speculative tier so
// find_usages on Ukrainian.Convert no longer returns the Gadget site.
func TestReceiverGate_DemotesUnrelatedAttribution(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Widget.cs": `namespace App {
    public class Gadget {}
    public class Widget {
        public void DoThing() {
            Gadget g = new Gadget();
            g.Convert();
        }
    }
}`,
		"Ukr.cs": `namespace App {
    public class Ukrainian {
        public string Convert() { return "uk"; }
    }
}`,
	})
	New(g).ResolveAll()

	edge := findCallEdge(g, "Widget.cs::Widget.DoThing", "Ukr.cs::Ukrainian.Convert")
	require.NotNil(t, edge, "locality fallback should have bound the misattribution")
	require.Equal(t, "Gadget", edge.Meta["receiver_type"])
	require.False(t, edge.IsSpeculative(), "edge is visible before the gate")

	n := demoteCSharpMisattributedMemberCalls(g)
	require.Equal(t, 1, n)
	// Re-fetch: the demotion removes and re-adds the edge, so query the graph
	// for its current state rather than trusting the pre-demote pointer.
	edge = findCallEdge(g, "Widget.cs::Widget.DoThing", "Ukr.cs::Ukrainian.Convert")
	require.NotNil(t, edge)
	assert.True(t, edge.IsSpeculative(), "unrelated-type attribution must be demoted")
	assert.Equal(t, graph.OriginSpeculative, edge.Origin)
	assert.Equal(t, "receiver_type_mismatch", edge.Meta["demoted"])
}

// TestReceiverGate_PreservesValidExtensionBinding: the extension rule binds
// `w.Foo()` to a `static Foo(this Widget)` extension whose target receiver is
// the static host class (unrelated to Widget). The gate must NOT demote that
// valid binding.
func TestReceiverGate_PreservesValidExtensionBinding(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Types.cs": `namespace App {
    public class Widget {}
    public static class E {
        public static int Foo(this Widget w) { return 1; }
    }
}`,
		"Caller.cs": `namespace App {
    public class Runner {
        public void Run() {
            Widget w = new Widget();
            w.Foo();
        }
    }
}`,
	})
	New(g).ResolveAll()

	edge := findCallEdge(g, "Caller.cs::Runner.Run", "Types.cs::E.Foo")
	require.NotNil(t, edge, "w.Foo() should bind to the extension E.Foo")
	require.Equal(t, "Widget", edge.Meta["receiver_type"])
	require.Equal(t, "extension_method", edge.Meta["resolution"])

	n := demoteCSharpMisattributedMemberCalls(g)
	assert.Equal(t, 0, n, "a valid extension binding must not be demoted")
	edge = findCallEdge(g, "Caller.cs::Runner.Run", "Types.cs::E.Foo")
	require.NotNil(t, edge)
	assert.False(t, edge.IsSpeculative(), "extension binding stays visible")
}

// TestReceiverGate_PreservesSiblingCall: when a caller has both a misattributed
// and a correctly-typed call to the same target, only the misattribution is
// demoted; the coarse RemoveEdge must not drop the legitimate sibling.
func TestReceiverGate_PreservesSiblingCall(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Types.cs": `namespace App {
    public class Gadget {}
    public class Ukrainian { public string Convert() { return "uk"; } }
}`,
		"Caller.cs": `namespace App {
    public class Runner {
        public void Run() {
            Gadget g = new Gadget();
            g.Convert();
            Ukrainian u = new Ukrainian();
            u.Convert();
        }
    }
}`,
	})
	New(g).ResolveAll()

	n := demoteCSharpMisattributedMemberCalls(g)
	require.Equal(t, 1, n, "only the Gadget-receiver misattribution is demoted")

	var visible, speculative int
	for _, e := range g.GetOutEdges("Caller.cs::Runner.Run") {
		if e.Kind != graph.EdgeCalls || e.To != "Types.cs::Ukrainian.Convert" {
			continue
		}
		if e.IsSpeculative() {
			speculative++
		} else {
			visible++
		}
	}
	assert.Equal(t, 1, visible, "the correctly-typed u.Convert() call stays visible")
	assert.Equal(t, 1, speculative, "the Gadget-receiver call is demoted")
}

// TestReceiverGate_PreservesInheritedCall: a call on a subtype receiver bound to
// a method declared on its base type is a legitimate inherited call; the gate
// must NOT demote it (no false negatives on polymorphism).
func TestReceiverGate_PreservesInheritedCall(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Base.cs": `namespace App {
    public class Base {
        public string Describe() { return "base"; }
    }
}`,
		"Derived.cs": `namespace App {
    public class Derived : Base {}
    public class User {
        public void Use() {
            Derived d = new Derived();
            d.Describe();
        }
    }
}`,
	})
	New(g).ResolveAll()

	edge := findCallEdge(g, "Derived.cs::User.Use", "Base.cs::Base.Describe")
	require.NotNil(t, edge, "inherited call should bind to the base method")
	require.Equal(t, "Derived", edge.Meta["receiver_type"])

	n := demoteCSharpMisattributedMemberCalls(g)
	assert.Equal(t, 0, n, "an inherited (related-type) call must not be demoted")
	assert.False(t, edge.IsSpeculative())
}

// TestReceiverGate_PreservesCallThroughIncompleteHierarchy: a receiver whose
// base/interface is defined outside the indexed set (another assembly) has an
// unresolved supertype edge, so its hierarchy is only partially known. A call
// that locality-binds to a same-named method on an unrelated indexed type must
// NOT be demoted — the real target may live on the unindexed supertype. Without
// the hierarchy-completeness guard this legitimate polymorphic call is trimmed.
func TestReceiverGate_PreservesCallThroughIncompleteHierarchy(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Shape.cs": `namespace App {
    public class Shape {
        public void Draw() {}
    }
}`,
		"Widget.cs": `namespace App {
    public class Widget : IExternalDrawable {
        public void Render() {
            Widget w = new Widget();
            w.Draw();
        }
    }
}`,
	})
	New(g).ResolveAll()

	edge := findCallEdge(g, "Widget.cs::Widget.Render", "Shape.cs::Shape.Draw")
	require.NotNil(t, edge, "w.Draw() locality-binds to the only indexed Draw")
	require.Equal(t, "Widget", edge.Meta["receiver_type"])
	require.False(t, edge.IsSpeculative())

	n := demoteCSharpMisattributedMemberCalls(g)
	assert.Equal(t, 0, n, "a call through an incompletely-indexed hierarchy must not be demoted")
	edge = findCallEdge(g, "Widget.cs::Widget.Render", "Shape.cs::Shape.Draw")
	require.NotNil(t, edge)
	assert.False(t, edge.IsSpeculative(),
		"a possibly-legitimate polymorphic call through an unindexed supertype stays visible")
}
