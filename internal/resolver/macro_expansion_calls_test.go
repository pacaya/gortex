package resolver

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser/languages"
)

// loadCSource extracts a C source string with the real C extractor and
// loads its nodes + edges (plus a backing file node) into a fresh graph —
// the faithful end-to-end harness: a real extractor produces the macro
// node, its recovered body-callee edges, and the unresolved use-site call
// edge, exactly as a live index would.
func loadCSource(t *testing.T, path, src string) graph.Store {
	t.Helper()
	g := graph.New()
	r, err := languages.NewCExtractor().Extract(path, []byte(src))
	require.NoError(t, err)
	g.AddNode(&graph.Node{ID: path, Kind: graph.KindFile, Name: path, FilePath: path, Language: "c"})
	for _, n := range r.Nodes {
		g.AddNode(n)
	}
	for _, e := range r.Edges {
		g.AddEdge(e)
	}
	return g
}

// outCallEdge returns the call edge leaving fromID to a target whose
// UnresolvedName / id equals wantCallee at the given 1-based line, or nil.
func outCallEdge(g graph.Store, fromID, wantCallee string, line int) *graph.Edge {
	for _, e := range g.GetOutEdges(fromID) {
		if e.Kind != graph.EdgeCalls || e.Line != line {
			continue
		}
		if e.To == wantCallee || graph.UnresolvedName(e.To) == wantCallee ||
			strings.HasSuffix(e.To, "::"+wantCallee) {
			return e
		}
	}
	return nil
}

// A function-like macro's body call is re-attributed to the use site: the
// `CALL_M(o);` line inside the calling function, not the `#define` line.
func TestMacroExpansion_AttributesCalleeToUseSite(t *testing.T) {
	src := "#define CALL_M(o) (o)->run()\n" + // line 1: definition
		"\n" +
		"void use(Obj* o) {\n" + // line 3
		"    CALL_M(o);\n" + // line 4: use site
		"}\n"
	g := loadCSource(t, "u.c", src)
	New(g).ResolveAll()

	owned := ResolveMacroExpansionCalls(g)
	assert.Equal(t, 1, owned, "one use-site callee edge synthesized")

	// The minted edge: use -> run, at the USE-SITE line (4), not the
	// #define line (1).
	useSite := outCallEdge(g, "u.c::use", "run", 4)
	require.NotNil(t, useSite, "use -> run must exist at the use-site line 4")
	assert.Equal(t, macroExpansionVia, useSite.Meta["via"])
	assert.Equal(t, SynthMacroExpansion, useSite.Meta[MetaSynthesizedBy])
	assert.Equal(t, graph.OriginASTInferred, useSite.Origin)
	assert.Equal(t, "CALL_M", useSite.Meta["macro"])

	// No use -> run edge is attributed to the #define line (1) — the
	// re-attribution is to the use site, not the definition.
	assert.Nil(t, outCallEdge(g, "u.c::use", "run", 1),
		"no use -> run edge at the #define line")

	// Coexistence: the extractor's def-line edge (macro -> run, line 1)
	// and the use-site placeholder (use -> CALL_M, line 4) are untouched.
	assert.NotNil(t, outCallEdge(g, "u.c::CALL_M", "run", 1),
		"def-line macro -> run edge survives")
	assert.NotNil(t, outCallEdge(g, "u.c::use", "CALL_M", 4),
		"use-site placeholder use -> CALL_M survives")
}

// Re-running the pass is idempotent: the edge key includes the line, so a
// second run lands the same owned count and adds no duplicate edge.
func TestMacroExpansion_Idempotent(t *testing.T) {
	src := "#define LOG(m) write_log(m)\n" +
		"void f(void) {\n" +
		"    LOG(1);\n" + // line 3
		"}\n"
	g := loadCSource(t, "m.c", src)
	New(g).ResolveAll()

	first := ResolveMacroExpansionCalls(g)
	second := ResolveMacroExpansionCalls(g)
	assert.Equal(t, first, second, "owned count stable across runs")

	n := 0
	for _, e := range g.GetOutEdges("m.c::f") {
		if e.Kind == graph.EdgeCalls && graph.UnresolvedName(e.To) == "write_log" {
			n++
		}
	}
	assert.Equal(t, 1, n, "exactly one synthesized use-site edge, no duplicate")
}

// An object-like (non-parameterised) macro has no call use site, so the
// pass never fires for it even when its replacement names a call.
func TestMacroExpansion_SkipsObjectLikeMacro(t *testing.T) {
	src := "#define WIDTH compute()\n" +
		"void g(void) {\n" +
		"    int w = WIDTH;\n" + // line 3
		"}\n"
	g := loadCSource(t, "o.c", src)
	New(g).ResolveAll()

	assert.Equal(t, 0, ResolveMacroExpansionCalls(g),
		"object-like macro use site is not a call_expression")
}

