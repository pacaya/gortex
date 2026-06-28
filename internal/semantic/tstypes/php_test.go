package tstypes

import (
	"testing"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
)

// A typed parameter grounds its receiver: `$x->bar()` on a `Foo $x`
// resolves to Foo::bar.
func TestPHP_TypedParamResolvesCall(t *testing.T) {
	g, dir := buildFixture(t, map[string]string{
		"app.php": `<?php
class Foo {
    public function bar(): void {}
}

class App {
    public function f(Foo $x): void {
        $x->bar();
    }
}
`,
	})
	p := NewProvider(PHPSpec(), zap.NewNop())
	res, err := p.Enrich(g, dir)
	if err != nil {
		t.Fatal(err)
	}
	caller := nodeByNameKind(t, g, "f", graph.KindMethod)
	target := nodeByNameKind(t, g, "bar", graph.KindMethod)
	e := callEdgeTo(g, caller.ID, target.ID)
	if e == nil {
		t.Fatalf("typed-param call %s -> %s not resolved; edges: %v", caller.ID, target.ID, g.GetOutEdges(caller.ID))
	}
	assertASTProvenance(t, e, "php-types")
	if res.EdgesConfirmed+res.EdgesAdded == 0 {
		t.Errorf("result reported no edge work: %+v", res)
	}
}

// `$this->field->method()` resolves through the declared property type.
func TestPHP_ThisFieldResolvesCall(t *testing.T) {
	g, dir := buildFixture(t, map[string]string{
		"app.php": `<?php
class Foo {
    public function bar(): void {}
}

class App {
    private Foo $x;

    public function f(): void {
        $this->x->bar();
    }
}
`,
	})
	p := NewProvider(PHPSpec(), zap.NewNop())
	if _, err := p.Enrich(g, dir); err != nil {
		t.Fatal(err)
	}
	caller := nodeByNameKind(t, g, "f", graph.KindMethod)
	target := nodeByNameKind(t, g, "bar", graph.KindMethod)
	if callEdgeTo(g, caller.ID, target.ID) == nil {
		t.Fatalf("$this->x->bar() not resolved through field type; edges: %v", g.GetOutEdges(caller.ID))
	}
}

// `(new Foo())->bar()` and the parenthesis-free `new Foo()->bar()` both
// type their receiver from the constructor expression.
func TestPHP_NewExprChainResolvesCall(t *testing.T) {
	g, dir := buildFixture(t, map[string]string{
		"app.php": `<?php
class Foo {
    public function bar(): void {}
}

class App {
    public function f(): void {
        (new Foo())->bar();
        new Foo()->bar();
    }
}
`,
	})
	p := NewProvider(PHPSpec(), zap.NewNop())
	if _, err := p.Enrich(g, dir); err != nil {
		t.Fatal(err)
	}
	caller := nodeByNameKind(t, g, "f", graph.KindMethod)
	target := nodeByNameKind(t, g, "bar", graph.KindMethod)
	e := callEdgeTo(g, caller.ID, target.ID)
	if e == nil {
		t.Fatalf("new-expression chain not resolved; edges: %v", g.GetOutEdges(caller.ID))
	}
	assertASTProvenance(t, e, "php-types")
}

// A local bound from `new Foo()` propagates its type to a later call.
func TestPHP_LocalConstructorInferenceResolvesCall(t *testing.T) {
	g, dir := buildFixture(t, map[string]string{
		"app.php": `<?php
class Foo {
    public function bar(): void {}
}

class App {
    public function f(): void {
        $o = new Foo();
        $o->bar();
    }
}
`,
	})
	p := NewProvider(PHPSpec(), zap.NewNop())
	if _, err := p.Enrich(g, dir); err != nil {
		t.Fatal(err)
	}
	caller := nodeByNameKind(t, g, "f", graph.KindMethod)
	target := nodeByNameKind(t, g, "bar", graph.KindMethod)
	if callEdgeTo(g, caller.ID, target.ID) == nil {
		t.Fatalf("constructor-inferred local call not resolved; edges: %v", g.GetOutEdges(caller.ID))
	}
}

// A static `Foo::make()` resolves to the named class's method.
func TestPHP_StaticCallResolves(t *testing.T) {
	g, dir := buildFixture(t, map[string]string{
		"app.php": `<?php
class Foo {
    public static function make(): void {}
}

class App {
    public function f(): void {
        Foo::make();
    }
}
`,
	})
	p := NewProvider(PHPSpec(), zap.NewNop())
	if _, err := p.Enrich(g, dir); err != nil {
		t.Fatal(err)
	}
	caller := nodeByNameKind(t, g, "f", graph.KindMethod)
	target := nodeByNameKind(t, g, "make", graph.KindMethod)
	e := callEdgeTo(g, caller.ID, target.ID)
	if e == nil {
		t.Fatalf("static Foo::make() not resolved; edges: %v", g.GetOutEdges(caller.ID))
	}
	assertASTProvenance(t, e, "php-types")
}

