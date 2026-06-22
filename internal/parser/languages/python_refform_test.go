package languages

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// refEdge finds the first edge with the given kind whose To matches target
// and (when useKind != "") whose Meta["use_kind"] matches.
func findRefEdge(edges []*graph.Edge, kind graph.EdgeKind, target, useKind string) *graph.Edge {
	for _, e := range edges {
		if e.Kind != kind || e.To != target {
			continue
		}
		if useKind != "" {
			if uk, _ := e.Meta["use_kind"].(string); uk != useKind {
				continue
			}
		}
		return e
	}
	return nil
}

func hasEdgeTo(edges []*graph.Edge, target string) bool {
	for _, e := range edges {
		if e.To == target {
			return true
		}
	}
	return false
}

// TestPyRefForm_Instantiation: `HttpResponse(...)` emits an instantiates
// edge to the class, stamped OriginASTResolved with use_kind=instantiate.
func TestPyRefForm_Instantiation(t *testing.T) {
	src := `from django.http import HttpResponse

def view(request):
    return HttpResponse("ok")
`
	_, edges := runPyExtract(t, "app/views.py", src)
	e := findRefEdge(edges, graph.EdgeInstantiates, "unresolved::HttpResponse", "instantiate")
	if e == nil {
		t.Fatalf("expected EdgeInstantiates -> unresolved::HttpResponse (use_kind=instantiate); edges=%v", edgeDump(edges))
	}
	if e.Origin != graph.OriginASTResolved {
		t.Errorf("instantiate edge Origin = %q, want OriginASTResolved", e.Origin)
	}
	if e.From != "app/views.py::view" {
		t.Errorf("instantiate edge From = %q, want app/views.py::view", e.From)
	}
}

// TestPyRefForm_DottedInstantiation: `http.HttpResponse(...)` — the
// Capitalized leaf attribute is the constructed type.
func TestPyRefForm_DottedInstantiation(t *testing.T) {
	src := `import django.http as http

def view(request):
    return http.HttpResponse("ok")
`
	_, edges := runPyExtract(t, "app/v.py", src)
	if findRefEdge(edges, graph.EdgeInstantiates, "unresolved::HttpResponse", "instantiate") == nil {
		t.Fatalf("expected instantiate edge for http.HttpResponse(...); edges=%v", edgeDump(edges))
	}
}

// TestPyRefForm_Inheritance: `class V(HttpResponse):` emits a references
// edge (use_kind=inherit), OriginASTResolved.
func TestPyRefForm_Inheritance(t *testing.T) {
	src := `from django.http import HttpResponse

class MyResponse(HttpResponse):
    pass
`
	_, edges := runPyExtract(t, "app/r.py", src)
	e := findRefEdge(edges, graph.EdgeReferences, "unresolved::HttpResponse", "inherit")
	if e == nil {
		t.Fatalf("expected EdgeReferences -> unresolved::HttpResponse (use_kind=inherit); edges=%v", edgeDump(edges))
	}
	if e.Origin != graph.OriginASTResolved {
		t.Errorf("inherit edge Origin = %q, want OriginASTResolved (else cross_pkg_guard reverts it)", e.Origin)
	}
}

// TestPyRefForm_DottedInheritance: `class V(http.HttpResponse):`.
func TestPyRefForm_DottedInheritance(t *testing.T) {
	src := `import django.http as http

class MyResponse(http.HttpResponse):
    pass
`
	_, edges := runPyExtract(t, "app/r2.py", src)
	if findRefEdge(edges, graph.EdgeReferences, "unresolved::HttpResponse", "inherit") == nil {
		t.Fatalf("expected inherit edge for http.HttpResponse base; edges=%v", edgeDump(edges))
	}
}

