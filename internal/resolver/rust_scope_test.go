package resolver

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser/languages"
)

// buildRustGraph extracts each file via the real Rust extractor and
// loads the result into a fresh in-memory graph, mirroring the
// indexer's per-file ingest.
func buildRustGraph(t *testing.T, files map[string]string) graph.Store {
	t.Helper()
	g := graph.New()
	e := languages.NewRustExtractor()
	for path, src := range files {
		r, err := e.Extract(path, []byte(src))
		require.NoError(t, err, "rust extract %s", path)
		for _, n := range r.Nodes {
			g.AddNode(n)
		}
		for _, ed := range r.Edges {
			g.AddEdge(ed)
		}
	}
	return g
}

// callTargetsFromRust collects the To-end of every call edge leaving
// fromID, so a test can assert on the post-resolution shape of a
// caller's outbound calls.
func callTargetsFromRust(g graph.Store, fromID string) []string {
	var out []string
	for _, e := range g.GetOutEdges(fromID) {
		if e.Kind == graph.EdgeCalls {
			out = append(out, e.To)
		}
	}
	return out
}

// TestRustScope_ImplMethodOwner: `Foo::new()` lands on impl Foo's new.
func TestRustScope_ImplMethodOwner(t *testing.T) {
	src := `
struct Foo {}

impl Foo {
    fn new() -> Foo { Foo {} }
}

struct Bar {}

impl Bar {
    fn new() -> Bar { Bar {} }
}

fn run() {
    let _f = Foo::new();
}
`
	g := buildRustGraph(t, map[string]string{"lib.rs": src})

	landed := ResolveRustScopeCalls(g)
	require.GreaterOrEqual(t, landed, 1, "expected at least Foo::new to land")

	wantNew := "lib.rs::Foo.new"
	require.NotNil(t, g.GetNode(wantNew), "Foo.new method node should exist")

	targets := callTargetsFromRust(g, "lib.rs::run")
	require.Contains(t, targets, wantNew, "Foo::new() should resolve to impl Foo's new, got %v", targets)
	// The other type's new must NOT be picked.
	require.NotContains(t, targets, "lib.rs::Bar.new")
}

// TestRustScope_SelfReceiver: `self.bar()` inside impl Foo lands on
// Foo.bar; `Self::new()` lands on Foo.new.
func TestRustScope_SelfReceiver(t *testing.T) {
	src := `
struct Foo {}

impl Foo {
    fn new() -> Foo { Foo {} }
    fn bar(&self) {}
    fn run(&self) {
        self.bar();
        let _x = Self::new();
    }
}

struct Other {}

impl Other {
    fn bar(&self) {}
    fn new() -> Other { Other {} }
}
`
	g := buildRustGraph(t, map[string]string{"lib.rs": src})

	ResolveRustScopeCalls(g)

	targets := callTargetsFromRust(g, "lib.rs::Foo.run")
	require.Contains(t, targets, "lib.rs::Foo.bar", "self.bar() should resolve to Foo.bar, got %v", targets)
	require.Contains(t, targets, "lib.rs::Foo.new", "Self::new() should resolve to Foo.new, got %v", targets)
	require.NotContains(t, targets, "lib.rs::Other.bar")
	require.NotContains(t, targets, "lib.rs::Other.new")
}

// TestRustScope_ModulePath: `crate::util::helper()` resolves to the
// helper free function. Gortex doesn't node-model the module tree, so
// the trailing-segment binding rides at ast_inferred.
func TestRustScope_ModulePath(t *testing.T) {
	src := `
fn helper() {}

fn run() {
    crate::util::helper();
    super::helper();
    self::helper();
}
`
	g := buildRustGraph(t, map[string]string{"lib.rs": src})

	ResolveRustScopeCalls(g)

	wantHelper := "lib.rs::helper"
	require.NotNil(t, g.GetNode(wantHelper))

	targets := callTargetsFromRust(g, "lib.rs::run")
	count := 0
	for _, tgt := range targets {
		if tgt == wantHelper {
			count++
		}
	}
	require.GreaterOrEqual(t, count, 1, "crate::util::helper() should resolve to helper, got %v", targets)

	// The module-path bindings should ride at ast_inferred.
	for _, e := range g.GetOutEdges("lib.rs::run") {
		if e.Kind == graph.EdgeCalls && e.To == wantHelper {
			require.Equal(t, graph.OriginASTInferred, e.Origin, "module-path binding should be ast_inferred")
		}
	}
}