// A constructor-promoted property is treated as a typed field, so a
// call through it resolves.
func TestPHP_PromotedParamFieldResolvesCall(t *testing.T) {
	g, dir := buildFixture(t, map[string]string{
		"app.php": `<?php
class Dep {
    public function work(): void {}
}

class App {
    public function __construct(private Dep $dep) {}

    public function f(): void {
        $this->dep->work();
    }
}
`,
	})
	p := NewProvider(PHPSpec(), zap.NewNop())
	if _, err := p.Enrich(g, dir); err != nil {
		t.Fatal(err)
	}
	caller := nodeByNameKind(t, g, "f", graph.KindMethod)
	target := nodeByNameKind(t, g, "work", graph.KindMethod)
	if callEdgeTo(g, caller.ID, target.ID) == nil {
		t.Fatalf("promoted-property call not resolved; edges: %v", g.GetOutEdges(caller.ID))
	}
}

// Assigning a typed parameter to a property gives the property that
// type, even when the property declaration itself is untyped.
func TestPHP_ThisFieldFromParamInference(t *testing.T) {
	g, dir := buildFixture(t, map[string]string{
		"app.php": `<?php
class Dep {
    public function work(): void {}
}

class App {
    private $cached;

    public function __construct(Dep $seed) {
        $this->cached = $seed;
    }

    public function f(): void {
        $this->cached->work();
    }
}
`,
	})
	p := NewProvider(PHPSpec(), zap.NewNop())
	if _, err := p.Enrich(g, dir); err != nil {
		t.Fatal(err)
	}
	caller := nodeByNameKind(t, g, "f", graph.KindMethod)
	target := nodeByNameKind(t, g, "work", graph.KindMethod)
	if callEdgeTo(g, caller.ID, target.ID) == nil {
		t.Fatalf("property-from-parameter inference did not resolve the call; edges: %v", g.GetOutEdges(caller.ID))
	}
}

// `class Impl extends Base implements Greeter` synthesizes the
// inheritance edges, and a call to an inherited method resolves through
// the extends climb.
func TestPHP_ExtendsImplementsAndInheritedCall(t *testing.T) {
	g, dir := buildFixture(t, map[string]string{
		"app.php": `<?php
interface Greeter {
    public function greet(): void;
}

class Base {
    public function run(): void {}
}

class Impl extends Base implements Greeter {
    public function greet(): void {}

    public function go(Impl $i): void {
        $i->run();
    }
}
`,
	})
	p := NewProvider(PHPSpec(), zap.NewNop())
	if _, err := p.Enrich(g, dir); err != nil {
		t.Fatal(err)
	}
	impl := nodeByNameKind(t, g, "Impl", graph.KindType)
	base := nodeByNameKind(t, g, "Base", graph.KindType)
	iface := nodeByNameKind(t, g, "Greeter", graph.KindInterface)

	ee := edgeBetween(g, impl.ID, graph.EdgeExtends, base.ID)
	if ee == nil {
		t.Fatalf("extends edge missing; edges: %v", g.GetOutEdges(impl.ID))
	}
	assertASTProvenance(t, ee, "php-types")

	ie := edgeBetween(g, impl.ID, graph.EdgeImplements, iface.ID)
	if ie == nil {
		t.Fatalf("implements edge missing; edges: %v", g.GetOutEdges(impl.ID))
	}
	assertASTProvenance(t, ie, "php-types")

	goMethod := nodeByNameKind(t, g, "go", graph.KindMethod)
	run := nodeByNameKind(t, g, "run", graph.KindMethod)
	if callEdgeTo(g, goMethod.ID, run.ID) == nil {
		t.Fatalf("inherited method call did not resolve through extends; edges: %v", g.GetOutEdges(goMethod.ID))
	}
}

// An ambiguous overload (two same-named methods, no way to choose) is
// skipped rather than guessed.
func TestPHP_AmbiguousOverloadSkipped(t *testing.T) {
	g, dir := buildFixture(t, map[string]string{
		"app.php": `<?php
class K {
    public function bar() {}
    public function bar() {}
}

class App {
    public function f(K $k): void {
        $k->bar();
    }
}
`,
	})
	p := NewProvider(PHPSpec(), zap.NewNop())
	if _, err := p.Enrich(g, dir); err != nil {
		t.Fatal(err)
	}
	caller := nodeByNameKind(t, g, "f", graph.KindMethod)
	assertUntouched(t, g, caller.ID, "bar", "php-types")
}

