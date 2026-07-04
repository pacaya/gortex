package resolver

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

// fakeLSPHelper is a deterministic mock implementing LSPHelper for
// tests. exts narrows which file paths it claims; defs is the
// canonical mapping from (callerPath, line, name) → (defPath, line).
type fakeLSPHelper struct {
	exts        []string
	defs        map[lspKey]lspAnswer
	calls       int
	inFlight    int
	maxInFlight int // high-water mark of concurrent Definition calls
	mu          sync.Mutex
	hangCh      chan struct{} // optional: when non-nil, blocks Definition until closed (timeout testing)
}

type lspKey struct {
	path string
	line int
	name string
}

type lspAnswer struct {
	defPath string
	defLine int
}

func (f *fakeLSPHelper) SupportsPath(relPath string) bool {
	if len(f.exts) == 0 {
		return true
	}
	for _, e := range f.exts {
		if hasSuffix(relPath, e) {
			return true
		}
	}
	return false
}

func (f *fakeLSPHelper) Definition(relPath string, line int, name string) (string, int, bool) {
	f.mu.Lock()
	f.calls++
	f.inFlight++
	if f.inFlight > f.maxInFlight {
		f.maxInFlight = f.inFlight
	}
	f.mu.Unlock()
	if f.hangCh != nil {
		<-f.hangCh
	}
	f.mu.Lock()
	f.inFlight--
	f.mu.Unlock()
	a, ok := f.defs[lspKey{path: relPath, line: line, name: name}]
	if !ok {
		return "", 0, false
	}
	return a.defPath, a.defLine, true
}

func hasSuffix(s, suffix string) bool {
	return len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix
}

// TestLSPHotPath_BarrelReExport — the canonical case the heuristic
// loses: a method called by selector through a barrel re-export. The
// AST resolver can find a same-named target anywhere in the repo
// (potentially the wrong one); the LSP definition lookup pins the
// edge to the precise re-exported declaration.
//
// Driven through the single-file ResolveFile path: that is where the LSP
// helper is consulted inline (LSP-first), the behaviour an interactive edit
// relies on. A whole-graph ResolveAll runs in bulk mode, where LSP is deferred
// to a post-loop mop-up for the edges the heuristic cascade leaves unresolved
// (see TestLSPHotPath_BulkDefersLSP*).
func TestLSPHotPath_BarrelReExport(t *testing.T) {
	g := graph.New()
	// Files
	g.AddNode(&graph.Node{ID: "src/caller.ts", Kind: graph.KindFile, Name: "caller.ts", FilePath: "src/caller.ts", Language: "typescript"})
	g.AddNode(&graph.Node{ID: "src/real.ts", Kind: graph.KindFile, Name: "real.ts", FilePath: "src/real.ts", Language: "typescript"})
	g.AddNode(&graph.Node{ID: "src/decoy.ts", Kind: graph.KindFile, Name: "decoy.ts", FilePath: "src/decoy.ts", Language: "typescript"})

	// Decoy: same name in another file — the heuristic resolver
	// would pick this first because filterByReachability and same-
	// dir bias both fail.
	g.AddNode(&graph.Node{
		ID: "src/decoy.ts::doWork", Kind: graph.KindFunction, Name: "doWork",
		FilePath: "src/decoy.ts", StartLine: 12, EndLine: 14, Language: "typescript",
	})
	// Real definition the LSP will report.
	g.AddNode(&graph.Node{
		ID: "src/real.ts::doWork", Kind: graph.KindFunction, Name: "doWork",
		FilePath: "src/real.ts", StartLine: 7, EndLine: 9, Language: "typescript",
	})
	// Caller
	g.AddNode(&graph.Node{
		ID: "src/caller.ts::callIt", Kind: graph.KindFunction, Name: "callIt",
		FilePath: "src/caller.ts", StartLine: 3, EndLine: 5, Language: "typescript",
	})

	callEdge := &graph.Edge{
		From: "src/caller.ts::callIt", To: "unresolved::doWork",
		Kind: graph.EdgeCalls, FilePath: "src/caller.ts", Line: 4,
	}
	g.AddEdge(callEdge)

	helper := &fakeLSPHelper{
		exts: []string{".ts"},
		defs: map[lspKey]lspAnswer{
			{path: "src/caller.ts", line: 4, name: "doWork"}: {defPath: "src/real.ts", defLine: 7},
		},
	}

	r := New(g)
	r.SetLSPHelper(helper)
	stats := r.ResolveFile("src/caller.ts")

	require.Equal(t, 1, stats.Resolved)
	assert.Equal(t, "src/real.ts::doWork", callEdge.To, "edge must bind to LSP-reported definition, not the decoy")
	assert.Equal(t, graph.OriginLSPResolved, callEdge.Origin)
	require.NotNil(t, callEdge.Meta)
	assert.Equal(t, "lsp", callEdge.Meta["resolved_by"])
	assert.Equal(t, 1, helper.calls)
}

