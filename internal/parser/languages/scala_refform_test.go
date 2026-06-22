package languages

import (
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// scalaRefEdge is a flattened view of a reference-form edge for assertions.
type scalaRefEdge struct {
	to     string // canonical type name (after the unresolved:: prefix)
	kind   graph.EdgeKind
	refCtx string
}

// extractScalaRefEdges runs the Scala extractor over src and returns every
// instantiate / structural-reference edge keyed by canonical target type.
func extractScalaRefEdges(t *testing.T, src string) []scalaRefEdge {
	t.Helper()
	res, err := NewScalaExtractor().Extract("demo.scala", []byte(src))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	var out []scalaRefEdge
	for _, e := range res.Edges {
		if e.Kind != graph.EdgeInstantiates && e.Kind != graph.EdgeReferences {
			continue
		}
		to := strings.TrimPrefix(e.To, "unresolved::")
		ctx, _ := e.Meta["ref_context"].(string)
		out = append(out, scalaRefEdge{to: to, kind: e.Kind, refCtx: ctx})
		// Every reference-form edge must be stamped OriginASTResolved so the
		// cross-package guard does not revert the bare unresolved target.
		if e.Origin != graph.OriginASTResolved {
			t.Errorf("edge to %s kind=%s origin=%q, want %q", e.To, e.Kind, e.Origin, graph.OriginASTResolved)
		}
	}
	return out
}

// hasRefEdge reports whether edges contains one matching to/kind/refCtx.
func hasRefEdge(edges []scalaRefEdge, to string, kind graph.EdgeKind, refCtx string) bool {
	for _, e := range edges {
		if e.to == to && e.kind == kind && e.refCtx == refCtx {
			return true
		}
	}
	return false
}

// countRefEdgesTo counts reference-form edges whose target is `to`.
func countRefEdgesTo(edges []scalaRefEdge, to string) int {
	n := 0
	for _, e := range edges {
		if e.to == to {
			n++
		}
	}
	return n
}

func TestScalaRefFormInstantiation(t *testing.T) {
	src := `
package demo
class Widget
object Cat
class Factory {
  def make(): Widget = new Widget()
  def cat(): Cat = Cat()
}
`
	edges := extractScalaRefEdges(t, src)
	if !hasRefEdge(edges, "Widget", graph.EdgeInstantiates, "") {
		t.Errorf("expected instantiate edge to Widget (new Widget()), got %+v", edges)
	}
	if !hasRefEdge(edges, "Cat", graph.EdgeInstantiates, "") {
		t.Errorf("expected instantiate edge to Cat (Cat() apply), got %+v", edges)
	}
}

func TestScalaRefFormInheritance(t *testing.T) {
	src := `
package demo
class Base
trait T1
trait T2
class X extends Base with T1 with T2
object O extends Foo
`
	edges := extractScalaRefEdges(t, src)
	for _, want := range []string{"Base", "T1", "T2"} {
		if !hasRefEdge(edges, want, graph.EdgeReferences, graph.RefContextInherit) {
			t.Errorf("expected inherit ref to %s, got %+v", want, edges)
		}
	}
	if !hasRefEdge(edges, "Foo", graph.EdgeReferences, graph.RefContextInherit) {
		t.Errorf("expected inherit ref to Foo (object O extends Foo), got %+v", edges)
	}
}

func TestScalaRefFormCasts(t *testing.T) {
	src := `
package demo
class Checker {
  def is(x: Any): Boolean = x.isInstanceOf[Foo]
  def as(x: Any): Foo = x.asInstanceOf[Foo]
  def m(x: Any): String = x match {
    case _: Bar => "b"
    case d: Baz => "z"
    case _ => "?"
  }
}
`
	edges := extractScalaRefEdges(t, src)
	for _, want := range []string{"Foo", "Bar", "Baz"} {
		if !hasRefEdge(edges, want, graph.EdgeReferences, graph.RefContextCast) {
			t.Errorf("expected cast ref to %s, got %+v", want, edges)
		}
	}
}

func TestScalaRefFormStaticAccess(t *testing.T) {
	src := `
package demo
class User {
  def color(): String = Color.Red
  def max(): Int = Config.MAX
}
`
	edges := extractScalaRefEdges(t, src)
	if !hasRefEdge(edges, "Color", graph.EdgeReferences, graph.RefContextStaticAccess) {
		t.Errorf("expected static_access ref to Color, got %+v", edges)
	}
	if !hasRefEdge(edges, "Config", graph.EdgeReferences, graph.RefContextStaticAccess) {
		t.Errorf("expected static_access ref to Config, got %+v", edges)
	}
}

func TestScalaRefFormNegatives(t *testing.T) {
	src := `
package demo
class Thing {
  val owner: Person = null
  def lower(): Int = foo()
  def self(): Int = this.x
  def low(): Int = lowercaseObj.field
  def num(p: Int): Int = p
  def prim(): Boolean = p.isInstanceOf[Int]
}
`
	edges := extractScalaRefEdges(t, src)

	// `foo()` is a lowercase apply — not construction.
	if hasRefEdge(edges, "foo", graph.EdgeInstantiates, "") {
		t.Errorf("lowercase apply foo() must not instantiate, got %+v", edges)
	}
	// `this.x` head is `this` — never static access.
	if countRefEdgesTo(edges, "this") != 0 {
		t.Errorf("this.x must not emit static_access, got %+v", edges)
	}
	// `lowercaseObj.field` head is lowercase — not static access.
	if hasRefEdge(edges, "lowercaseObj", graph.EdgeReferences, graph.RefContextStaticAccess) {
		t.Errorf("lowercase select must not emit static_access, got %+v", edges)
	}
	// `Int` is a primitive — filtered everywhere (annotation, cast, etc.).
	if countRefEdgesTo(edges, "Int") != 0 {
		t.Errorf("primitive Int must not emit any reference-form edge, got %+v", edges)
	}
	// `Person` here is only a val type annotation — owned by #143's typed_as
	// pass, never re-emitted as instantiate/inherit/cast/static_access.
	for _, e := range edges {
		if e.to == "Person" {
			t.Errorf("val type annotation Person must not produce a reference-form edge, got %+v", e)
		}
	}
}
