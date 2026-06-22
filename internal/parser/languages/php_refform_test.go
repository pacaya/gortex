package languages

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// hasPHPRefEdge reports whether edges contains an edge of kind k from `from`
// to `unresolved::<to>` whose Meta["ref_context"] equals refContext ("" to
// ignore the context). Used to assert the PHP reference-form edges
// emitPHPReferenceForms produces.
func hasPHPRefEdge(edges []*graph.Edge, from, to string, k graph.EdgeKind, refContext string) bool {
	for _, e := range edges {
		if e.Kind != k || e.From != from || e.To != "unresolved::"+to {
			continue
		}
		if refContext == "" {
			return true
		}
		if rc, _ := e.Meta["ref_context"].(string); rc == refContext {
			return true
		}
	}
	return false
}

// phpRefEdgeOrigin returns the Origin of the first matching edge, or "".
func phpRefEdgeOrigin(edges []*graph.Edge, from, to string, k graph.EdgeKind, refContext string) string {
	for _, e := range edges {
		if e.Kind != k || e.From != from || e.To != "unresolved::"+to {
			continue
		}
		if refContext != "" {
			if rc, _ := e.Meta["ref_context"].(string); rc != refContext {
				continue
			}
		}
		return e.Origin
	}
	return ""
}

// TestPHPRefForm_Instantiation verifies `new Foo(...)` and a namespaced
// `new \App\Http\Client()` each emit an EdgeInstantiates from the enclosing
// method to the bare constructed type.
func TestPHPRefForm_Instantiation(t *testing.T) {
	src := []byte(`<?php
class Svc {
    public function handle() {
        $a = new RestClient();
        $b = new \App\Http\HttpClient();
    }
}
`)
	result, err := NewPHPExtractor().Extract("p/Svc.php", src)
	if err != nil {
		t.Fatal(err)
	}
	const methodID = "p/Svc.php::Svc.handle"

	for _, tgt := range []string{"RestClient", "HttpClient"} {
		if !hasPHPRefEdge(result.Edges, methodID, tgt, graph.EdgeInstantiates, "") {
			t.Errorf("expected EdgeInstantiates %s -> %s", methodID, tgt)
		}
	}
}

// TestPHPRefForm_Inheritance verifies `class X extends Base implements I, J`
// emits one EdgeReferences (ref_context "inherit") per base / interface name,
// attributed to the declaring class node.
func TestPHPRefForm_Inheritance(t *testing.T) {
	src := []byte(`<?php
class Derived extends BaseSvc implements HandlerInterface, Countable {
}
`)
	result, err := NewPHPExtractor().Extract("p/Derived.php", src)
	if err != nil {
		t.Fatal(err)
	}
	const typeID = "p/Derived.php::Derived"

	for _, tgt := range []string{"BaseSvc", "HandlerInterface", "Countable"} {
		if !hasPHPRefEdge(result.Edges, typeID, tgt, graph.EdgeReferences, graph.RefContextInherit) {
			t.Errorf("expected EdgeReferences (inherit) %s -> %s", typeID, tgt)
		}
	}
}

// TestPHPRefForm_Instanceof verifies `$x instanceof Foo` emits an
// EdgeReferences with ref_context "cast"; a namespaced test reduces to the
// bare class name.
func TestPHPRefForm_Instanceof(t *testing.T) {
	src := []byte(`<?php
class Svc {
    public function handle($x) {
        if ($x instanceof Response) {}
        if ($x instanceof \App\Models\User) {}
    }
}
`)
	result, err := NewPHPExtractor().Extract("p/Svc.php", src)
	if err != nil {
		t.Fatal(err)
	}
	const methodID = "p/Svc.php::Svc.handle"

	for _, tgt := range []string{"Response", "User"} {
		if !hasPHPRefEdge(result.Edges, methodID, tgt, graph.EdgeReferences, graph.RefContextCast) {
			t.Errorf("expected EdgeReferences (cast) %s -> %s", methodID, tgt)
		}
	}
}

// TestPHPRefForm_StaticAccess verifies `Foo::CONST`, `Foo::class`, and
// `Foo::method()` each emit an EdgeReferences with ref_context
// "static_access" to the scope type.
func TestPHPRefForm_StaticAccess(t *testing.T) {
	src := []byte(`<?php
class Svc {
    public function handle() {
        $v = Status::OK;
        $c = Middleware::class;
        $m = Logger::log();
    }
}
`)
	result, err := NewPHPExtractor().Extract("p/Svc.php", src)
	if err != nil {
		t.Fatal(err)
	}
	const methodID = "p/Svc.php::Svc.handle"

	for _, tgt := range []string{"Status", "Middleware", "Logger"} {
		if !hasPHPRefEdge(result.Edges, methodID, tgt, graph.EdgeReferences, graph.RefContextStaticAccess) {
			t.Errorf("expected EdgeReferences (static_access) %s -> %s", methodID, tgt)
		}
	}
}

