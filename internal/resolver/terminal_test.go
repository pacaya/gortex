package resolver

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// TestClassifyTerminal covers the four classifier shapes: an external-receiver
// method whose name matches only an external stub (stub_only), a C++ stdlib
// angle-include (stdlib_header), a bare name no repo defines (no_definition),
// and a genuinely-pending edge whose name has a real in-graph definition — that
// last one must NOT be classified terminal.
func TestClassifyTerminal(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "pkg/a.go", Kind: graph.KindFile, FilePath: "pkg/a.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "src/a.cpp", Kind: graph.KindFile, FilePath: "src/a.cpp", Language: "cpp"})
	g.AddNode(&graph.Node{ID: "pkg/a.go::Caller", Kind: graph.KindFunction, Name: "Caller", FilePath: "pkg/a.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "src/a.cpp::use", Kind: graph.KindFunction, Name: "use", FilePath: "src/a.cpp", Language: "cpp"})
	// A real Go function the genuinely-pending edge can bind to.
	g.AddNode(&graph.Node{ID: "pkg/a.go::Bar", Kind: graph.KindFunction, Name: "Bar", FilePath: "pkg/a.go", Language: "go"})
	// An external-call stub — a name that matches only a stub placeholder.
	g.AddNode(&graph.Node{ID: "external_call::database/sql::QueryRow", Kind: graph.KindFunction, Name: "QueryRow", FilePath: ""})

	r := New(g)

	cases := []struct {
		name         string
		edge         *graph.Edge
		wantReason   string
		wantTerminal bool
	}{
		{
			name: "external receiver method matching only a stub",
			edge: &graph.Edge{From: "pkg/a.go::Caller", To: "unresolved::*.QueryRow",
				Kind: graph.EdgeCalls, FilePath: "pkg/a.go", Line: 4,
				Meta: map[string]any{"receiver_type": "*sql.DB"}},
			wantReason: terminalReasonStubOnly, wantTerminal: true,
		},
		{
			name: "cpp stdlib angle-include",
			edge: &graph.Edge{From: "src/a.cpp::use", To: "unresolved::import::vector",
				Kind: graph.EdgeImports, FilePath: "src/a.cpp", Line: 1,
				Meta: map[string]any{"include_kind": "system"}},
			wantReason: terminalReasonStdlibHeader, wantTerminal: true,
		},
		{
			name: "bare name no repo defines",
			edge: &graph.Edge{From: "pkg/a.go::Caller", To: "unresolved::Ghost",
				Kind: graph.EdgeCalls, FilePath: "pkg/a.go", Line: 6},
			wantReason: terminalReasonNoDefinition, wantTerminal: true,
		},
		{
			name: "genuinely pending — real candidate exists",
			edge: &graph.Edge{From: "pkg/a.go::Caller", To: "unresolved::Bar",
				Kind: graph.EdgeCalls, FilePath: "pkg/a.go", Line: 7},
			wantReason: "", wantTerminal: false,
		},
		{
			name: "quoted (non-system) cpp include stays pending",
			edge: &graph.Edge{From: "src/a.cpp::use", To: "unresolved::import::vector",
				Kind: graph.EdgeImports, FilePath: "src/a.cpp", Line: 2,
				Meta: map[string]any{"include_kind": "quoted"}},
			wantReason: "", wantTerminal: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason, terminal := r.classifyTerminal(tc.edge)
			assert.Equal(t, tc.wantTerminal, terminal)
			assert.Equal(t, tc.wantReason, reason)
		})
	}
}

// TestFilterTerminalSkip asserts the scoped terminal-skip filter drops only the
// bare terminal edge from a foreign repo, while keeping a non-terminal edge, a
// terminal edge whose source repo is in scope, and a terminal edge whose target
// is repo-qualified to a scope repo.
func TestFilterTerminalSkip(t *testing.T) {
	scope := map[string]struct{}{"repoa": {}}

	nonTerminal := &graph.Edge{From: "repoc/x.go::C", To: "unresolved::Baz", Kind: graph.EdgeCalls}
	termForeign := &graph.Edge{From: "repoc/x.go::C", To: "unresolved::Ghost", Kind: graph.EdgeCalls,
		Meta: map[string]any{metaResolveTerminal: true}}
	termSrcInScope := &graph.Edge{From: "repoa/a.go::A", To: "unresolved::Ghost", Kind: graph.EdgeCalls,
		Meta: map[string]any{metaResolveTerminal: true}}
	termTgtInScope := &graph.Edge{From: "repoc/x.go::C", To: "repoa::unresolved::Ghost", Kind: graph.EdgeCalls,
		Meta: map[string]any{metaResolveTerminal: true}}

	kept, skipped := filterTerminalSkip(
		[]*graph.Edge{nonTerminal, termForeign, termSrcInScope, termTgtInScope}, scope)

	assert.Equal(t, 1, skipped, "only the bare foreign terminal edge must be skipped")
	assert.ElementsMatch(t, []*graph.Edge{nonTerminal, termSrcInScope, termTgtInScope}, kept)
}

// buildTerminalSkipGraph builds a two-repo graph: repo A resolves its own call,
// and repo C carries a bare unresolvable call optionally pre-stamped terminal.
func buildTerminalSkipGraph(stamped bool) (*graph.Graph, *graph.Edge, *graph.Edge) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "repoa/a.go", Kind: graph.KindFile, FilePath: "repoa/a.go", Language: "go", RepoPrefix: "repoa"})
	g.AddNode(&graph.Node{ID: "repoa/a.go::CallerA", Kind: graph.KindFunction, Name: "CallerA", FilePath: "repoa/a.go", Language: "go", RepoPrefix: "repoa"})
	g.AddNode(&graph.Node{ID: "repoa/a.go::Foo", Kind: graph.KindFunction, Name: "Foo", FilePath: "repoa/a.go", Language: "go", RepoPrefix: "repoa"})
	g.AddNode(&graph.Node{ID: "repoc/c.go", Kind: graph.KindFile, FilePath: "repoc/c.go", Language: "go", RepoPrefix: "repoc"})
	g.AddNode(&graph.Node{ID: "repoc/c.go::CallerC", Kind: graph.KindFunction, Name: "CallerC", FilePath: "repoc/c.go", Language: "go", RepoPrefix: "repoc"})

	resolvable := &graph.Edge{From: "repoa/a.go::CallerA", To: "unresolved::Foo", Kind: graph.EdgeCalls, FilePath: "repoa/a.go", Line: 5}
	terminal := &graph.Edge{From: "repoc/c.go::CallerC", To: "unresolved::Ghost", Kind: graph.EdgeCalls, FilePath: "repoc/c.go", Line: 9}
	if stamped {
		setEdgeTerminal(terminal, terminalReasonNoDefinition)
	}
	g.AddEdge(resolvable)
	g.AddEdge(terminal)
	return g, resolvable, terminal
}

