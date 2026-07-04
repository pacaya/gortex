package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func recvTypeOfCall(edges []*graph.Edge, from string) string {
	for _, ed := range edges {
		if ed.Kind == graph.EdgeCalls && ed.From == from {
			if rt, _ := ed.Meta["receiver_type"].(string); rt != "" {
				return rt
			}
		}
	}
	return ""
}

// A selector call on a parameter receiver stamps receiver_type from the
// caller's own parameter scope (mirrors Go's paramsByFunc), so the resolver
// can bind it to the right method.
func TestRsExtractor_ParamReceiverType(t *testing.T) {
	src := []byte(`struct HiArgs {}
impl HiArgs { fn has_implicit_path(&self) -> bool { true } }
fn search(args: &HiArgs) -> bool { args.has_implicit_path() }
`)
	e := NewRustExtractor()
	result, err := e.Extract("main.rs", src)
	require.NoError(t, err)
	assert.Equal(t, "HiArgs", recvTypeOfCall(result.Edges, "main.rs::search"))
}

// A self selector call binds to the enclosing impl type via the self scope.
func TestRsExtractor_SelfReceiverType(t *testing.T) {
	src := []byte(`struct Foo {}
impl Foo {
    fn a(&self) { self.b() }
    fn b(&self) {}
}
`)
	e := NewRustExtractor()
	result, err := e.Extract("foo.rs", src)
	require.NoError(t, err)
	assert.Equal(t, "Foo", recvTypeOfCall(result.Edges, "foo.rs::Foo.a"))
}

// recvTypeForMethod returns the receiver_type stamped on the selector-call
// edge from `from` whose method is `method` (To == "unresolved::*.<method>"),
// so a test can assert the inferred type for one specific call among several.
func recvTypeForMethod(edges []*graph.Edge, from, method string) (string, bool) {
	for _, ed := range edges {
		if ed.Kind == graph.EdgeCalls && ed.From == from && ed.To == "unresolved::*."+method {
			rt, _ := ed.Meta["receiver_type"].(string)
			return rt, true
		}
	}
	return "", false
}

// A let bound to a Self-returning associated constructor (default/with_capacity
// /from) seeds the local's type, so a later selector call on it stamps
// receiver_type for the resolver. A lowercase module function and a
// single-letter generic qualifier must NOT seed a type — the constructor
// returns a value of an unknown concrete type there, so guessing would risk a
// wrong-owner bind.
func TestRsExtractor_ConstructorReceiverType(t *testing.T) {
	src := []byte(`
struct Config {}
struct Buf {}
struct Matcher {}
fn run() {
    let c = Config::default();
    c.tune();
    let b = Buf::with_capacity(8);
    b.push(1);
    let m = Matcher::from(pattern);
    m.find();
    let g = thing::from(x);
    g.walk();
    let t = T::from(x);
    t.step();
}
`)
	e := NewRustExtractor()
	result, err := e.Extract("main.rs", src)
	require.NoError(t, err)

	got := func(method string) string {
		rt, _ := recvTypeForMethod(result.Edges, "main.rs::run", method)
		return rt
	}
	assert.Equal(t, "Config", got("tune"), "Config::default() should seed tenv[c]=Config")
	assert.Equal(t, "Buf", got("push"), "Buf::with_capacity() should seed tenv[b]=Buf")
	assert.Equal(t, "Matcher", got("find"), "Matcher::from() should seed tenv[m]=Matcher")
	assert.Equal(t, "", got("walk"), "lowercase module qualifier must not seed a type")
	assert.Equal(t, "", got("step"), "single-letter generic qualifier must not seed a type")
}

// A parameter shadows a same-named file-wide let binding at call resolution.
func TestRsExtractor_ParamShadowsLet(t *testing.T) {
	src := []byte(`struct A {}
struct B {}
impl A { fn go(&self) {} }
impl B { fn go(&self) {} }
fn outer(x: &A) { x.go(); }
fn other() { let x = B {}; x.go(); }
`)
	e := NewRustExtractor()
	result, err := e.Extract("m.rs", src)
	require.NoError(t, err)
	assert.Equal(t, "A", recvTypeOfCall(result.Edges, "m.rs::outer"))
	assert.Equal(t, "B", recvTypeOfCall(result.Edges, "m.rs::other"))
}
