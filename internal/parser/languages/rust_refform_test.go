package languages

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// refEdgeUseKind returns the ref_context Meta tag of a reference edge, or "".
func refEdgeUseKind(e *graph.Edge) string {
	if e.Meta == nil {
		return ""
	}
	if v, ok := e.Meta["ref_context"].(string); ok {
		return v
	}
	return ""
}

// hasInstantiate reports whether an EdgeInstantiates to unresolved::<typ>
// exists, and that it is stamped OriginASTResolved (so the cross-package
// guard never reverts it).
func hasInstantiate(t *testing.T, edges []*graph.Edge, typ string) bool {
	t.Helper()
	want := "unresolved::" + typ
	for _, e := range edges {
		if e.Kind == graph.EdgeInstantiates && e.To == want {
			if e.Origin != graph.OriginASTResolved {
				t.Errorf("EdgeInstantiates → %s Origin = %q; want ast_resolved", want, e.Origin)
			}
			return true
		}
	}
	return false
}

// hasRef reports whether an EdgeReferences to unresolved::<typ> with the
// given ref_context exists, stamped OriginASTResolved.
func hasRef(t *testing.T, edges []*graph.Edge, typ, useKind string) bool {
	t.Helper()
	want := "unresolved::" + typ
	for _, e := range edges {
		if e.Kind == graph.EdgeReferences && e.To == want && refEdgeUseKind(e) == useKind {
			if e.Origin != graph.OriginASTResolved {
				t.Errorf("EdgeReferences(%s) → %s Origin = %q; want ast_resolved", useKind, want, e.Origin)
			}
			return true
		}
	}
	return false
}

// TestRustRefForm_Construction covers the construction surface:
// associated-function constructors, struct-expression literals, and
// tuple-struct / enum-variant calls.
func TestRustRefForm_Construction(t *testing.T) {
	src := `fn run() {
    let a = Foo::new();
    let b = Bar { x: 1 };
    let c = Variant(1, 2);
}
`
	_, edges := runRustExtract(t, "src/lib.rs", src)

	if !hasInstantiate(t, edges, "Foo") {
		t.Errorf("Foo::new() must emit EdgeInstantiates → unresolved::Foo")
	}
	if !hasInstantiate(t, edges, "Bar") {
		t.Errorf("Bar { x: 1 } struct expression must emit EdgeInstantiates → unresolved::Bar")
	}
	if !hasInstantiate(t, edges, "Variant") {
		t.Errorf("Variant(1, 2) tuple-struct/variant call must emit EdgeInstantiates → unresolved::Variant")
	}
}

// TestRustRefForm_TraitImpls covers inheritance / trait edges: inherent
// impl, trait impl (both the trait and the type), trait bound, where
// predicate, supertrait, and dyn Trait.
func TestRustRefForm_TraitImpls(t *testing.T) {
	src := `struct S;

impl Inherent for S {}

trait Greeter: Base {
    fn hi(&self);
}

fn run<T: Bound>(x: T) -> Box<dyn Animal> where T: Where {
    todo!()
}
`
	_, edges := runRustExtract(t, "src/lib.rs", src)

	// impl Inherent for S — both the trait and the implementing type.
	if !hasRef(t, edges, "Inherent", "inherit") {
		t.Errorf("impl Inherent for S must emit inherit → Inherent")
	}
	if !hasRef(t, edges, "S", "inherit") {
		t.Errorf("impl Inherent for S must emit inherit → S (the implementing type)")
	}
	// trait Greeter: Base — supertrait.
	if !hasRef(t, edges, "Base", "inherit") {
		t.Errorf("supertrait `: Base` must emit inherit → Base")
	}
	// T: Bound — type-parameter bound.
	if !hasRef(t, edges, "Bound", "inherit") {
		t.Errorf("trait bound T: Bound must emit inherit → Bound")
	}
	// where T: Where — where-clause bound.
	if !hasRef(t, edges, "Where", "inherit") {
		t.Errorf("where T: Where must emit inherit → Where")
	}
	// Box<dyn Animal> — dynamic trait object.
	if !hasRef(t, edges, "Animal", "inherit") {
		t.Errorf("dyn Animal must emit inherit → Animal")
	}
}

// TestRustRefForm_InherentImpl checks a bare `impl Foo` (no trait) emits a
// single inherit edge to the implementing type.
func TestRustRefForm_InherentImpl(t *testing.T) {
	src := `struct Foo;
impl Foo {
    fn m(&self) {}
}
`
	_, edges := runRustExtract(t, "src/lib.rs", src)
	if !hasRef(t, edges, "Foo", "inherit") {
		t.Errorf("impl Foo must emit inherit → Foo")
	}
}

