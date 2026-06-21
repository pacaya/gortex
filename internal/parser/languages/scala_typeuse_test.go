package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

// scalaTypedAsTargets returns the set of EdgeTypedAs targets emitted for src,
// asserting every such edge carries the AST-inferred origin.
func scalaTypedAsTargets(t *testing.T, file string, src string) map[string]bool {
	t.Helper()
	res, err := NewScalaExtractor().Extract(file, []byte(src))
	require.NoError(t, err)
	targets := map[string]bool{}
	for _, e := range edgesOfKind(res.Edges, graph.EdgeTypedAs) {
		assert.Equal(t, graph.OriginASTInferred, e.Origin,
			"EdgeTypedAs %s -> %s should be AST-inferred", e.From, e.To)
		targets[e.To] = true
	}
	return targets
}

// TestScalaTypeUse_FieldValAnnotation covers a class-field `val resp:
// HttpResponse = ...`: the named type surfaces as a usage edge, while a
// primitive-typed field emits nothing.
func TestScalaTypeUse_FieldValAnnotation(t *testing.T) {
	targets := scalaTypedAsTargets(t, "Svc.scala", `class Svc {
  val resp: HttpResponse = doThing()
  var count: Int = 0
}
`)
	assert.True(t, targets["unresolved::HttpResponse"],
		"val resp: HttpResponse should emit a usage edge")
	assert.False(t, targets["unresolved::Int"],
		"primitive Int must not emit a usage edge")
}

// TestScalaTypeUse_LazyVal covers a lazy val annotation.
func TestScalaTypeUse_LazyVal(t *testing.T) {
	targets := scalaTypedAsTargets(t, "App.scala", `object App {
  lazy val cfg: AppConfig = load()
}
`)
	assert.True(t, targets["unresolved::AppConfig"],
		"lazy val cfg: AppConfig should emit a usage edge")
}

// TestScalaTypeUse_DefParamsAndReturn covers `def f(p: Foo): Bar` — both the
// parameter type and the return type surface, while a primitive parameter
// (Int) and a primitive-ish return (Unit) emit nothing.
func TestScalaTypeUse_DefParamsAndReturn(t *testing.T) {
	targets := scalaTypedAsTargets(t, "H.scala", `object H {
  def handle(req: Request, n: Int): HttpResponse = doThing()
  def ack(req: Request): Unit = ()
}
`)
	assert.True(t, targets["unresolved::Request"], "param Request should surface")
	assert.True(t, targets["unresolved::HttpResponse"], "return HttpResponse should surface")
	assert.False(t, targets["unresolved::Int"], "primitive param Int must not surface")
	assert.False(t, targets["unresolved::Unit"], "Unit return must not surface")
}

// TestScalaTypeUse_TopLevelFunction covers a top-level (file-scope) def.
func TestScalaTypeUse_TopLevelFunction(t *testing.T) {
	targets := scalaTypedAsTargets(t, "top.scala", `def render(ctx: Context): Page = build(ctx)
`)
	assert.True(t, targets["unresolved::Context"], "top-level param Context should surface")
	assert.True(t, targets["unresolved::Page"], "top-level return Page should surface")
}

// TestScalaTypeUse_ClassConstructorParams covers `class C(repo: Repository)`.
func TestScalaTypeUse_ClassConstructorParams(t *testing.T) {
	targets := scalaTypedAsTargets(t, "UserService.scala", `class UserService(repo: Repository, limit: Int) {
  def noop(): Unit = ()
}
`)
	assert.True(t, targets["unresolved::Repository"], "constructor param Repository should surface")
	assert.False(t, targets["unresolved::Int"], "primitive constructor param must not surface")
}

// TestScalaTypeUse_ContainerUnwrap covers square-bracket generics: a type
// reachable only through Option/Seq/Future surfaces as the inner element type,
// and a multi-arg generic (Map) surfaces as the container name.
func TestScalaTypeUse_ContainerUnwrap(t *testing.T) {
	targets := scalaTypedAsTargets(t, "R.scala", `class R {
  val one: Option[User] = None
  val many: Seq[Widget] = Nil
  val nested: Future[Seq[Repo]] = null
  val lookup: Map[String, Account] = Map()
}
`)
	assert.True(t, targets["unresolved::User"], "Option[User] should unwrap to User")
	assert.True(t, targets["unresolved::Widget"], "Seq[Widget] should unwrap to Widget")
	assert.True(t, targets["unresolved::Repo"], "Future[Seq[Repo]] should unwrap to Repo")
	assert.True(t, targets["unresolved::Map"], "Map[K,V] (multi-arg) should surface as Map")
	assert.False(t, targets["unresolved::Option"], "Option wrapper itself must not surface")
}

// TestScalaTypeUse_TopLevelVal covers a file-level `val x: Foo` annotation.
func TestScalaTypeUse_TopLevelVal(t *testing.T) {
	targets := scalaTypedAsTargets(t, "globals.scala", `val client: HttpClient = makeClient()
`)
	assert.True(t, targets["unresolved::HttpClient"],
		"top-level val client: HttpClient should emit a usage edge")
}

// TestScalaTypeUse_TraitMethodSignatures covers abstract trait method
// signatures (function_declaration, no body).
func TestScalaTypeUse_TraitMethodSignatures(t *testing.T) {
	targets := scalaTypedAsTargets(t, "Repo.scala", `trait Repo {
  def findById(id: String): Option[User]
  def save(user: User): Unit
}
`)
	assert.True(t, targets["unresolved::User"],
		"trait method param/return User should surface")
}

// TestScalaTypeUse_DottedTypePrefix covers a package-qualified annotation.
func TestScalaTypeUse_DottedTypePrefix(t *testing.T) {
	targets := scalaTypedAsTargets(t, "D.scala", `class D {
  val s: com.example.HttpResponse = null
}
`)
	assert.True(t, targets["unresolved::HttpResponse"],
		"dotted prefix should be stripped to the simple name")
	assert.False(t, targets["unresolved::com.example.HttpResponse"],
		"the fully-qualified form must not be emitted")
}