// TestPyRefForm_IsInstance: `isinstance(x, HttpResponse)` references the
// 2nd argument as a type (use_kind=cast). Tuple second arg references each.
func TestPyRefForm_IsInstance(t *testing.T) {
	src := `from django.http import HttpResponse, JsonResponse

def check(x):
    if isinstance(x, HttpResponse):
        return True
    return issubclass(type(x), (HttpResponse, JsonResponse))
`
	_, edges := runPyExtract(t, "app/c.py", src)
	e := findRefEdge(edges, graph.EdgeReferences, "unresolved::HttpResponse", "cast")
	if e == nil {
		t.Fatalf("expected EdgeReferences -> unresolved::HttpResponse (use_kind=cast); edges=%v", edgeDump(edges))
	}
	if e.Origin != graph.OriginASTResolved {
		t.Errorf("cast edge Origin = %q, want OriginASTResolved", e.Origin)
	}
	if findRefEdge(edges, graph.EdgeReferences, "unresolved::JsonResponse", "cast") == nil {
		t.Errorf("expected cast edge for JsonResponse in the issubclass tuple; edges=%v", edgeDump(edges))
	}
	// The first arg of isinstance (`x`) must NOT produce a type ref.
	if hasEdgeTo(edges, "unresolved::x") {
		t.Errorf("first isinstance arg `x` must not emit a type reference")
	}
}

// TestPyRefForm_StaticAccess: `HttpResponse.status_code` emits a
// references edge (use_kind=static_access) to the class.
func TestPyRefForm_StaticAccess(t *testing.T) {
	src := `from django.http import HttpResponse

def f():
    return HttpResponse.status_code
`
	_, edges := runPyExtract(t, "app/s.py", src)
	e := findRefEdge(edges, graph.EdgeReferences, "unresolved::HttpResponse", "static_access")
	if e == nil {
		t.Fatalf("expected EdgeReferences -> unresolved::HttpResponse (use_kind=static_access); edges=%v", edgeDump(edges))
	}
	if e.Origin != graph.OriginASTResolved {
		t.Errorf("static_access edge Origin = %q, want OriginASTResolved", e.Origin)
	}
}

// TestPyRefForm_Decorator: `@Validator(...)` / `@Validator` references the
// Capitalized decorator name; lowercase `@property` / `@app.route` do not.
func TestPyRefForm_Decorator(t *testing.T) {
	src := `from x import Validator

@Validator
def a():
    pass

@Validator("strict")
def b():
    pass

@property
def c(self):
    pass

@app.route("/x")
def d():
    pass
`
	_, edges := runPyExtract(t, "app/d.py", src)
	if findRefEdge(edges, graph.EdgeReferences, "unresolved::Validator", "static_access") == nil {
		t.Fatalf("expected reference edge for @Validator decorator; edges=%v", edgeDump(edges))
	}
	if hasEdgeTo(edges, "unresolved::property") {
		t.Errorf("lowercase decorator @property must not emit a type reference")
	}
	if hasEdgeTo(edges, "unresolved::route") {
		t.Errorf("lowercase decorator @app.route must not emit a type reference")
	}
}

// TestPyRefForm_NegativeCases: lowercase calls, method calls on instances,
// and bare builtins must not emit reference/instantiate edges.
func TestPyRefForm_NegativeCases(t *testing.T) {
	src := `def run(obj):
    foo()
    obj.method()
    x = dict()
    y = list()
    z = helper.compute()
    return x, y, z
`
	_, edges := runPyExtract(t, "app/n.py", src)

	for _, bad := range []string{
		"unresolved::foo", "unresolved::method", "unresolved::compute",
		"unresolved::helper",
	} {
		for _, e := range edges {
			if (e.Kind == graph.EdgeInstantiates || (e.Kind == graph.EdgeReferences && e.Meta["use_kind"] != nil)) && e.To == bad {
				t.Errorf("lowercase callee/method %q must not emit an instantiate/reference type edge (got kind=%s)", bad, e.Kind)
			}
		}
	}
	// Lowercase builtins dict()/list() must not produce instantiate edges.
	for _, b := range []string{"unresolved::dict", "unresolved::list"} {
		if findRefEdge(edges, graph.EdgeInstantiates, b, "instantiate") != nil {
			t.Errorf("lowercase builtin %q must not emit an instantiate edge", b)
		}
	}
}

// edgeDump renders edges compactly for failure messages.
func edgeDump(edges []*graph.Edge) []string {
	out := make([]string, 0, len(edges))
	for _, e := range edges {
		uk, _ := e.Meta["use_kind"].(string)
		out = append(out, string(e.Kind)+" "+e.From+" -> "+e.To+" ["+uk+"]")
	}
	return out
}