// `use App\Service` binds the short name to the imported FQN, steering a
// cross-file resolution onto the right package when several types share
// a name.
func TestPHP_ImportHintDisambiguatesCrossFile(t *testing.T) {
	g, dir := buildFixture(t, map[string]string{
		"App/Service.php": `<?php
namespace App;

class Service {
    public function run(): void {}
}
`,
		"Other/Service.php": `<?php
namespace Other;

class Service {
    public function run(): void {}
}
`,
		"Client/Handler.php": `<?php
namespace Client;

use App\Service;

class Handler {
    public function handle(Service $s): void {
        $s->run();
    }
}
`,
	})
	p := NewProvider(PHPSpec(), zap.NewNop())
	if _, err := p.Enrich(g, dir); err != nil {
		t.Fatal(err)
	}
	caller := nodeByNameKind(t, g, "handle", graph.KindMethod)
	want := "App/Service.php::Service.run"
	if callEdgeTo(g, caller.ID, want) == nil {
		t.Fatalf("import-hinted call did not land on %s; edges: %v", want, g.GetOutEdges(caller.ID))
	}
	wrong := "Other/Service.php::Service.run"
	if callEdgeTo(g, caller.ID, wrong) != nil {
		t.Fatalf("call landed on the wrong namespace's type %s", wrong)
	}
}

// A trait method called on a using-class receiver resolves through the
// trait-composition (`use T;`) edge: `$c->fn()` lands on T::fn.
func TestPHP_TraitMethodResolvesCall(t *testing.T) {
	g, dir := buildFixture(t, map[string]string{
		"app.php": `<?php
trait T {
    public function fn(): void {}
}

class C {
    use T;
}

class App {
    public function run(C $c): void {
        $c->fn();
    }
}
`,
	})
	p := NewProvider(PHPSpec(), zap.NewNop())
	if _, err := p.Enrich(g, dir); err != nil {
		t.Fatal(err)
	}
	c := nodeByNameKind(t, g, "C", graph.KindType)
	traitT := nodeByNameKind(t, g, "T", graph.KindType)
	// The unresolved `use T;` extends edge is resolved onto the trait node.
	if edgeBetween(g, c.ID, graph.EdgeExtends, traitT.ID) == nil {
		t.Fatalf("trait-use extends edge C -> T not resolved; edges: %v", g.GetOutEdges(c.ID))
	}
	run := nodeByNameKind(t, g, "run", graph.KindMethod)
	fn := nodeByNameKind(t, g, "fn", graph.KindMethod)
	e := callEdgeTo(g, run.ID, fn.ID)
	if e == nil {
		t.Fatalf("trait-method call $c->fn() not resolved to T::fn; edges: %v", g.GetOutEdges(run.ID))
	}
	assertASTProvenance(t, e, "php-types")
}

// A fluent trait method returning the trait type (`: self`) rebinds to
// the using class when chained: `$c->step()->done()` types step()'s
// result as C, so done() resolves on C. The inner trait call is direct;
// the outer chained edge is graded inferred.
func TestPHP_TraitSelfReturnChainRebindsToUsingClass(t *testing.T) {
	g, dir := buildFixture(t, map[string]string{
		"app.php": `<?php
trait T {
    public function step(): self { return $this; }
}

class C {
    use T;

    public function done(): void {}
}

class App {
    public function run(C $c): void {
        $c->step()->done();
    }
}
`,
	})
	p := NewProvider(PHPSpec(), zap.NewNop())
	if _, err := p.Enrich(g, dir); err != nil {
		t.Fatal(err)
	}
	run := nodeByNameKind(t, g, "run", graph.KindMethod)
	step := nodeByNameKind(t, g, "step", graph.KindMethod)
	done := nodeByNameKind(t, g, "done", graph.KindMethod)

	inner := callEdgeTo(g, run.ID, step.ID)
	if inner == nil {
		t.Fatalf("inner trait call $c->step() not resolved to T::step; edges: %v", g.GetOutEdges(run.ID))
	}
	assertASTProvenance(t, inner, "php-types")
	if inner.Meta["resolution_strategy"] == string(strategyInferred) {
		t.Errorf("inner direct trait call should not be graded inferred")
	}

	outer := callEdgeTo(g, run.ID, done.ID)
	if outer == nil {
		t.Fatalf("chained $c->step()->done() not rebound to C::done; edges: %v", g.GetOutEdges(run.ID))
	}
	if outer.Origin != graph.OriginASTResolved {
		t.Errorf("chained edge origin = %q, want %q", outer.Origin, graph.OriginASTResolved)
	}
	if outer.Meta["semantic_source"] != "php-types" {
		t.Errorf("chained edge semantic_source = %v, want php-types", outer.Meta["semantic_source"])
	}
	if outer.Meta["resolution_strategy"] != string(strategyInferred) {
		t.Errorf("chained edge resolution_strategy = %v, want %q", outer.Meta["resolution_strategy"], strategyInferred)
	}
	if outer.Confidence != inferredConfidence {
		t.Errorf("chained edge confidence = %v, want %v", outer.Confidence, inferredConfidence)
	}
}