// TestLSPHotPath_FallthroughOnMiss — when the LSP returns no answer,
// the heuristic cascade still runs. The edge gets resolved by the
// AST resolver and its Origin reflects the AST tier (NOT lsp_*).
func TestLSPHotPath_FallthroughOnMiss(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "src/a.ts", Kind: graph.KindFile, Name: "a.ts", FilePath: "src/a.ts", Language: "typescript"})
	g.AddNode(&graph.Node{ID: "src/b.ts", Kind: graph.KindFile, Name: "b.ts", FilePath: "src/b.ts", Language: "typescript"})
	g.AddNode(&graph.Node{ID: "src/a.ts::caller", Kind: graph.KindFunction, Name: "caller", FilePath: "src/a.ts", Language: "typescript"})
	g.AddNode(&graph.Node{
		ID: "src/b.ts::theTarget", Kind: graph.KindFunction, Name: "theTarget",
		FilePath: "src/b.ts", StartLine: 4, EndLine: 6, Language: "typescript",
	})

	callEdge := &graph.Edge{
		From: "src/a.ts::caller", To: "unresolved::theTarget",
		Kind: graph.EdgeCalls, FilePath: "src/a.ts", Line: 2,
	}
	g.AddEdge(callEdge)

	helper := &fakeLSPHelper{
		exts: []string{".ts"},
		defs: map[lspKey]lspAnswer{}, // empty — every call misses
	}

	r := New(g)
	r.SetLSPHelper(helper)
	stats := r.ResolveAll()

	require.Equal(t, 1, stats.Resolved, "heuristic cascade should still resolve")
	assert.Equal(t, "src/b.ts::theTarget", callEdge.To)
	assert.NotEqual(t, graph.OriginLSPResolved, callEdge.Origin, "miss → heuristic tier, not lsp_resolved")
	if callEdge.Meta != nil {
		assert.NotEqual(t, "lsp", callEdge.Meta["resolved_by"])
	}
}

// TestLSPHotPath_ExtensionGate — the helper short-circuits on
// SupportsPath, so a Go-file edge doesn't trigger any LSP call when
// the helper claims only TS extensions. The heuristic resolver
// produces the answer.
func TestLSPHotPath_ExtensionGate(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "a.go", Kind: graph.KindFile, Name: "a.go", FilePath: "a.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "b.go", Kind: graph.KindFile, Name: "b.go", FilePath: "b.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "a.go::caller", Kind: graph.KindFunction, Name: "caller", FilePath: "a.go", Language: "go"})
	g.AddNode(&graph.Node{
		ID: "b.go::target", Kind: graph.KindFunction, Name: "target",
		FilePath: "b.go", StartLine: 3, EndLine: 5, Language: "go",
	})

	callEdge := &graph.Edge{
		From: "a.go::caller", To: "unresolved::target",
		Kind: graph.EdgeCalls, FilePath: "a.go", Line: 2,
	}
	g.AddEdge(callEdge)

	helper := &fakeLSPHelper{
		exts: []string{".ts", ".tsx"},
	}

	r := New(g)
	r.SetLSPHelper(helper)
	stats := r.ResolveAll()

	assert.Equal(t, 1, stats.Resolved)
	assert.Equal(t, "b.go::target", callEdge.To)
	assert.Equal(t, 0, helper.calls, "helper must NOT be called for non-claimed extensions")
}