// TestScopedPassSkipsTerminalEdge asserts a scoped ResolveAll drops a
// terminal-stamped foreign edge that the scope filter alone would keep (it is a
// bare unqualified name). The control run without the stamp keeps it, and the
// GORTEX_WARMUP_FULL_RESOLVE override re-examines it despite the stamp.
func TestScopedPassSkipsTerminalEdge(t *testing.T) {
	scope := map[string]struct{}{"repoa": {}}

	// Stamped: the terminal edge is skipped, so only repo A's call remains.
	g, resolvable, _ := buildTerminalSkipGraph(true)
	r := New(g)
	r.SetScope(scope)
	stats := r.ResolveAll()
	assert.Equal(t, 2, stats.PendingBefore, "both edges are unresolved before filtering")
	assert.Equal(t, 1, stats.PendingAfter, "the terminal foreign edge must be skipped")
	assert.Equal(t, "repoa/a.go::Foo", resolvable.To, "repo A's own call still resolves")

	// Control: without the stamp the scope filter keeps the bare foreign edge,
	// proving the drop above came from the terminal skip, not the scope filter.
	gc, _, _ := buildTerminalSkipGraph(false)
	rc := New(gc)
	rc.SetScope(scope)
	statsControl := rc.ResolveAll()
	assert.Equal(t, 2, statsControl.PendingAfter, "unstamped bare foreign edge is kept by the scope filter")

	// Override: GORTEX_WARMUP_FULL_RESOLVE=1 forces re-examination of the stamp.
	t.Setenv("GORTEX_WARMUP_FULL_RESOLVE", "1")
	go2, _, _ := buildTerminalSkipGraph(true)
	r2 := New(go2)
	r2.SetScope(scope)
	statsOverride := r2.ResolveAll()
	assert.Equal(t, 2, statsOverride.PendingAfter, "the override must re-examine the terminal edge")
}