// A fluent trait method declaring its own trait name as the return type
// (`: T`) also rebinds to the using class when chained.
func TestPHP_TraitNamedReturnChainRebindsToUsingClass(t *testing.T) {
	g, dir := buildFixture(t, map[string]string{
		"app.php": `<?php
trait T {
    public function step(): T { return $this; }
}

class C {
    use T;

    public function done(): void {}
}

class App {
    public function run(C $c): void {
        $c->step()->done();
    }
}
`,
	})
	p := NewProvider(PHPSpec(), zap.NewNop())
	if _, err := p.Enrich(g, dir); err != nil {
		t.Fatal(err)
	}
	run := nodeByNameKind(t, g, "run", graph.KindMethod)
	done := nodeByNameKind(t, g, "done", graph.KindMethod)
	outer := callEdgeTo(g, run.ID, done.ID)
	if outer == nil {
		t.Fatalf("chained $c->step()->done() (`: T` return) not rebound to C::done; edges: %v", g.GetOutEdges(run.ID))
	}
	if outer.Meta["resolution_strategy"] != string(strategyInferred) {
		t.Errorf("chained edge resolution_strategy = %v, want %q", outer.Meta["resolution_strategy"], strategyInferred)
	}
}

// A trait-use alias (`use T { T::fn as renamed; }`) exposes the renamed
// member on the using class: `$c->renamed()` resolves to T::fn.
func TestPHP_TraitAliasResolvesToOriginalMethod(t *testing.T) {
	g, dir := buildFixture(t, map[string]string{
		"app.php": `<?php
trait T {
    public function fn(): void {}
}

class C {
    use T {
        T::fn as renamed;
    }
}

class App {
    public function run(C $c): void {
        $c->renamed();
    }
}
`,
	})
	p := NewProvider(PHPSpec(), zap.NewNop())
	if _, err := p.Enrich(g, dir); err != nil {
		t.Fatal(err)
	}
	run := nodeByNameKind(t, g, "run", graph.KindMethod)
	fn := nodeByNameKind(t, g, "fn", graph.KindMethod)
	e := callEdgeTo(g, run.ID, fn.ID)
	if e == nil {
		t.Fatalf("aliased call $c->renamed() not resolved to T::fn; edges: %v", g.GetOutEdges(run.ID))
	}
	assertASTProvenance(t, e, "php-types")
}

// A trait conflict resolved with `insteadof` is NOT precedence-resolved:
// the member stays ambiguous across the two traits, so the call is
// skipped rather than bound to one arbitrary side, and nothing crashes.
func TestPHP_TraitInsteadofIsSkippedNotMisresolved(t *testing.T) {
	g, dir := buildFixture(t, map[string]string{
		"app.php": `<?php
trait A {
    public function fn(): void {}
}

trait B {
    public function fn(): void {}
}

class C {
    use A, B {
        A::fn insteadof B;
    }
}

class App {
    public function run(C $c): void {
        $c->fn();
    }
}
`,
	})
	p := NewProvider(PHPSpec(), zap.NewNop())
	if _, err := p.Enrich(g, dir); err != nil {
		t.Fatal(err)
	}
	run := nodeByNameKind(t, g, "run", graph.KindMethod)
	// Ambiguous across A::fn and B::fn → no engine-resolved edge for fn.
	assertUntouched(t, g, run.ID, "fn", "php-types")
}

// EnrichFile resolves only the named file's calls, leaving others alone.
func TestPHP_EnrichFileScopesToOneFile(t *testing.T) {
	g, dir := buildFixture(t, map[string]string{
		"foo.php": `<?php
class Foo {
    public function bar(): void {}
    public function baz(): void {}
}
`,
		"app.php": `<?php
class App {
    public function main(Foo $x): void {
        $x->bar();
    }
}
`,
		"other.php": `<?php
class Other {
    public function go(Foo $x): void {
        $x->baz();
    }
}
`,
	})
	p := NewProvider(PHPSpec(), zap.NewNop())
	if _, err := p.EnrichFile(g, dir, "app.php"); err != nil {
		t.Fatal(err)
	}
	caller := nodeByNameKind(t, g, "main", graph.KindMethod)
	bar := nodeByNameKind(t, g, "bar", graph.KindMethod)
	if callEdgeTo(g, caller.ID, bar.ID) == nil {
		t.Fatalf("EnrichFile did not resolve the target file's call")
	}
	other := nodeByNameKind(t, g, "go", graph.KindMethod)
	assertUntouched(t, g, other.ID, "baz", "php-types")
}