// TestLSPHotPath_KindGate — the LSP helper returns a file-node
// location, but the edge is a `calls` edge that must land on a
// function/method/closure. lspKindAcceptableFor rejects the bind
// and the heuristic falls through.
func TestLSPHotPath_KindGate(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "src/caller.ts", Kind: graph.KindFile, Name: "caller.ts", FilePath: "src/caller.ts", Language: "typescript"})
	g.AddNode(&graph.Node{ID: "src/util.ts", Kind: graph.KindFile, Name: "util.ts", FilePath: "src/util.ts", Language: "typescript", StartLine: 1})
	g.AddNode(&graph.Node{ID: "src/caller.ts::callIt", Kind: graph.KindFunction, Name: "callIt", FilePath: "src/caller.ts", Language: "typescript"})
	g.AddNode(&graph.Node{
		ID: "src/util.ts::reallyDoIt", Kind: graph.KindFunction, Name: "reallyDoIt",
		FilePath: "src/util.ts", StartLine: 5, EndLine: 7, Language: "typescript",
	})

	callEdge := &graph.Edge{
		From: "src/caller.ts::callIt", To: "unresolved::reallyDoIt",
		Kind: graph.EdgeCalls, FilePath: "src/caller.ts", Line: 2,
	}
	g.AddEdge(callEdge)

	// LSP points the edge at the file node, not the function node —
	// the kind-gate should reject.
	helper := &fakeLSPHelper{
		exts: []string{".ts"},
		defs: map[lspKey]lspAnswer{
			{path: "src/caller.ts", line: 2, name: "reallyDoIt"}: {defPath: "src/util.ts", defLine: 1},
		},
	}

	r := New(g)
	r.SetLSPHelper(helper)
	stats := r.ResolveAll()

	require.Equal(t, 1, stats.Resolved)
	// Heuristic picked the function node, not the file.
	assert.Equal(t, "src/util.ts::reallyDoIt", callEdge.To)
	// Origin must NOT be lsp_resolved — gate rejected the LSP answer.
	assert.NotEqual(t, graph.OriginLSPResolved, callEdge.Origin)
}

// TestLSPHotPath_MethodSelector — the resolver receives an unresolved
// `*.Name` selector target. tryResolveViaLSP strips the prefix and
// asks the helper for `Name`. On a hit, the method edge binds to the
// LSP-reported target across files.
func TestLSPHotPath_MethodSelector(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "src/caller.ts", Kind: graph.KindFile, Name: "caller.ts", FilePath: "src/caller.ts", Language: "typescript"})
	g.AddNode(&graph.Node{ID: "src/svc.ts", Kind: graph.KindFile, Name: "svc.ts", FilePath: "src/svc.ts", Language: "typescript"})
	g.AddNode(&graph.Node{ID: "src/decoy.ts", Kind: graph.KindFile, Name: "decoy.ts", FilePath: "src/decoy.ts", Language: "typescript"})

	g.AddNode(&graph.Node{
		ID: "src/svc.ts::Service.handle", Kind: graph.KindMethod, Name: "handle",
		FilePath: "src/svc.ts", StartLine: 10, EndLine: 12, Language: "typescript",
	})
	g.AddNode(&graph.Node{
		ID: "src/decoy.ts::Other.handle", Kind: graph.KindMethod, Name: "handle",
		FilePath: "src/decoy.ts", StartLine: 5, EndLine: 7, Language: "typescript",
	})
	g.AddNode(&graph.Node{ID: "src/caller.ts::callIt", Kind: graph.KindFunction, Name: "callIt", FilePath: "src/caller.ts", Language: "typescript"})

	callEdge := &graph.Edge{
		From: "src/caller.ts::callIt", To: "unresolved::*.handle",
		Kind: graph.EdgeCalls, FilePath: "src/caller.ts", Line: 9,
	}
	g.AddEdge(callEdge)

	helper := &fakeLSPHelper{
		exts: []string{".ts"},
		defs: map[lspKey]lspAnswer{
			{path: "src/caller.ts", line: 9, name: "handle"}: {defPath: "src/svc.ts", defLine: 10},
		},
	}

	r := New(g)
	r.SetLSPHelper(helper)
	stats := r.ResolveFile("src/caller.ts")

	require.Equal(t, 1, stats.Resolved)
	assert.Equal(t, "src/svc.ts::Service.handle", callEdge.To)
	assert.Equal(t, graph.OriginLSPResolved, callEdge.Origin)
	assert.Equal(t, "lsp", callEdge.Meta["resolved_by"])
}