// TestScopedPassKeepsTerminalEdgeInScope asserts a terminal edge whose SOURCE
// repo is in scope is re-examined, not skipped.
func TestScopedPassKeepsTerminalEdgeInScope(t *testing.T) {
	// The terminal edge lives in repoc; put repoc in scope.
	g, _, _ := buildTerminalSkipGraph(true)
	r := New(g)
	r.SetScope(map[string]struct{}{"repoc": {}})
	stats := r.ResolveAll()
	assert.Equal(t, 2, stats.PendingAfter,
		"a terminal edge whose source repo is in scope must not be skipped")
}

// TestFullPassStampsTerminalEdge asserts a FULL master pass durably stamps a
// bare unresolvable edge and that a pass without SetStampTerminal never stamps.
func TestFullPassStampsTerminalEdge(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "pkg/a.go", Kind: graph.KindFile, FilePath: "pkg/a.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "pkg/a.go::Caller", Kind: graph.KindFunction, Name: "Caller", FilePath: "pkg/a.go", Language: "go"})
	e := &graph.Edge{From: "pkg/a.go::Caller", To: "unresolved::Ghost", Kind: graph.EdgeCalls, FilePath: "pkg/a.go", Line: 5}
	g.AddEdge(e)

	r := New(g)
	r.SetStampTerminal(true)
	r.ResolveAll()

	require.True(t, edgeTerminalFlag(e), "full master pass must stamp the terminal edge")
	assert.Equal(t, terminalReasonNoDefinition, e.Meta[metaResolveTerminalReason])

	// A pass without stamping enabled (per-repo / single-file) must not stamp.
	g2 := graph.New()
	g2.AddNode(&graph.Node{ID: "pkg/a.go", Kind: graph.KindFile, FilePath: "pkg/a.go", Language: "go"})
	g2.AddNode(&graph.Node{ID: "pkg/a.go::Caller", Kind: graph.KindFunction, Name: "Caller", FilePath: "pkg/a.go", Language: "go"})
	e2 := &graph.Edge{From: "pkg/a.go::Caller", To: "unresolved::Ghost", Kind: graph.EdgeCalls, FilePath: "pkg/a.go", Line: 5}
	g2.AddEdge(e2)
	New(g2).ResolveAll()
	assert.False(t, edgeTerminalFlag(e2), "a pass without SetStampTerminal must not stamp")
}

// TestFullPassUnStampsResolvedEdge asserts the self-healing path: an edge that
// was stamped terminal now resolves (a matching symbol appeared), and the full
// pass drops the stale flag.
func TestFullPassUnStampsResolvedEdge(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "pkg/a.go", Kind: graph.KindFile, FilePath: "pkg/a.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "pkg/a.go::Caller", Kind: graph.KindFunction, Name: "Caller", FilePath: "pkg/a.go", Language: "go"})
	// The matching symbol now exists in the caller's own file, so the call binds.
	g.AddNode(&graph.Node{ID: "pkg/a.go::Foo", Kind: graph.KindFunction, Name: "Foo", FilePath: "pkg/a.go", Language: "go"})
	e := &graph.Edge{From: "pkg/a.go::Caller", To: "unresolved::Foo", Kind: graph.EdgeCalls, FilePath: "pkg/a.go", Line: 5}
	setEdgeTerminal(e, terminalReasonNoDefinition) // stale stamp from a prior pass
	g.AddEdge(e)

	r := New(g)
	r.SetStampTerminal(true)
	stats := r.ResolveAll()

	require.Equal(t, 1, stats.Resolved, "the edge must now resolve")
	assert.Equal(t, "pkg/a.go::Foo", e.To)
	assert.False(t, edgeTerminalFlag(e), "a now-resolved edge must shed its terminal flag")
}

