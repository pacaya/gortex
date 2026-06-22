package languages

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// kotlinUseKindEdges returns the ref_context-tagged reference edges the Kotlin
// extractor emitted for src, keyed for assertion convenience. Each entry is
// (kind, target, ref_context).
type kotlinRefEdge struct {
	kind    graph.EdgeKind
	to      string
	useKind string
}

func extractKotlinRefEdges(t *testing.T, src string) []kotlinRefEdge {
	t.Helper()
	res, err := NewKotlinExtractor().Extract("app/Sample.kt", []byte(src))
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	var out []kotlinRefEdge
	for _, e := range res.Edges {
		uk := ""
		if e.Meta != nil {
			uk, _ = e.Meta["ref_context"].(string)
		}
		out = append(out, kotlinRefEdge{kind: e.Kind, to: e.To, useKind: uk})
	}
	return out
}

func hasRefEdge(edges []kotlinRefEdge, kind graph.EdgeKind, to, useKind string) bool {
	for _, e := range edges {
		if e.kind == kind && e.to == to && e.useKind == useKind {
			return true
		}
	}
	return false
}

// countTo counts edges whose target equals `to`, regardless of kind/ref_context.
func countTo(edges []kotlinRefEdge, to string) int {
	n := 0
	for _, e := range edges {
		if e.to == to {
			n++
		}
	}
	return n
}

// TestKotlinReferenceForm_Instantiation: a Kotlin constructor call has no
// `new` — `OkHttpClient()` is a call_expression whose callee is a Capitalized
// simple_identifier. It must emit an EdgeInstantiates to the type, NOT a flat
// EdgeCalls to `unresolved::*.OkHttpClient`.
func TestKotlinReferenceForm_Instantiation(t *testing.T) {
	edges := extractKotlinRefEdges(t, `package p
fun build() {
  val c = OkHttpClient()
}
`)
	if !hasRefEdge(edges, graph.EdgeInstantiates, "unresolved::OkHttpClient", "instantiate") {
		t.Fatalf("expected EdgeInstantiates -> unresolved::OkHttpClient (ref_context=instantiate); got %+v", edges)
	}
	// The redundant constructor "call" edge must be suppressed.
	for _, e := range edges {
		if e.kind == graph.EdgeCalls && e.to == "unresolved::*.OkHttpClient" {
			t.Fatalf("constructor call must not also emit EdgeCalls -> unresolved::*.OkHttpClient")
		}
	}
}

// TestKotlinReferenceForm_NestedInstantiation: `OkHttpClient.Builder()`
// constructs Builder (instantiate -> Builder) and statically references the
// head type OkHttpClient (static_access -> OkHttpClient).
func TestKotlinReferenceForm_NestedInstantiation(t *testing.T) {
	edges := extractKotlinRefEdges(t, `package p
fun build() {
  val b = OkHttpClient.Builder()
}
`)
	if !hasRefEdge(edges, graph.EdgeInstantiates, "unresolved::Builder", "instantiate") {
		t.Fatalf("expected EdgeInstantiates -> unresolved::Builder; got %+v", edges)
	}
	if !hasRefEdge(edges, graph.EdgeReferences, "unresolved::OkHttpClient", "static_access") {
		t.Fatalf("expected EdgeReferences -> unresolved::OkHttpClient (static_access) for the head type; got %+v", edges)
	}
}

// TestKotlinReferenceForm_Cast: `x as OkHttpClient` (as_expression) emits a
// cast reference to the type.
func TestKotlinReferenceForm_Cast(t *testing.T) {
	edges := extractKotlinRefEdges(t, `package p
fun coerce(x: Any) {
  val c = x as OkHttpClient
  val d = x as? OkHttpClient
}
`)
	if !hasRefEdge(edges, graph.EdgeReferences, "unresolved::OkHttpClient", "cast") {
		t.Fatalf("expected EdgeReferences -> unresolved::OkHttpClient (cast); got %+v", edges)
	}
}

// TestKotlinReferenceForm_TypeTest: `x is OkHttpClient` (check_expression)
// emits a reference to the tested type.
func TestKotlinReferenceForm_TypeTest(t *testing.T) {
	edges := extractKotlinRefEdges(t, `package p
fun probe(x: Any) {
  if (x is OkHttpClient) {}
  if (x !is OkHttpClient) {}
}
`)
	if !hasRefEdge(edges, graph.EdgeReferences, "unresolved::OkHttpClient", "cast") {
		t.Fatalf("expected EdgeReferences -> unresolved::OkHttpClient for the is/!is test; got %+v", edges)
	}
}

// TestKotlinReferenceForm_StaticAccess: `OkHttpClient.DEFAULT` (a
// navigation_expression with a Capitalized head, not a call) references the
// head type.
func TestKotlinReferenceForm_StaticAccess(t *testing.T) {
	edges := extractKotlinRefEdges(t, `package p
fun read() {
  val v = OkHttpClient.DEFAULT
}
`)
	if !hasRefEdge(edges, graph.EdgeReferences, "unresolved::OkHttpClient", "static_access") {
		t.Fatalf("expected EdgeReferences -> unresolved::OkHttpClient (static_access); got %+v", edges)
	}
}

// TestKotlinReferenceForm_Inheritance: `class X : Bar(), Iface` emits an
// EdgeExtends to the superclass (constructor_invocation) and an EdgeReferences
// to the bare-user_type supertype/interface, both attributed to the class.
func TestKotlinReferenceForm_Inheritance(t *testing.T) {
	edges := extractKotlinRefEdges(t, `package p
class X : Bar(), Iface {
}
`)
	if !hasRefEdge(edges, graph.EdgeExtends, "unresolved::Bar", "inherit") {
		t.Fatalf("expected EdgeExtends -> unresolved::Bar (inherit); got %+v", edges)
	}
	if !hasRefEdge(edges, graph.EdgeReferences, "unresolved::Iface", "inherit") {
		t.Fatalf("expected EdgeReferences -> unresolved::Iface (inherit); got %+v", edges)
	}
}

// TestKotlinReferenceForm_NoFalsePositives: a lowercase function call, a
// lowercase receiver access, and a primitive type must emit NO ref_context edge.
func TestKotlinReferenceForm_NoFalsePositives(t *testing.T) {
	edges := extractKotlinRefEdges(t, `package p
fun run(svc: Service) {
  foo()
  svc.doThing()
  val n: Int = 3
  val s = "x"
  val list = mutableListOf(1, 2)
}
`)
	for _, e := range edges {
		if e.useKind == "" {
			continue
		}
		switch e.to {
		case "unresolved::foo", "unresolved::*.foo",
			"unresolved::doThing", "unresolved::*.doThing",
			"unresolved::Int", "unresolved::n", "unresolved::s",
			"unresolved::svc", "unresolved::list", "unresolved::mutableListOf":
			t.Fatalf("false positive: emitted ref_context edge %+v for a non-type reference", e)
		}
	}
	// Specifically: no instantiate edge for the lowercase `mutableListOf` call,
	// no static_access for the lowercase `svc` receiver, no reference for `Int`.
	if countTo(edges, "unresolved::Int") != 0 {
		t.Fatalf("primitive Int must not produce a reference edge")
	}
	if hasRefEdge(edges, graph.EdgeReferences, "unresolved::svc", "static_access") {
		t.Fatalf("lowercase receiver svc must not produce a static_access reference")
	}
}
