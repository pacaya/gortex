package resolver

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// fooCallTarget returns the To-end of the single Foo call edge leaving fromID.
func fooCallTarget(g graph.Store, fromID string) string {
	for _, e := range g.GetOutEdges(fromID) {
		if e.Kind != graph.EdgeCalls {
			continue
		}
		if graph.IsUnresolvedTarget(e.To) {
			if graph.UnresolvedName(e.To) == "*.Foo" || graph.UnresolvedName(e.To) == "Foo" {
				return e.To
			}
			continue
		}
		if n := g.GetNode(e.To); n != nil && n.Name == "Foo" {
			return e.To
		}
	}
	return ""
}

// TestResolveCSharpExtension_UniqueBinds: a call on a literal receiver with no
// type evidence binds to the sole extension method of that name.
func TestResolveCSharpExtension_UniqueBinds(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Ext.cs": `namespace App {
    public static class StringExt {
        public static int Foo(this string s) { return 1; }
    }
}`,
		"Caller.cs": `namespace App {
    public class Runner {
        public void Run() {
            "a".Foo();
        }
    }
}`,
	})
	New(g).ResolveAll()

	target := fooCallTarget(g, "Caller.cs::Runner.Run")
	require.Equal(t, "Ext.cs::StringExt.Foo", target)
	e := g.GetNode(target)
	require.NotNil(t, e)
}

// TestResolveCSharpExtension_TypedDisambiguates: with a known receiver type, the
// extension whose this_param_type matches wins over a same-named extension on
// another type.
func TestResolveCSharpExtension_TypedDisambiguates(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Exts.cs": `namespace App {
    public static class E {
        public static int Foo(this Widget w) { return 1; }
        public static int Foo(this Gadget x) { return 2; }
    }
    public class Widget {}
    public class Gadget {}
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

	target := fooCallTarget(g, "Caller.cs::Runner.Run")
	require.NotEmpty(t, target)
	require.False(t, graph.IsUnresolvedTarget(target), "typed receiver should resolve")
	n := g.GetNode(target)
	require.NotNil(t, n)
	assert.Equal(t, "Widget", n.Meta["this_param_type"], "should bind the Widget extension")
}

// TestResolveCSharpExtension_AmbiguousStaysUnresolved: two same-named extensions
// on different types + a receiver with no type evidence must not be guessed —
// neither the extension rule nor the locality fallback may pick one.
func TestResolveCSharpExtension_AmbiguousStaysUnresolved(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Exts.cs": `namespace App {
    public static class E1 { public static int Foo(this string s) { return 1; } }
    public static class E2 { public static int Foo(this int n) { return 2; } }
}`,
		"Caller.cs": `namespace App {
    public class Runner {
        public void Run(Thing t) {
            t.Foo();
        }
    }
}`,
	})
	New(g).ResolveAll()

	target := fooCallTarget(g, "Caller.cs::Runner.Run")
	require.NotEmpty(t, target)
	assert.True(t, graph.IsUnresolvedTarget(target),
		"ambiguous extension call must stay unresolved, got %q", target)
}

// TestResolveCSharpExtension_InstanceWinsUntypedReceiver: with an untyped
// receiver (a field, no receiver_type), the sole extension must NOT preempt a
// same-named instance method — the locality fallback binds the instance method.
func TestResolveCSharpExtension_InstanceWinsUntypedReceiver(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Types.cs": `namespace App {
    public class Repo { public int Count() { return 0; } }
    public static class StringExt { public static int Count(this string s) { return s.Length; } }
}`,
		"Consumer.cs": `namespace App {
    public class Consumer {
        private Repo _repo = new Repo();
        public void Run() {
            _repo.Count();
        }
    }
}`,
	})
	New(g).ResolveAll()

	var target string
	for _, e := range g.GetOutEdges("Consumer.cs::Consumer.Run") {
		if e.Kind == graph.EdgeCalls && !graph.IsUnresolvedTarget(e.To) {
			if n := g.GetNode(e.To); n != nil && n.Name == "Count" {
				target = e.To
			}
		}
	}
	assert.Equal(t, "Types.cs::Repo.Count", target,
		"an untyped receiver must not let the extension preempt the instance method")
}

// TestIsCSharpExtension_LanguageGated: the extension guard is C#-only, so a
// Scala extension-method node (which also stamps Meta[extension]) is never
// treated as a C# extension — keeping Scala's locality resolution unchanged.
func TestIsCSharpExtension_LanguageGated(t *testing.T) {
	cs := &graph.Node{Kind: graph.KindMethod, Language: "csharp", Meta: map[string]any{"extension": true}}
	scala := &graph.Node{Kind: graph.KindMethod, Language: "scala", Meta: map[string]any{"extension": true}}
	assert.True(t, isCSharpExtension(cs))
	assert.False(t, isCSharpExtension(scala),
		"a Scala extension node must not be treated as a C# extension")
}

// TestResolveCSharpExtension_InstanceWins: an instance method beats an extension
// of the same name (C# member-lookup precedence).
func TestResolveCSharpExtension_InstanceWins(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Types.cs": `namespace App {
    public class Widget {
        public int Foo() { return 0; }
    }
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

	target := fooCallTarget(g, "Caller.cs::Runner.Run")
	require.Equal(t, "Types.cs::Widget.Foo", target, "instance method should win over the extension")
}