// TestRustRefForm_Cast checks `x as Foo` emits a cast reference.
func TestRustRefForm_Cast(t *testing.T) {
	src := `fn run(x: u64) {
    let y = x as Widget;
    let z = x as u32;
}
`
	_, edges := runRustExtract(t, "src/lib.rs", src)
	if !hasRef(t, edges, "Widget", "cast") {
		t.Errorf("x as Widget must emit cast → Widget")
	}
	// Primitive cast target must not emit an edge.
	for _, e := range edges {
		if e.To == "unresolved::u32" {
			t.Errorf("x as u32 (primitive) must not emit a reference edge")
		}
	}
}

// TestRustRefForm_PathAccess covers static / path access: a constant, an
// enum variant, a non-constructor associated function, and a
// module-qualified path whose trailing segment is a type.
func TestRustRefForm_PathAccess(t *testing.T) {
	src := `fn run() {
    let a = Config::CONST;
    let b = Color::Red;
    let c = Helper::compute();
    let d = std::io::Error::last();
}
`
	_, edges := runRustExtract(t, "src/lib.rs", src)

	if !hasRef(t, edges, "Config", "static_access") {
		t.Errorf("Config::CONST must emit static_access → Config")
	}
	if !hasRef(t, edges, "Color", "static_access") {
		t.Errorf("Color::Red must emit static_access → Color")
	}
	if !hasRef(t, edges, "Helper", "static_access") {
		t.Errorf("Helper::compute() must emit static_access → Helper")
	}
	// std::io::Error::last() — module-qualified, lowercase head; the
	// trailing Capitalized segment Error is the type. last() is not a
	// constructor, so this is a static_access (not instantiation).
	if !hasRef(t, edges, "Error", "static_access") {
		t.Errorf("std::io::Error::last() must emit static_access → Error")
	}
}

// TestRustRefForm_DeriveAttribute checks `#[derive(Foo, Bar)]` emits a
// static_access reference for each derive macro name.
func TestRustRefForm_DeriveAttribute(t *testing.T) {
	src := `#[derive(Serialize, Deserialize)]
struct Payload {
    id: u32,
}
`
	_, edges := runRustExtract(t, "src/lib.rs", src)
	if !hasRef(t, edges, "Serialize", "static_access") {
		t.Errorf("#[derive(Serialize, ...)] must emit static_access → Serialize")
	}
	if !hasRef(t, edges, "Deserialize", "static_access") {
		t.Errorf("#[derive(..., Deserialize)] must emit static_access → Deserialize")
	}
}

// TestRustRefForm_Negatives checks the false-positive guards: lowercase
// function calls, primitive let annotations, all-lowercase module paths,
// and `self::`/`crate::` paths emit no reference-form edges.
func TestRustRefForm_Negatives(t *testing.T) {
	src := `fn run() {
    foo();
    let x: i32 = 0;
    let y = self::helper();
    let z = crate::util::compute();
    bar::baz();
}
`
	_, edges := runRustExtract(t, "src/lib.rs", src)

	for _, e := range edges {
		if e.Kind != graph.EdgeInstantiates && e.Kind != graph.EdgeReferences {
			continue
		}
		switch e.To {
		case "unresolved::foo", "unresolved::i32", "unresolved::helper",
			"unresolved::self", "unresolved::crate", "unresolved::util",
			"unresolved::compute", "unresolved::bar", "unresolved::baz":
			t.Errorf("unexpected reference-form edge %s → %s (ref_context=%q)", e.Kind, e.To, refEdgeUseKind(e))
		}
	}
}

// TestRustRefForm_NoDoubleEmitForLetType guards against double-counting:
// a `let x: Type = Type::new()` line should NOT emit a reference-form edge
// for the let-annotation type (that's the base extractor's EdgeTypedAs
// territory) — only the construction view from the RHS.
func TestRustRefForm_NoDoubleEmitForLetType(t *testing.T) {
	src := `fn run() {
    let c: Client = Client::new();
}
`
	_, edges := runRustExtract(t, "src/lib.rs", src)

	// The let annotation `: Client` stays an EdgeTypedAs (base extractor).
	typed := edgesByKind(edges, graph.EdgeTypedAs)
	foundTyped := false
	for _, e := range typed {
		if e.To == "unresolved::Client" {
			foundTyped = true
		}
	}
	if !foundTyped {
		t.Errorf("let annotation : Client must still emit EdgeTypedAs → Client; got %v", edgeTargets(typed))
	}
	// The RHS Client::new() is the construction view.
	if !hasInstantiate(t, edges, "Client") {
		t.Errorf("Client::new() must emit EdgeInstantiates → Client")
	}
	// No `static_access Client` should leak from the callee path.
	for _, e := range edges {
		if e.Kind == graph.EdgeReferences && e.To == "unresolved::Client" && refEdgeUseKind(e) == "static_access" {
			t.Errorf("Client::new() must not emit a static_access reference (it is a construction)")
		}
	}
}