// TestFullPassUnStampsRegainedCandidate asserts the reconcile un-stamps an edge
// that stays unresolved but has regained a real candidate — an extends edge
// stamped terminal, now with a same-name function candidate the type-position
// resolver won't bind. It stays unresolved yet is no longer terminal.
func TestFullPassUnStampsRegainedCandidate(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "pkg/a.go", Kind: graph.KindFile, FilePath: "pkg/a.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "pkg/a.go::Child", Kind: graph.KindType, Name: "Child", FilePath: "pkg/a.go", Language: "go"})
	// A same-name FUNCTION — a definition candidate, but not a type, so the
	// extends edge cannot bind to it.
	g.AddNode(&graph.Node{ID: "pkg/a.go::Widget", Kind: graph.KindFunction, Name: "Widget", FilePath: "pkg/a.go", Language: "go"})
	e := &graph.Edge{From: "pkg/a.go::Child", To: "unresolved::Widget", Kind: graph.EdgeExtends, FilePath: "pkg/a.go", Line: 3}
	setEdgeTerminal(e, terminalReasonNoDefinition)
	g.AddEdge(e)

	r := New(g)
	r.SetStampTerminal(true)
	r.ResolveAll()

	assert.True(t, graph.IsUnresolvedTarget(e.To), "the extends edge must stay unresolved")
	assert.False(t, edgeTerminalFlag(e), "an edge that regained a candidate must be un-stamped")
}