// TestLSPHotPath_MethodValueReadPromotesToReferences — when the LSP
// helper binds an EdgeReads to a KindMethod (the `mux.HandleFunc("/p",
// h.foo)` shape where h.foo is passed as a method value), the kind
// must be promoted to EdgeReferences. The heuristic cascade already
// does this in resolver.go's `*. + Reads/Writes` case; the LSP hot
// path used to short-circuit before that branch ran and silently
// leave the kind as EdgeReads — which GetCallers/FindUsages drop
// (they only follow Calls/Matches/References). Every HTTP handler in
// every router-style codebase looked like dead code as a result.
func TestLSPHotPath_MethodValueReadPromotesToReferences(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "src/routes.go", Kind: graph.KindFile, Name: "routes.go", FilePath: "src/routes.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "src/handler.go", Kind: graph.KindFile, Name: "handler.go", FilePath: "src/handler.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "src/routes.go::RegisterRoutes", Kind: graph.KindFunction, Name: "RegisterRoutes", FilePath: "src/routes.go", Language: "go"})
	g.AddNode(&graph.Node{
		ID: "src/handler.go::Handler.HandleHealth", Kind: graph.KindMethod, Name: "HandleHealth",
		FilePath: "src/handler.go", StartLine: 42, EndLine: 45, Language: "go",
	})

	readEdge := &graph.Edge{
		From: "src/routes.go::RegisterRoutes", To: "unresolved::*.HandleHealth",
		Kind: graph.EdgeReads, FilePath: "src/routes.go", Line: 10,
	}
	g.AddEdge(readEdge)

	helper := &fakeLSPHelper{
		exts: []string{".go"},
		defs: map[lspKey]lspAnswer{
			{path: "src/routes.go", line: 10, name: "HandleHealth"}: {defPath: "src/handler.go", defLine: 42},
		},
	}

	r := New(g)
	r.SetLSPHelper(helper)
	stats := r.ResolveFile("src/routes.go")

	require.Equal(t, 1, stats.Resolved)
	assert.Equal(t, "src/handler.go::Handler.HandleHealth", readEdge.To)
	assert.Equal(t, graph.OriginLSPResolved, readEdge.Origin)
	assert.Equal(t, graph.EdgeReferences, readEdge.Kind,
		"LSP-bound EdgeReads on a KindMethod must be promoted so get_callers surfaces it")
}

// TestLSPHotPath_FunctionValueReadPromotesToReferences — companion
// to the method-value test above. The cobra/CLI pattern
// `&cobra.Command{RunE: runClean}` emits EdgeReads with To=
// "unresolved::runClean", and the LSP helper happily binds it to
// the runClean function. Without promotion the wire-up site is
// invisible to get_callers, so every cobra subcommand looked dead.
func TestLSPHotPath_FunctionValueReadPromotesToReferences(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "cmd/x/main.go", Kind: graph.KindFile, Name: "main.go", FilePath: "cmd/x/main.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "cmd/x/clean.go", Kind: graph.KindFile, Name: "clean.go", FilePath: "cmd/x/clean.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "cmd/x/main.go::init", Kind: graph.KindFunction, Name: "init", FilePath: "cmd/x/main.go", Language: "go"})
	g.AddNode(&graph.Node{
		ID: "cmd/x/clean.go::runClean", Kind: graph.KindFunction, Name: "runClean",
		FilePath: "cmd/x/clean.go", StartLine: 20, EndLine: 30, Language: "go",
	})

	readEdge := &graph.Edge{
		From: "cmd/x/main.go::init", To: "unresolved::runClean",
		Kind: graph.EdgeReads, FilePath: "cmd/x/main.go", Line: 13,
	}
	g.AddEdge(readEdge)

	helper := &fakeLSPHelper{
		exts: []string{".go"},
		defs: map[lspKey]lspAnswer{
			{path: "cmd/x/main.go", line: 13, name: "runClean"}: {defPath: "cmd/x/clean.go", defLine: 20},
		},
	}

	r := New(g)
	r.SetLSPHelper(helper)
	stats := r.ResolveFile("cmd/x/main.go")

	require.Equal(t, 1, stats.Resolved)
	assert.Equal(t, "cmd/x/clean.go::runClean", readEdge.To)
	assert.Equal(t, graph.OriginLSPResolved, readEdge.Origin)
	assert.Equal(t, graph.EdgeReferences, readEdge.Kind,
		"LSP-bound EdgeReads on a KindFunction must promote to References")
}