// TestPHPRefForm_Attribute verifies a PHP 8 attribute `#[Foo]` / `#[Foo(...)]`
// emits an EdgeReferences with ref_context "static_access" to the attribute
// type. A class-level attribute attributes to the declaring class node; a
// method-level attribute to the enclosing method.
func TestPHPRefForm_Attribute(t *testing.T) {
	src := []byte(`<?php
#[Route("/api")]
class Svc {
    #[HttpGet]
    public function handle() { }
}
`)
	result, err := NewPHPExtractor().Extract("p/Svc.php", src)
	if err != nil {
		t.Fatal(err)
	}
	const typeID = "p/Svc.php::Svc"
	const methodID = "p/Svc.php::Svc.handle"

	if !hasPHPRefEdge(result.Edges, typeID, "Route", graph.EdgeReferences, graph.RefContextStaticAccess) {
		t.Errorf("expected EdgeReferences (static_access) %s -> Route", typeID)
	}
	if !hasPHPRefEdge(result.Edges, methodID, "HttpGet", graph.EdgeReferences, graph.RefContextStaticAccess) {
		t.Errorf("expected EdgeReferences (static_access) %s -> HttpGet", methodID)
	}
}

// TestPHPRefForm_OriginASTResolved verifies every reference-form edge rides
// OriginASTResolved — the load-bearing tier that keeps the cross-package
// guard from reverting the structural EdgeReferences edges to their
// unresolved placeholder.
func TestPHPRefForm_OriginASTResolved(t *testing.T) {
	src := []byte(`<?php
class Svc extends BaseSvc {
    public function handle($x) {
        $a = new RestClient();
        if ($x instanceof Response) {}
        $v = Status::OK;
    }
}
`)
	result, err := NewPHPExtractor().Extract("p/Svc.php", src)
	if err != nil {
		t.Fatal(err)
	}
	const typeID = "p/Svc.php::Svc"
	const methodID = "p/Svc.php::Svc.handle"

	cases := []struct {
		from, to string
		kind     graph.EdgeKind
		refctx   string
	}{
		{typeID, "BaseSvc", graph.EdgeReferences, graph.RefContextInherit},
		{methodID, "RestClient", graph.EdgeInstantiates, ""},
		{methodID, "Response", graph.EdgeReferences, graph.RefContextCast},
		{methodID, "Status", graph.EdgeReferences, graph.RefContextStaticAccess},
	}
	for _, c := range cases {
		if got := phpRefEdgeOrigin(result.Edges, c.from, c.to, c.kind, c.refctx); got != graph.OriginASTResolved {
			t.Errorf("%s -> %s (%s/%s) Origin = %q, want %q", c.from, c.to, c.kind, c.refctx, got, graph.OriginASTResolved)
		}
	}
}

// TestPHPRefForm_Negatives confirms the scope guards: a lowercase free call
// (`bar()`), an instance member call (`$this->foo()`), the relative scopes
// (`self::x`, `parent::base()`, `static::make()`), and a primitive type used
// in a cast / type position produce no reference-form edge.
func TestPHPRefForm_Negatives(t *testing.T) {
	src := []byte(`<?php
class Svc {
    public function handle($x) {
        bar();
        $this->foo();
        self::helper();
        $n = parent::base();
        $m = static::make();
        $s = "string";
        if ($x instanceof $klass) {}
    }
}
`)
	result, err := NewPHPExtractor().Extract("p/Svc.php", src)
	if err != nil {
		t.Fatal(err)
	}

	for _, e := range result.Edges {
		if e.Kind != graph.EdgeReferences && e.Kind != graph.EdgeInstantiates {
			continue
		}
		rc, _ := e.Meta["ref_context"].(string)
		switch e.To {
		case "unresolved::bar", "unresolved::foo", "unresolved::this",
			"unresolved::self", "unresolved::static", "unresolved::parent",
			"unresolved::helper", "unresolved::base", "unresolved::make",
			"unresolved::string", "unresolved::klass", "unresolved::x":
			t.Errorf("reference form must not emit %s (kind=%s ref_context=%q)", e.To, e.Kind, rc)
		}
	}
}