// Two function-like macros sharing a name in the same repo make any use
// of that name ambiguous; the pass refuses to guess and synthesizes
// nothing for it.
func TestMacroExpansion_AmbiguousNameSkipped(t *testing.T) {
	g := graph.New()
	// Two macros named DO, different bodies, both with recovered callees.
	g.AddNode(&graph.Node{ID: "a.c::DO", Kind: graph.KindMacro, Name: "DO", FilePath: "a.c",
		Meta: map[string]any{"macro_kind": "function"}})
	g.AddEdge(&graph.Edge{From: "a.c::DO", To: "unresolved::alpha", Kind: graph.EdgeCalls,
		FilePath: "a.c", Line: 1, Origin: graph.OriginASTInferred})
	g.AddNode(&graph.Node{ID: "b.c::DO", Kind: graph.KindMacro, Name: "DO", FilePath: "b.c",
		Meta: map[string]any{"macro_kind": "function"}})
	g.AddEdge(&graph.Edge{From: "b.c::DO", To: "unresolved::beta", Kind: graph.EdgeCalls,
		FilePath: "b.c", Line: 1, Origin: graph.OriginASTInferred})
	// A caller invoking DO.
	g.AddNode(&graph.Node{ID: "c.c::caller", Kind: graph.KindFunction, Name: "caller", FilePath: "c.c"})
	g.AddEdge(&graph.Edge{From: "c.c::caller", To: "unresolved::DO", Kind: graph.EdgeCalls,
		FilePath: "c.c", Line: 7})

	assert.Equal(t, 0, ResolveMacroExpansionCalls(g),
		"ambiguous macro name synthesizes nothing")
	assert.Nil(t, outCallEdge(g, "c.c::caller", "alpha", 7))
	assert.Nil(t, outCallEdge(g, "c.c::caller", "beta", 7))
}

// A use-site call edge landed directly on the macro node (rather than
// left as an `unresolved::<macro>` placeholder) is also re-attributed —
// the defensive second seam.
func TestMacroExpansion_UseSiteLandedOnMacroNode(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "k.c::WRAP", Kind: graph.KindMacro, Name: "WRAP", FilePath: "k.c",
		Meta: map[string]any{"macro_kind": "function"}})
	g.AddEdge(&graph.Edge{From: "k.c::WRAP", To: "unresolved::inner", Kind: graph.EdgeCalls,
		FilePath: "k.c", Line: 1, Origin: graph.OriginASTInferred})
	g.AddNode(&graph.Node{ID: "k.c::caller", Kind: graph.KindFunction, Name: "caller", FilePath: "k.c"})
	// Use site already bound to the macro node itself, at line 5.
	g.AddEdge(&graph.Edge{From: "k.c::caller", To: "k.c::WRAP", Kind: graph.EdgeCalls,
		FilePath: "k.c", Line: 5})

	assert.Equal(t, 1, ResolveMacroExpansionCalls(g))
	mint := outCallEdge(g, "k.c::caller", "inner", 5)
	require.NotNil(t, mint, "caller -> inner minted at the use-site line 5")
	assert.Equal(t, macroExpansionVia, mint.Meta["via"])
}

// A real edge already occupying the exact (from, to, file, line) identity
// slot is never overwritten or downgraded — the load-bearing invariant.
func TestMacroExpansion_DoesNotOverwriteRealEdge(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "h.c::M", Kind: graph.KindMacro, Name: "M", FilePath: "h.c",
		Meta: map[string]any{"macro_kind": "function"}})
	g.AddNode(&graph.Node{ID: "h.c::run", Kind: graph.KindFunction, Name: "run", FilePath: "h.c"})
	g.AddEdge(&graph.Edge{From: "h.c::M", To: "h.c::run", Kind: graph.EdgeCalls,
		FilePath: "h.c", Line: 1, Origin: graph.OriginASTInferred})
	g.AddNode(&graph.Node{ID: "h.c::caller", Kind: graph.KindFunction, Name: "caller", FilePath: "h.c"})
	g.AddEdge(&graph.Edge{From: "h.c::caller", To: "unresolved::M", Kind: graph.EdgeCalls,
		FilePath: "h.c", Line: 9})
	// A pre-existing compiler-grade edge occupies the slot the pass would
	// mint into (caller -> run at line 9).
	g.AddEdge(&graph.Edge{From: "h.c::caller", To: "h.c::run", Kind: graph.EdgeCalls,
		FilePath: "h.c", Line: 9, Origin: graph.OriginLSPResolved})

	ResolveMacroExpansionCalls(g)

	got := outCallEdge(g, "h.c::caller", "run", 9)
	require.NotNil(t, got)
	assert.Equal(t, graph.OriginLSPResolved, got.Origin,
		"the real LSP edge is preserved, not downgraded")
	_, stamped := got.Meta["via"]
	assert.False(t, stamped, "the real edge is not stamped with macro-expansion provenance")
}