// TestLSPHotPath_NilHelper — when no helper is installed, the
// resolver runs heuristic-only as in the pre-N5 world.
func TestLSPHotPath_NilHelper(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "a.ts", Kind: graph.KindFile, Name: "a.ts", FilePath: "a.ts", Language: "typescript"})
	g.AddNode(&graph.Node{ID: "b.ts", Kind: graph.KindFile, Name: "b.ts", FilePath: "b.ts", Language: "typescript"})
	g.AddNode(&graph.Node{ID: "a.ts::caller", Kind: graph.KindFunction, Name: "caller", FilePath: "a.ts", Language: "typescript"})
	g.AddNode(&graph.Node{
		ID: "b.ts::tgt", Kind: graph.KindFunction, Name: "tgt",
		FilePath: "b.ts", StartLine: 1, EndLine: 3, Language: "typescript",
	})

	callEdge := &graph.Edge{
		From: "a.ts::caller", To: "unresolved::tgt",
		Kind: graph.EdgeCalls, FilePath: "a.ts", Line: 1,
	}
	g.AddEdge(callEdge)

	r := New(g)
	// no helper installed
	stats := r.ResolveAll()
	assert.Equal(t, 1, stats.Resolved)
	assert.Equal(t, "b.ts::tgt", callEdge.To)
}

// TestLSPHotPath_IdentifierFromTarget — covers the prefix stripping
// for the target shapes the resolver dispatches on.
func TestIdentifierFromTarget(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"foo", "foo"},
		{"*.handle", "handle"},
		{"extern::pkg/sub::Symbol", "Symbol"},
		{"extern::pkg::A::B", "B"},
		{"import::pkg/foo", ""},
		{"pyrel::foo", ""},
		{"grpc::Svc::Method", ""},
	}
	for _, c := range cases {
		got := identifierFromTarget(c.in)
		assert.Equalf(t, c.want, got, "input=%q", c.in)
	}
}

// TestLSPKindAcceptableFor covers the kind-gate rules.
func TestLSPKindAcceptableFor(t *testing.T) {
	cases := []struct {
		ek   graph.EdgeKind
		nk   graph.NodeKind
		want bool
	}{
		{graph.EdgeCalls, graph.KindFunction, true},
		{graph.EdgeCalls, graph.KindMethod, true},
		{graph.EdgeCalls, graph.KindFile, false},
		{graph.EdgeCalls, graph.KindImport, false},
		{graph.EdgeExtends, graph.KindType, true},
		{graph.EdgeExtends, graph.KindFunction, false},
		{graph.EdgeImplements, graph.KindInterface, true},
		{graph.EdgeImplements, graph.KindMethod, false},
		{graph.EdgeReads, graph.KindField, true},
		{graph.EdgeReads, graph.KindVariable, true},
		{graph.EdgeReads, graph.KindFile, false},
		{graph.EdgeReferences, graph.KindType, true},
		{graph.EdgeReferences, graph.KindFile, false},
	}
	for _, c := range cases {
		got := lspKindAcceptableFor(c.ek, c.nk)
		assert.Equalf(t, c.want, got, "edge=%s node=%s", c.ek, c.nk)
	}
}

// TestLSPHotPath_LSPIndexCaching — multiple edges resolving via LSP
// to the same definition should hit the lspIndex cache, not rescan
// the file each time.
func TestLSPHotPath_LSPIndexCaching(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "src/caller.ts", Kind: graph.KindFile, Name: "caller.ts", FilePath: "src/caller.ts", Language: "typescript"})
	g.AddNode(&graph.Node{ID: "src/svc.ts", Kind: graph.KindFile, Name: "svc.ts", FilePath: "src/svc.ts", Language: "typescript"})
	g.AddNode(&graph.Node{
		ID: "src/svc.ts::theTarget", Kind: graph.KindFunction, Name: "theTarget",
		FilePath: "src/svc.ts", StartLine: 4, EndLine: 6, Language: "typescript",
	})
	g.AddNode(&graph.Node{ID: "src/caller.ts::callerA", Kind: graph.KindFunction, Name: "callerA", FilePath: "src/caller.ts", Language: "typescript"})
	g.AddNode(&graph.Node{ID: "src/caller.ts::callerB", Kind: graph.KindFunction, Name: "callerB", FilePath: "src/caller.ts", Language: "typescript"})

	e1 := &graph.Edge{From: "src/caller.ts::callerA", To: "unresolved::theTarget", Kind: graph.EdgeCalls, FilePath: "src/caller.ts", Line: 3}
	e2 := &graph.Edge{From: "src/caller.ts::callerB", To: "unresolved::theTarget", Kind: graph.EdgeCalls, FilePath: "src/caller.ts", Line: 7}
	g.AddEdge(e1)
	g.AddEdge(e2)

	helper := &fakeLSPHelper{
		exts: []string{".ts"},
		defs: map[lspKey]lspAnswer{
			{path: "src/caller.ts", line: 3, name: "theTarget"}: {defPath: "src/svc.ts", defLine: 4},
			{path: "src/caller.ts", line: 7, name: "theTarget"}: {defPath: "src/svc.ts", defLine: 4},
		},
	}

	r := New(g)
	r.SetLSPHelper(helper)
	stats := r.ResolveFile("src/caller.ts")

	require.Equal(t, 2, stats.Resolved)
	assert.Equal(t, "src/svc.ts::theTarget", e1.To)
	assert.Equal(t, "src/svc.ts::theTarget", e2.To)
	assert.Equal(t, graph.OriginLSPResolved, e1.Origin)
	assert.Equal(t, graph.OriginLSPResolved, e2.Origin)
}

