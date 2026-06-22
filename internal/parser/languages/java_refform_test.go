package languages

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// refEdge finds the first edge with the given kind, target, and
// (optional) ref_context. ref_context == "" matches any (or no) ref_context.
func refEdge(edges []*graph.Edge, kind graph.EdgeKind, to, useKind string) *graph.Edge {
	for _, e := range edges {
		if e.Kind != kind || e.To != to {
			continue
		}
		if useKind == "" {
			return e
		}
		if e.Meta != nil {
			if uk, _ := e.Meta["ref_context"].(string); uk == useKind {
				return e
			}
		}
	}
	return nil
}

// hasRefTo reports whether any edge of any kind targets unresolved::<name>
// with the given ref_context — used by negative assertions.
func hasUseKindTo(edges []*graph.Edge, to, useKind string) bool {
	for _, e := range edges {
		if e.To != to || e.Meta == nil {
			continue
		}
		if uk, _ := e.Meta["ref_context"].(string); uk == useKind {
			return true
		}
	}
	return false
}

// TestJavaRefForm_Instantiation pins `new Foo()` / `new Foo[]` /
// `new Outer.Inner()` → EdgeInstantiates (ref_context=instantiate), and
// generic args of `new ArrayList<Request>()` → Request.
func TestJavaRefForm_Instantiation(t *testing.T) {
	src := `package app;
public class Factory {
	void build() {
		Foo f = new Foo();
		Foo[] arr = new Foo[3];
		Outer.Inner oi = new Outer.Inner();
		java.util.List<Request> reqs = new java.util.ArrayList<Request>();
	}
}
`
	_, edges := runJavaExtract(t, "app/Factory.java", src)

	if e := refEdge(edges, graph.EdgeInstantiates, "unresolved::Foo", "instantiate"); e == nil {
		t.Errorf("expected EdgeInstantiates -> Foo (ref_context=instantiate)")
	} else if e.Origin != graph.OriginASTInferred {
		t.Errorf("instantiate Origin = %q, want OriginASTInferred", e.Origin)
	}
	if refEdge(edges, graph.EdgeInstantiates, "unresolved::Inner", "instantiate") == nil {
		t.Errorf("expected EdgeInstantiates -> Inner for `new Outer.Inner()`")
	}
	if refEdge(edges, graph.EdgeInstantiates, "unresolved::ArrayList", "instantiate") == nil {
		t.Errorf("expected EdgeInstantiates -> ArrayList for `new java.util.ArrayList<>()`")
	}
	if refEdge(edges, graph.EdgeInstantiates, "unresolved::Request", "instantiate") == nil {
		t.Errorf("expected EdgeInstantiates -> Request (generic arg of new ArrayList<Request>())")
	}
}

// TestJavaRefForm_Inheritance pins `extends Foo` / `implements Bar, Baz`
// → EdgeReferences (ref_context=inherit), stamped OriginASTResolved so the
// cross-package guard never reverts them.
func TestJavaRefForm_Inheritance(t *testing.T) {
	src := `package app;
public class Worker extends Base implements Runnable, Closeable {
}
`
	_, edges := runJavaExtract(t, "app/Worker.java", src)

	for _, want := range []string{"Base", "Runnable", "Closeable"} {
		e := refEdge(edges, graph.EdgeReferences, "unresolved::"+want, "inherit")
		if e == nil {
			t.Errorf("expected EdgeReferences -> %s (ref_context=inherit)", want)
			continue
		}
		if e.Origin != graph.OriginASTResolved {
			t.Errorf("inherit -> %s Origin = %q, want OriginASTResolved (else cross_pkg_guard reverts it)", want, e.Origin)
		}
	}
}

