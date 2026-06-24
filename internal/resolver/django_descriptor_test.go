package resolver

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// fakeClaimingResolver claims any unresolved ref named "widget" and rebinds
// it to a fixed target — exercises the generic claimsReference tier.
type fakeClaimingResolver struct{ target string }

func (fakeClaimingResolver) Name() string { return "fake-claim" }
func (fakeClaimingResolver) Claims(e *graph.Edge) bool {
	return e != nil && djangoRefName(e.To) == "widget"
}
func (f fakeClaimingResolver) Resolve(g graph.Store, e *graph.Edge) bool {
	if g.GetNode(f.target) == nil {
		return false
	}
	oldTo := e.To
	e.To = f.target
	StampSynthesized(e, "fake-claim")
	g.ReindexEdges([]graph.EdgeReindex{{Edge: e, OldTo: oldTo}})
	return true
}

// runClaimingResolversWith offers each residual unresolved edge to a single
// resolver — the generic tier, decoupled from the default registry.
func runClaimingResolversWith(g graph.Store, r ClaimingResolver) int {
	var pending []*graph.Edge
	for _, kind := range []graph.EdgeKind{graph.EdgeCalls, graph.EdgeReferences} {
		for e := range g.EdgesByKind(kind) {
			if e != nil && graph.IsUnresolvedTarget(e.To) {
				pending = append(pending, e)
			}
		}
	}
	claimed := 0
	for _, e := range pending {
		if r.Claims(e) && r.Resolve(g, e) {
			claimed++
		}
	}
	return claimed
}

func TestClaimingResolver_GenericTierClaimsResidualRef(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "w.py::Widget.render", Kind: graph.KindMethod, Name: "render", FilePath: "w.py"})
	g.AddNode(&graph.Node{ID: "w.py::Caller.go", Kind: graph.KindFunction, Name: "go", FilePath: "w.py"})
	// A residual unresolved ref no declared symbol matches.
	g.AddEdge(&graph.Edge{From: "w.py::Caller.go", To: "unresolved::*.widget", Kind: graph.EdgeCalls, FilePath: "w.py"})

	n := runClaimingResolversWith(g, fakeClaimingResolver{target: "w.py::Widget.render"})
	require.Equal(t, 1, n, "the fake resolver claims and rewrites the residual ref")

	var bound bool
	for e := range g.EdgesByKind(graph.EdgeCalls) {
		if e.From == "w.py::Caller.go" && e.To == "w.py::Widget.render" {
			bound = true
		}
	}
	assert.True(t, bound, "the unresolved ref was rebound before external-call synthesis")
}

// The Django descriptor resolver must stay wired into the default claiming
// registry — a drift fence so it cannot be silently dropped and quietly stop
// claiming Django's named-class references. Asserts membership by Name()
// directly, independent of any one resolver's functional behaviour.
func TestDefaultClaimingResolvers_IncludesDjangoDescriptor(t *testing.T) {
	names := map[string]bool{}
	for _, r := range defaultClaimingResolvers() {
		require.NotNil(t, r, "registered claiming resolvers are non-nil")
		names[r.Name()] = true
	}
	require.True(t, names[SynthDjangoDescriptor],
		"DjangoDescriptorResolver must be registered in defaultClaimingResolvers (got %v)", names)
}

func djangoIter(g *graph.Graph, id, file, class string) {
	g.AddNode(&graph.Node{ID: id, Kind: graph.KindMethod, Name: "__iter__", FilePath: file, Language: "python",
		Meta: map[string]any{"receiver": class}})
}

func djangoQuerySet(g *graph.Graph, classID, methodID, file, class, iterable string) {
	meta := map[string]any{}
	if iterable != "" {
		meta["django_iterable_class"] = iterable
	}
	g.AddNode(&graph.Node{ID: classID, Kind: graph.KindType, Name: class, FilePath: file, Language: "python", Meta: meta})
	g.AddNode(&graph.Node{ID: methodID, Kind: graph.KindMethod, Name: "iterator", FilePath: file, Language: "python",
		Meta: map[string]any{"receiver": class}})
	g.AddEdge(&graph.Edge{From: methodID, To: "unresolved::*._iterable_class", Kind: graph.EdgeCalls, FilePath: file})
}

func synthDjangoEdge(g graph.Store, from, to string) *graph.Edge {
	for e := range g.EdgesByKind(graph.EdgeCalls) {
		if e == nil || e.From != from || e.To != to || e.Meta == nil {
			continue
		}
		if by, _ := e.Meta[MetaSynthesizedBy].(string); by == SynthDjangoDescriptor {
			return e
		}
	}
	return nil
}

func TestDjangoDescriptor_IterableClassHint(t *testing.T) {
	g := graph.New()
	djangoIter(g, "m.py::ModelIterable.__iter__", "m.py", "ModelIterable")
	djangoQuerySet(g, "m.py::QuerySet", "m.py::QuerySet.iterator", "m.py", "QuerySet", "ModelIterable")

	claimed := RunClaimingResolvers(g)
	require.Equal(t, 1, claimed[SynthDjangoDescriptor])
	e := synthDjangoEdge(g, "m.py::QuerySet.iterator", "m.py::ModelIterable.__iter__")
	require.NotNil(t, e, "the QuerySet iterator binds to the iterable class's __iter__")
	assert.Equal(t, 0.7, e.Confidence)
	assert.Equal(t, ProvenanceHeuristic, e.Meta[MetaProvenance])
}

func TestDjangoDescriptor_DefaultModelIterable(t *testing.T) {
	// No hint: fall back to Django's default ModelIterable.
	g := graph.New()
	djangoIter(g, "m.py::ModelIterable.__iter__", "m.py", "ModelIterable")
	djangoQuerySet(g, "m.py::QuerySet", "m.py::QuerySet.iterator", "m.py", "QuerySet", "")

	RunClaimingResolvers(g)
	assert.NotNil(t, synthDjangoEdge(g, "m.py::QuerySet.iterator", "m.py::ModelIterable.__iter__"))
}

func TestDjangoDescriptor_DoesNotClaimUnknownRef(t *testing.T) {
	g := graph.New()
	djangoIter(g, "m.py::ModelIterable.__iter__", "m.py", "ModelIterable")
	g.AddNode(&graph.Node{ID: "m.py::C.m", Kind: graph.KindMethod, Name: "m", FilePath: "m.py", Meta: map[string]any{"receiver": "C"}})
	g.AddEdge(&graph.Edge{From: "m.py::C.m", To: "unresolved::*.something_else", Kind: graph.EdgeCalls, FilePath: "m.py"})

	assert.Equal(t, 0, RunClaimingResolvers(g)[SynthDjangoDescriptor])
}