// TestLSPHotPath_NoOpAfterFileMiss — when LSP returns a path that
// doesn't exist in the graph, the bind should fall through. This
// protects against off-by-one path mismatches.
func TestLSPHotPath_NoOpAfterFileMiss(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "src/caller.ts", Kind: graph.KindFile, Name: "caller.ts", FilePath: "src/caller.ts", Language: "typescript"})
	g.AddNode(&graph.Node{ID: "src/caller.ts::callIt", Kind: graph.KindFunction, Name: "callIt", FilePath: "src/caller.ts", Language: "typescript"})

	callEdge := &graph.Edge{
		From: "src/caller.ts::callIt", To: "unresolved::ghost",
		Kind: graph.EdgeCalls, FilePath: "src/caller.ts", Line: 2,
	}
	g.AddEdge(callEdge)

	helper := &fakeLSPHelper{
		exts: []string{".ts"},
		defs: map[lspKey]lspAnswer{
			{path: "src/caller.ts", line: 2, name: "ghost"}: {defPath: "nonexistent/file.ts", defLine: 1},
		},
	}

	r := New(g)
	r.SetLSPHelper(helper)
	stats := r.ResolveAll()

	// LSP miss path triggered (no graph node at that file) — heuristic
	// has nothing to find either, so the edge is left unresolved.
	assert.Equal(t, 0, stats.Resolved)
	assert.Equal(t, 1, stats.Unresolved)
}

// TestLSPHotPath_BulkDefersLSP_ResolvesUnresolved — the deferral contract on
// the whole-graph ResolveAll (bulk) path. The heuristic cascade cannot bind
// the call (there is no graph node named "doThing"), so the edge is collected
// and bound in the post-loop deferred LSP batch instead of an inline round-
// trip inside the parallel workers. resolveEdge never consults the helper in
// bulk mode, so the single recorded call can only have come from the deferred
// batch, and maxInFlight==1 confirms the batch ran the call serially, off the
// parallel worker barrier.
func TestLSPHotPath_BulkDefersLSP_ResolvesUnresolved(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "src/caller.ts", Kind: graph.KindFile, Name: "caller.ts", FilePath: "src/caller.ts", Language: "typescript"})
	g.AddNode(&graph.Node{ID: "src/svc.ts", Kind: graph.KindFile, Name: "svc.ts", FilePath: "src/svc.ts", Language: "typescript"})
	// The definition the LSP reports lives under a different name than the
	// call site uses (a rename / barrel re-export), so the name-only heuristic
	// finds nothing to bind — only the LSP location lookup can resolve it.
	g.AddNode(&graph.Node{
		ID: "src/svc.ts::renamedTarget", Kind: graph.KindFunction, Name: "renamedTarget",
		FilePath: "src/svc.ts", StartLine: 5, EndLine: 7, Language: "typescript",
	})
	g.AddNode(&graph.Node{ID: "src/caller.ts::callIt", Kind: graph.KindFunction, Name: "callIt", FilePath: "src/caller.ts", Language: "typescript"})

	callEdge := &graph.Edge{
		From: "src/caller.ts::callIt", To: "unresolved::doThing",
		Kind: graph.EdgeCalls, FilePath: "src/caller.ts", Line: 3,
	}
	g.AddEdge(callEdge)

	helper := &fakeLSPHelper{
		exts: []string{".ts"},
		defs: map[lspKey]lspAnswer{
			{path: "src/caller.ts", line: 3, name: "doThing"}: {defPath: "src/svc.ts", defLine: 5},
		},
	}

	r := New(g)
	r.SetLSPHelper(helper)
	stats := r.ResolveAll()

	require.Equal(t, 1, stats.Resolved)
	assert.Equal(t, "src/svc.ts::renamedTarget", callEdge.To, "deferred LSP batch must bind the heuristic-unresolved edge")
	assert.Equal(t, graph.OriginLSPResolved, callEdge.Origin)
	require.NotNil(t, callEdge.Meta)
	assert.Equal(t, "lsp", callEdge.Meta["resolved_by"])
	assert.Equal(t, 1, helper.calls, "helper is consulted exactly once, in the deferred batch")
	assert.Equal(t, 1, helper.maxInFlight, "deferred LSP calls run serially, off the parallel worker barrier")
}