// TestJavaRefForm_CastAndInstanceof pins `(Foo) x`, `x instanceof Foo`,
// and the pattern `x instanceof Foo f` → EdgeReferences (ref_context=cast),
// OriginASTResolved.
func TestJavaRefForm_CastAndInstanceof(t *testing.T) {
	src := `package app;
public class C {
	void m(Object o) {
		Foo a = (Foo) o;
		boolean b = o instanceof Bar;
		if (o instanceof Baz z) {}
	}
}
`
	_, edges := runJavaExtract(t, "app/C.java", src)

	for _, want := range []string{"Foo", "Bar", "Baz"} {
		e := refEdge(edges, graph.EdgeReferences, "unresolved::"+want, "cast")
		if e == nil {
			t.Errorf("expected EdgeReferences -> %s (ref_context=cast)", want)
			continue
		}
		if e.Origin != graph.OriginASTResolved {
			t.Errorf("cast -> %s Origin = %q, want OriginASTResolved", want, e.Origin)
		}
	}
}

// TestJavaRefForm_StaticAccess pins `Foo.CONST`, `Foo.class`, and
// `Foo.staticMethod()` whose scope is a Capitalized type → EdgeReferences
// (ref_context=static_access). A `@Foo` annotation also references Foo.
func TestJavaRefForm_StaticAccess(t *testing.T) {
	src := `package app;
@Component
public class C {
	void m() {
		int x = Constants.MAX;
		Class<?> c = Foo.class;
		String s = Helper.format();
	}
}
`
	_, edges := runJavaExtract(t, "app/C.java", src)

	for _, want := range []string{"Constants", "Foo", "Helper"} {
		if refEdge(edges, graph.EdgeReferences, "unresolved::"+want, "static_access") == nil {
			t.Errorf("expected EdgeReferences -> %s (ref_context=static_access)", want)
		}
	}
	// @Component annotation → reference to Component.
	e := refEdge(edges, graph.EdgeReferences, "unresolved::Component", "static_access")
	if e == nil {
		t.Errorf("expected EdgeReferences -> Component for @Component annotation")
	} else if e.Origin != graph.OriginASTResolved {
		t.Errorf("annotation ref Origin = %q, want OriginASTResolved", e.Origin)
	}
}

// TestJavaRefForm_NoFalsePositives pins the capitalization / shadow
// gates: a bare call `bar()`, an instance call `obj.method()`, a
// primitive (`int`), and `this.x` must emit no reference-form edge.
func TestJavaRefForm_NoFalsePositives(t *testing.T) {
	src := `package app;
public class C {
	int field;
	void m(Object obj) {
		bar();
		obj.method();
		int n = 0;
		this.field = n;
		String name = lower.value;
	}
}
`
	_, edges := runJavaExtract(t, "app/C.java", src)

	// No reference-form edge to a lowercase / primitive / this target.
	for _, e := range edges {
		if e.Meta == nil {
			continue
		}
		uk, _ := e.Meta["ref_context"].(string)
		switch uk {
		case "instantiate", "cast", "inherit", "static_access":
			to := e.To
			// Strip the unresolved:: prefix for the check.
			const p = "unresolved::"
			if len(to) > len(p) && to[:len(p)] == p {
				to = to[len(p):]
			}
			if to == "" {
				continue
			}
			c := to[0]
			if !(c >= 'A' && c <= 'Z') {
				t.Errorf("reference-form edge to non-Capitalized target %q (ref_context=%s) — capitalization gate leaked", e.To, uk)
			}
		}
	}

	// Specific negatives.
	if hasUseKindTo(edges, "unresolved::bar", "static_access") || hasUseKindTo(edges, "unresolved::bar", "cast") {
		t.Errorf("bare call bar() emitted a reference-form edge")
	}
	if hasUseKindTo(edges, "unresolved::method", "static_access") {
		t.Errorf("instance call obj.method() emitted a static_access reference")
	}
	if hasUseKindTo(edges, "unresolved::obj", "static_access") {
		t.Errorf("instance receiver obj emitted a static_access reference")
	}
	for _, prim := range []string{"int", "field", "name", "value", "lower"} {
		for _, uk := range []string{"instantiate", "cast", "inherit", "static_access"} {
			if hasUseKindTo(edges, "unresolved::"+prim, uk) {
				t.Errorf("primitive/lowercase %q emitted a %s reference-form edge", prim, uk)
			}
		}
	}
}