// TestRustScope_LocalShadowsImport: when a local binding (here a
// parameter) shares a name with an imported item, the local wins — the
// shadowed name is NOT resolved to the (module-path) free function.
func TestRustScope_LocalShadowsImport(t *testing.T) {
	src := `
fn helper() {}

fn run(helper: u32) {
    self::helper();
}
`
	g := buildRustGraph(t, map[string]string{"lib.rs": src})

	ResolveRustScopeCalls(g)

	// The param `helper` shadows the free function, so the module-path
	// call must stay unresolved rather than binding to lib.rs::helper.
	for _, e := range g.GetOutEdges("lib.rs::run") {
		if e.Kind != graph.EdgeCalls {
			continue
		}
		require.NotEqual(t, "lib.rs::helper", e.To,
			"shadowed self::helper() must not resolve to the free function while a local `helper` is in scope")
		require.True(t, graph.IsUnresolvedTarget(e.To),
			"shadowed call should remain unresolved, got %s", e.To)
	}
}

// TestRustScope_AmbiguousStaysUnresolved: a `Type::method` call where
// two types of the same name each have the method is ambiguous and must
// be left unresolved.
func TestRustScope_AmbiguousStaysUnresolved(t *testing.T) {
	// Two `impl Widget` blocks in two files, both defining `build`.
	// Foo::build()-style ambiguity: a single owner name `Widget` mapping
	// to two distinct method nodes (different files) → ambiguous.
	fileA := `
struct Widget {}

impl Widget {
    fn build() -> Widget { Widget {} }
}
`
	fileB := `
struct Widget {}

impl Widget {
    fn build() -> Widget { Widget {} }
}

fn run() {
    let _w = Widget::build();
}
`
	g := buildRustGraph(t, map[string]string{"a.rs": fileA, "b.rs": fileB})

	ResolveRustScopeCalls(g)

	for _, e := range g.GetOutEdges("b.rs::run") {
		if e.Kind != graph.EdgeCalls {
			continue
		}
		require.True(t, graph.IsUnresolvedTarget(e.To),
			"ambiguous Widget::build() (two same-name owners) must stay unresolved, got %s", e.To)
	}
}

// TestRustScope_ConstructorReceiverBind: a `let c = Config::default()` local
// seeds tenv[c]=Config in the extractor, so the selector call `c.tune()` binds
// to Config.tune end to end through the receiver_type path. A same-named method
// on a different type must NOT be picked — the inferred owner is exact.
func TestRustScope_ConstructorReceiverBind(t *testing.T) {
	src := `
struct Config {}

impl Config {
    fn default() -> Config { Config {} }
    fn tune(&self) {}
}

struct Other {}

impl Other {
    fn tune(&self) {}
}

fn run() {
    let c = Config::default();
    c.tune();
}
`
	g := buildRustGraph(t, map[string]string{"lib.rs": src})

	ResolveRustScopeCalls(g)

	targets := callTargetsFromRust(g, "lib.rs::run")
	require.Contains(t, targets, "lib.rs::Config.tune",
		"c.tune() where c = Config::default() should bind to Config.tune, got %v", targets)
	require.NotContains(t, targets, "lib.rs::Other.tune",
		"the same-named method on Other must not be picked")
}

// TestRustScope_Idempotent: a second run does not change the resolved
// state nor produce additional rebindings.
func TestRustScope_Idempotent(t *testing.T) {
	src := `
struct Foo {}

impl Foo {
    fn new() -> Foo { Foo {} }
    fn run(&self) {
        let _f = Foo::new();
        self.helper();
    }
    fn helper(&self) {}
}
`
	g := buildRustGraph(t, map[string]string{"lib.rs": src})

	first := ResolveRustScopeCalls(g)
	require.GreaterOrEqual(t, first, 2)

	before := callTargetsFromRust(g, "lib.rs::Foo.run")

	second := ResolveRustScopeCalls(g)
	require.Equal(t, 0, second, "second run should land no NEW edges (already resolved)")

	after := callTargetsFromRust(g, "lib.rs::Foo.run")
	require.ElementsMatch(t, before, after, "edge targets must be stable across runs")
}

// TestRustScope_NoOpOnNonRust: the pass is a no-op when there are no
// Rust nodes.
func TestRustScope_NoOpOnNonRust(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "x.go::F", Kind: graph.KindFunction, Name: "F", Language: "go"})
	require.Equal(t, 0, ResolveRustScopeCalls(g))
}

// Guard that the extractor stamps the metadata this pass depends on, so
// a future extractor change that drops it fails loudly here rather than
// silently regressing resolution.
func TestRustScope_ExtractorStampsPathMeta(t *testing.T) {
	src := `
struct Foo {}
impl Foo { fn new() -> Foo { Foo {} } }
fn run() { let _f = Foo::new(); }
`
	g := buildRustGraph(t, map[string]string{"lib.rs": src})
	e := findCallWithPathMeta(g, "lib.rs::run")
	require.NotNil(t, e, "Foo::new() call edge should carry rust_path meta")
	require.Equal(t, "Foo::new", e.Meta["rust_path"])
}

func findCallWithPathMeta(g graph.Store, fromID string) *graph.Edge {
	for _, e := range g.GetOutEdges(fromID) {
		if e.Kind != graph.EdgeCalls || e.Meta == nil {
			continue
		}
		if _, ok := e.Meta["rust_path"]; ok {
			return e
		}
	}
	return nil
}