// TestLSPHotPath_BulkDefersLSPEvenWhenHeuristicResolves — the other half of
// the deferral contract: the helper is never consulted INLINE during the bulk
// chunk loop, but a heuristic-resolved LSP-eligible edge is still collected
// for the post-loop deferred batch so LSP keeps its override authority. Here
// the heuristic binds the sole same-dir candidate and LSP confirms the same
// node; the edge ends LSP-stamped and the single helper call ran serially in
// the deferred batch (maxInFlight==1), off the parallel worker barrier —
// resolveEdge never touches the helper in bulk mode.
func TestLSPHotPath_BulkDefersLSPEvenWhenHeuristicResolves(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "src/caller.ts", Kind: graph.KindFile, Name: "caller.ts", FilePath: "src/caller.ts", Language: "typescript"})
	g.AddNode(&graph.Node{ID: "src/svc.ts", Kind: graph.KindFile, Name: "svc.ts", FilePath: "src/svc.ts", Language: "typescript"})
	g.AddNode(&graph.Node{
		ID: "src/svc.ts::realFn", Kind: graph.KindFunction, Name: "realFn",
		FilePath: "src/svc.ts", StartLine: 4, EndLine: 6, Language: "typescript",
	})
	g.AddNode(&graph.Node{ID: "src/caller.ts::callIt", Kind: graph.KindFunction, Name: "callIt", FilePath: "src/caller.ts", Language: "typescript"})

	callEdge := &graph.Edge{
		From: "src/caller.ts::callIt", To: "unresolved::realFn",
		Kind: graph.EdgeCalls, FilePath: "src/caller.ts", Line: 3,
	}
	g.AddEdge(callEdge)

	// The heuristic resolves the call itself (sole same-dir candidate), and the
	// helper confirms the same definition. The deferred batch must still run so
	// LSP retains override authority — here it upgrades the edge's provenance.
	helper := &fakeLSPHelper{
		exts: []string{".ts"},
		defs: map[lspKey]lspAnswer{
			{path: "src/caller.ts", line: 3, name: "realFn"}: {defPath: "src/svc.ts", defLine: 4},
		},
	}

	r := New(g)
	r.SetLSPHelper(helper)
	stats := r.ResolveAll()

	require.Equal(t, 1, stats.Resolved, "the confirmed edge stays counted once, not double-counted by the deferred batch")
	assert.Equal(t, "src/svc.ts::realFn", callEdge.To)
	assert.Equal(t, graph.OriginLSPResolved, callEdge.Origin, "the deferred batch confirms the bind and stamps LSP provenance")
	require.NotNil(t, callEdge.Meta)
	assert.Equal(t, "lsp", callEdge.Meta["resolved_by"])
	assert.Equal(t, 1, helper.calls, "helper consulted exactly once, in the deferred batch")
	assert.Equal(t, 1, helper.maxInFlight, "deferred LSP calls run serially, off the parallel worker barrier")
}