// TestReconcileTerminalStampsBulk proves the sqlite-backend bulk
// classification path (reconcileTerminalStampsBulk, taken because
// *store_sqlite.Store implements graph.NodeNameClassCounter) makes the
// IDENTICAL decisions as the in-memory per-edge classifyTerminal path for
// the same four shapes TestClassifyTerminal covers, plus the "stays
// pending" quoted-include case — the stage-4 diff-validation for the SQL
// port: same fixtures, same expected (reason, terminal) pairs, different
// backend and classification path. Calls reconcileTerminalStamps directly
// (not the full ResolveAll pipeline) so an earlier compute-loop pass can
// never resolve a fixture edge out from under the case it's meant to test.
func TestReconcileTerminalStampsBulk(t *testing.T) {
	s, err := store_sqlite.Open(filepath.Join(t.TempDir(), "bulk_term.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	s.AddNode(&graph.Node{ID: "pkg/a.go", Kind: graph.KindFile, FilePath: "pkg/a.go", Language: "go"})
	s.AddNode(&graph.Node{ID: "src/a.cpp", Kind: graph.KindFile, FilePath: "src/a.cpp", Language: "cpp"})
	s.AddNode(&graph.Node{ID: "pkg/a.go::Caller", Kind: graph.KindFunction, Name: "Caller", FilePath: "pkg/a.go", Language: "go"})
	s.AddNode(&graph.Node{ID: "src/a.cpp::use", Kind: graph.KindFunction, Name: "use", FilePath: "src/a.cpp", Language: "cpp"})
	s.AddNode(&graph.Node{ID: "pkg/a.go::Bar", Kind: graph.KindFunction, Name: "Bar", FilePath: "pkg/a.go", Language: "go"})
	s.AddNode(&graph.Node{ID: "external_call::database/sql::QueryRow", Kind: graph.KindFunction, Name: "QueryRow", FilePath: ""})

	stubOnly := &graph.Edge{From: "pkg/a.go::Caller", To: "unresolved::*.QueryRow",
		Kind: graph.EdgeCalls, FilePath: "pkg/a.go", Line: 4,
		Meta: map[string]any{"receiver_type": "*sql.DB"}}
	stdlibHeader := &graph.Edge{From: "src/a.cpp::use", To: "unresolved::import::vector",
		Kind: graph.EdgeImports, FilePath: "src/a.cpp", Line: 1,
		Meta: map[string]any{"include_kind": "system"}}
	noDefinition := &graph.Edge{From: "pkg/a.go::Caller", To: "unresolved::Ghost",
		Kind: graph.EdgeCalls, FilePath: "pkg/a.go", Line: 6}
	genuinelyPending := &graph.Edge{From: "pkg/a.go::Caller", To: "unresolved::Bar",
		Kind: graph.EdgeCalls, FilePath: "pkg/a.go", Line: 7}
	quotedInclude := &graph.Edge{From: "src/a.cpp::use", To: "unresolved::import::vector",
		Kind: graph.EdgeImports, FilePath: "src/a.cpp", Line: 2,
		Meta: map[string]any{"include_kind": "quoted"}}

	for _, e := range []*graph.Edge{stubOnly, stdlibHeader, noDefinition, genuinelyPending, quotedInclude} {
		s.AddEdge(e)
	}

	r := New(s)
	r.SetStampTerminal(true)
	stamped, unstamped := r.reconcileTerminalStamps()

	assert.Equal(t, 3, stamped, "stubOnly, stdlibHeader, noDefinition are the only terminal shapes")
	assert.Equal(t, 0, unstamped)

	// The sqlite backend returns detached row copies (unlike the in-memory
	// backend's live *Edge pointers), so the stamps just applied above live
	// on fresh objects reconcileTerminalStamps fetched internally — re-fetch
	// each edge from the store to observe them, exactly like
	// TestTerminalStampSQLitePersistence does.
	findEdge := func(from, to string, line int) *graph.Edge {
		for _, e := range s.GetOutEdges(from) {
			if e.To == to && e.Line == line {
				return e
			}
		}
		return nil
	}

	cases := []struct {
		name         string
		from, to     string
		line         int
		wantReason   string
		wantTerminal bool
	}{
		{"external receiver method matching only a stub", stubOnly.From, stubOnly.To, stubOnly.Line, terminalReasonStubOnly, true},
		{"cpp stdlib angle-include", stdlibHeader.From, stdlibHeader.To, stdlibHeader.Line, terminalReasonStdlibHeader, true},
		{"bare name no repo defines", noDefinition.From, noDefinition.To, noDefinition.Line, terminalReasonNoDefinition, true},
		{"genuinely pending — real candidate exists", genuinelyPending.From, genuinelyPending.To, genuinelyPending.Line, "", false},
		{"quoted (non-system) cpp include stays pending", quotedInclude.From, quotedInclude.To, quotedInclude.Line, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := findEdge(tc.from, tc.to, tc.line)
			require.NotNil(t, e, "edge must still be present in the store")
			assert.Equal(t, tc.wantTerminal, edgeTerminalFlag(e))
			reason, _ := e.Meta[metaResolveTerminalReason].(string)
			assert.Equal(t, tc.wantReason, reason)
		})
	}
}

// TestTerminalStampSQLitePersistence proves the durable flag survives a store
// reopen on the sqlite backend (the known meta-write-back bug class): stamp,
// close, reopen, flag still present.
func TestTerminalStampSQLitePersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "term.sqlite")
	s, err := store_sqlite.Open(path)
	require.NoError(t, err)

	s.AddNode(&graph.Node{ID: "pkg/a.go", Kind: graph.KindFile, FilePath: "pkg/a.go", Language: "go"})
	s.AddNode(&graph.Node{ID: "pkg/a.go::Caller", Kind: graph.KindFunction, Name: "Caller", FilePath: "pkg/a.go", Language: "go"})
	s.AddEdge(&graph.Edge{From: "pkg/a.go::Caller", To: "unresolved::Ghost", Kind: graph.EdgeCalls, FilePath: "pkg/a.go", Line: 5})

	r := New(s)
	r.SetStampTerminal(true)
	r.ResolveAll()
	require.NoError(t, s.Close())

	s2, err := store_sqlite.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s2.Close() })

	var found *graph.Edge
	for _, e := range s2.GetOutEdges("pkg/a.go::Caller") {
		if e.To == "unresolved::Ghost" {
			found = e
		}
	}
	require.NotNil(t, found, "the stamped edge must survive a store reopen")
	require.NotNil(t, found.Meta, "reopened edge must carry meta")
	v, _ := found.Meta[metaResolveTerminal].(bool)
	assert.True(t, v, "resolve_terminal must persist across a reopen")
	assert.Equal(t, terminalReasonNoDefinition, found.Meta[metaResolveTerminalReason])
}