// TestLSPHotPath_BulkLSPOverridesHeuristicMisbind — the regression that closes
// the bulk-mode LSP-override gap. The heuristic CONFIDENTLY binds a bare call
// to a same-directory sibling that shadows the name (src/neighbor.ts::foo at
// ast_resolved) because the import bringing the real symbol into scope can't be
// expanded. LSP knows the real definition lives cross-directory in
// lib/real.ts. In bulk mode the edge must still be deferred despite the
// confident heuristic bind, and the deferred batch must OVERRIDE it to the
// LSP-reported definition — matching the LSP-first single-file path. Before the
// fix, a heuristic-resolved edge was never deferred, so the mis-bind persisted
// across every restart and get_callers/find_usages attributed the call to the
// wrong function.
func TestLSPHotPath_BulkLSPOverridesHeuristicMisbind(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "src/caller.ts", Kind: graph.KindFile, Name: "caller.ts", FilePath: "src/caller.ts", Language: "typescript"})
	g.AddNode(&graph.Node{ID: "src/neighbor.ts", Kind: graph.KindFile, Name: "neighbor.ts", FilePath: "src/neighbor.ts", Language: "typescript"})
	g.AddNode(&graph.Node{ID: "lib/real.ts", Kind: graph.KindFile, Name: "real.ts", FilePath: "lib/real.ts", Language: "typescript"})

	g.AddNode(&graph.Node{ID: "src/caller.ts::callIt", Kind: graph.KindFunction, Name: "callIt", FilePath: "src/caller.ts", Language: "typescript"})
	// Same-directory shadow the heuristic will confidently (wrongly) pick.
	g.AddNode(&graph.Node{
		ID: "src/neighbor.ts::foo", Kind: graph.KindFunction, Name: "foo",
		FilePath: "src/neighbor.ts", StartLine: 2, EndLine: 4, Language: "typescript",
	})
	// The real definition, cross-directory — only LSP resolves to it.
	g.AddNode(&graph.Node{
		ID: "lib/real.ts::foo", Kind: graph.KindFunction, Name: "foo",
		FilePath: "lib/real.ts", StartLine: 7, EndLine: 9, Language: "typescript",
	})

	callEdge := &graph.Edge{
		From: "src/caller.ts::callIt", To: "unresolved::foo",
		Kind: graph.EdgeCalls, FilePath: "src/caller.ts", Line: 4,
	}
	g.AddEdge(callEdge)

	helper := &fakeLSPHelper{
		exts: []string{".ts"},
		defs: map[lspKey]lspAnswer{
			{path: "src/caller.ts", line: 4, name: "foo"}: {defPath: "lib/real.ts", defLine: 7},
		},
	}

	r := New(g)
	r.SetLSPHelper(helper)
	stats := r.ResolveAll()

	require.Equal(t, 1, stats.Resolved)
	assert.Equal(t, "lib/real.ts::foo", callEdge.To,
		"deferred LSP batch must override the heuristic's same-dir mis-bind")
	assert.Equal(t, graph.OriginLSPResolved, callEdge.Origin)
	require.NotNil(t, callEdge.Meta)
	assert.Equal(t, "lsp", callEdge.Meta["resolved_by"])
	assert.Equal(t, 1, helper.calls, "helper consulted once, in the deferred batch")
}

// TestLSPHotPath_BulkDeferRespectsKindGate — a deferred edge whose LSP target
// is an unacceptable kind (a file node for a calls edge) is rejected by the
// same kind-gate the inline path applies, and left unresolved rather than
// mis-bound.
func TestLSPHotPath_BulkDeferRespectsKindGate(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "src/caller.ts", Kind: graph.KindFile, Name: "caller.ts", FilePath: "src/caller.ts", Language: "typescript", StartLine: 1})
	g.AddNode(&graph.Node{ID: "src/util.ts", Kind: graph.KindFile, Name: "util.ts", FilePath: "src/util.ts", Language: "typescript", StartLine: 1})
	g.AddNode(&graph.Node{ID: "src/caller.ts::callIt", Kind: graph.KindFunction, Name: "callIt", FilePath: "src/caller.ts", Language: "typescript"})

	callEdge := &graph.Edge{
		From: "src/caller.ts::callIt", To: "unresolved::missing",
		Kind: graph.EdgeCalls, FilePath: "src/caller.ts", Line: 2,
	}
	g.AddEdge(callEdge)

	// LSP points at the util.ts FILE node (line 1) — the kind-gate must reject
	// binding a calls edge to a file.
	helper := &fakeLSPHelper{
		exts: []string{".ts"},
		defs: map[lspKey]lspAnswer{
			{path: "src/caller.ts", line: 2, name: "missing"}: {defPath: "src/util.ts", defLine: 1},
		},
	}

	r := New(g)
	r.SetLSPHelper(helper)
	stats := r.ResolveAll()

	assert.Equal(t, 0, stats.Resolved)
	assert.Equal(t, 1, helper.calls, "deferred batch attempted the bind")
	assert.True(t, graph.IsUnresolvedTarget(callEdge.To), "kind-gated LSP answer must leave the edge unresolved")
}
