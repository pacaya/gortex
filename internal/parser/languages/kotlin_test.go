package languages

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
	"github.com/zzet/gortex/internal/parser/tsitter/kotlin"
)

// TestKotlinAST_Debug dumps the AST to verify node types used in queries.
func TestKotlinAST_Debug(t *testing.T) {
	src := []byte(`package com.example

import kotlin.collections.List

interface Greeter {
    fun greet(name: String): String
}

class HelloGreeter : Greeter {
    override fun greet(name: String): String {
        return "Hello, $name"
    }

    fun helper() {}
}

data class User(val name: String, val age: Int)

object Singleton {
    fun instance(): Singleton = this
}

fun topLevel(): Int {
    println("hello")
    return 42
}

val VERSION = "1.0"
var counter = 0
`)
	lang := kotlin.GetLanguage()
	tree, err := parser.ParseFile(src, lang)
	require.NoError(t, err)
	defer tree.Close()

	root := tree.RootNode()
	var walk func(n *sitter.Node, depth int)
	walk = func(n *sitter.Node, depth int) {
		indent := ""
		for i := 0; i < depth; i++ {
			indent += "  "
		}
		if n.IsNamed() {
			t.Logf("%s%s [%d:%d - %d:%d] %q", indent, n.Type(),
				n.StartPoint().Row, n.StartPoint().Column,
				n.EndPoint().Row, n.EndPoint().Column,
				truncate(n.Content(src), 60))
		}
		for i := 0; i < int(n.ChildCount()); i++ {
			walk(n.Child(i), depth+1)
		}
	}
	walk(root, 0)
}

func truncate(s string, max int) string {
	if len(s) > max {
		return s[:max] + "..."
	}
	return s
}

func TestKotlinExtractor_ClassWithMethods(t *testing.T) {
	src := []byte(`class UserService {
    fun getUser(id: String): User {
        return findById(id)
    }

    fun deleteUser(id: String) {
        remove(id)
    }
}
`)
	e := NewKotlinExtractor()
	result, err := e.Extract("UserService.kt", src)
	require.NoError(t, err)

	types := nodesOfKind(result.Nodes, graph.KindType)
	require.Len(t, types, 1)
	assert.Equal(t, "UserService", types[0].Name)

	methods := nodesOfKind(result.Nodes, graph.KindMethod)
	require.Len(t, methods, 2)
	names := []string{methods[0].Name, methods[1].Name}
	assert.Contains(t, names, "getUser")
	assert.Contains(t, names, "deleteUser")

	memberEdges := edgesOfKind(result.Edges, graph.EdgeMemberOf)
	require.Len(t, memberEdges, 2)
	for _, edge := range memberEdges {
		assert.Equal(t, "UserService.kt::UserService", edge.To)
	}
}

func TestKotlinExtractor_Interface(t *testing.T) {
	src := []byte(`interface Repository {
    fun findById(id: String): User
    fun save(user: User)
}
`)
	e := NewKotlinExtractor()
	result, err := e.Extract("Repository.kt", src)
	require.NoError(t, err)

	ifaces := nodesOfKind(result.Nodes, graph.KindInterface)
	require.Len(t, ifaces, 1, "expected 1 interface, got %d", len(ifaces))
	assert.Equal(t, "Repository", ifaces[0].Name)
}

func TestKotlinExtractor_EnumClass(t *testing.T) {
	src := []byte(`enum class Direction {
    NORTH, SOUTH, EAST, WEST
}
`)
	e := NewKotlinExtractor()
	result, err := e.Extract("Direction.kt", src)
	require.NoError(t, err)

	types := nodesOfKind(result.Nodes, graph.KindType)
	require.Len(t, types, 1)
	assert.Equal(t, "Direction", types[0].Name)
	require.NotNil(t, types[0].Meta, "enum should carry Meta[\"kind\"]=\"enum\"")
	assert.Equal(t, "enum", types[0].Meta["kind"])

	entries := map[string]bool{}
	for _, n := range result.Nodes {
		if n.Kind == graph.KindVariable && n.Meta != nil && n.Meta["kind"] == "enum_entry" {
			entries[n.Name] = true
		}
	}
	assert.Equal(t, map[string]bool{"NORTH": true, "SOUTH": true, "EAST": true, "WEST": true}, entries)
}

func TestKotlinExtractor_TopLevelFunction(t *testing.T) {
	src := []byte(`fun greet(name: String): String {
    println(name)
    return "Hello, $name"
}

fun add(a: Int, b: Int): Int = a + b
`)
	e := NewKotlinExtractor()
	result, err := e.Extract("utils.kt", src)
	require.NoError(t, err)

	funcs := nodesOfKind(result.Nodes, graph.KindFunction)
	require.Len(t, funcs, 2)
	names := []string{funcs[0].Name, funcs[1].Name}
	assert.Contains(t, names, "greet")
	assert.Contains(t, names, "add")

	// Should not be methods.
	methods := nodesOfKind(result.Nodes, graph.KindMethod)
	assert.Empty(t, methods)
}

func TestKotlinExtractor_Imports(t *testing.T) {
	src := []byte(`import kotlin.collections.List
import com.example.service.UserService
import java.util.UUID

fun main() {}
`)
	e := NewKotlinExtractor()
	result, err := e.Extract("main.kt", src)
	require.NoError(t, err)

	imports := edgesOfKind(result.Edges, graph.EdgeImports)
	require.Len(t, imports, 3)
}

func TestKotlinExtractor_DataClass(t *testing.T) {
	src := []byte(`data class User(val name: String, val age: Int)
`)
	e := NewKotlinExtractor()
	result, err := e.Extract("User.kt", src)
	require.NoError(t, err)

	types := nodesOfKind(result.Nodes, graph.KindType)
	require.Len(t, types, 1)
	assert.Equal(t, "User", types[0].Name)
}

func TestKotlinExtractor_Object(t *testing.T) {
	src := []byte(`object Singleton {
    fun getInstance(): Singleton {
        return this
    }
}
`)
	e := NewKotlinExtractor()
	result, err := e.Extract("Singleton.kt", src)
	require.NoError(t, err)

	types := nodesOfKind(result.Nodes, graph.KindType)
	require.Len(t, types, 1)
	assert.Equal(t, "Singleton", types[0].Name)

	methods := nodesOfKind(result.Nodes, graph.KindMethod)
	require.Len(t, methods, 1)
	assert.Equal(t, "getInstance", methods[0].Name)

	memberEdges := edgesOfKind(result.Edges, graph.EdgeMemberOf)
	require.Len(t, memberEdges, 1)
	assert.Equal(t, "Singleton.kt::Singleton", memberEdges[0].To)
}

func TestKotlinExtractor_TopLevelProperties(t *testing.T) {
	src := []byte(`val VERSION = "1.0"
var counter = 0

class Foo {
    val internal = "hidden"
}
`)
	e := NewKotlinExtractor()
	result, err := e.Extract("config.kt", src)
	require.NoError(t, err)

	vars := nodesOfKind(result.Nodes, graph.KindVariable)
	require.Len(t, vars, 2, "expected only top-level properties")
	names := []string{vars[0].Name, vars[1].Name}
	assert.Contains(t, names, "VERSION")
	assert.Contains(t, names, "counter")
}

func TestKotlinExtractor_CallSites(t *testing.T) {
	src := []byte(`fun main() {
    println("hello")
    greet("world")
}

fun greet(name: String) {
    println(name)
}
`)
	e := NewKotlinExtractor()
	result, err := e.Extract("main.kt", src)
	require.NoError(t, err)

	calls := edgesOfKind(result.Edges, graph.EdgeCalls)
	assert.GreaterOrEqual(t, len(calls), 2, "expected at least 2 call edges")

	// Verify call targets contain println and greet.
	targets := make(map[string]bool)
	for _, c := range calls {
		targets[c.To] = true
	}
	assert.True(t, targets["unresolved::*.println"], "missing println call")
	assert.True(t, targets["unresolved::*.greet"], "missing greet call")
}

func TestKotlinExtractor_TypeEnv_ExplicitType(t *testing.T) {
	src := []byte(`class UserService {
    fun save() {}
}

fun main() {
    val svc: UserService = UserService()
    svc.save()
}
`)
	e := NewKotlinExtractor()
	result, err := e.Extract("app.kt", src)
	require.NoError(t, err)

	calls := edgesOfKind(result.Edges, graph.EdgeCalls)
	var saveCall *graph.Edge
	for _, c := range calls {
		if strings.HasSuffix(c.To, "save") {
			saveCall = c
			break
		}
	}
	require.NotNil(t, saveCall, "expected a call edge to save")
	require.NotNil(t, saveCall.Meta, "expected Meta on save call edge")
	assert.Equal(t, "UserService", saveCall.Meta["receiver_type"])
}

func TestKotlinExtractor_TypeEnv_Constructor(t *testing.T) {
	src := []byte(`class Client {
    fun connect() {}
}

fun main() {
    val client = Client()
    client.connect()
}
`)
	e := NewKotlinExtractor()
	result, err := e.Extract("app.kt", src)
	require.NoError(t, err)

	calls := edgesOfKind(result.Edges, graph.EdgeCalls)
	var connectCall *graph.Edge
	for _, c := range calls {
		if strings.HasSuffix(c.To, "connect") {
			connectCall = c
			break
		}
	}
	require.NotNil(t, connectCall)
	require.NotNil(t, connectCall.Meta)
	assert.Equal(t, "Client", connectCall.Meta["receiver_type"])
}

func TestKotlinExtractor_TypeEnv_Unknown(t *testing.T) {
	src := []byte(`fun getService(): Any = TODO()

fun main() {
    val svc = getService()
    svc.process()
}
`)
	e := NewKotlinExtractor()
	result, err := e.Extract("app.kt", src)
	require.NoError(t, err)

	calls := edgesOfKind(result.Edges, graph.EdgeCalls)
	var processCall *graph.Edge
	for _, c := range calls {
		if strings.HasSuffix(c.To, "process") {
			processCall = c
			break
		}
	}
	require.NotNil(t, processCall)
	assert.Nil(t, processCall.Meta, "unknown type should not produce Meta")
}

func TestKotlinExtractor_TypeEnv_Chain(t *testing.T) {
	src := []byte(`class Order {
    val id: Int = 0
}

class UserService {
    fun getOrder(): Order {
        return Order()
    }
}

fun main() {
    val svc = UserService()
    svc.getOrder().toString()
}
`)
	e := NewKotlinExtractor()
	result, err := e.Extract("app.kt", src)
	require.NoError(t, err)

	// Verify return_type is set on getOrder method.
	var getOrderNode *graph.Node
	for _, n := range result.Nodes {
		if n.Name == "getOrder" {
			getOrderNode = n
			break
		}
	}
	require.NotNil(t, getOrderNode, "expected a node for getOrder")
	assert.Equal(t, "Order", getOrderNode.Meta["return_type"])

	// Verify chain resolution: svc.getOrder() should resolve to Order.
	calls := edgesOfKind(result.Edges, graph.EdgeCalls)
	var toStringCall *graph.Edge
	for _, c := range calls {
		if strings.HasSuffix(c.To, "toString") {
			toStringCall = c
			break
		}
	}
	require.NotNil(t, toStringCall, "expected a call edge to toString")
	require.NotNil(t, toStringCall.Meta, "expected Meta on toString call edge")
	assert.Equal(t, "Order", toStringCall.Meta["receiver_type"])
}

func TestKotlinExtractor_DocAndVisibility(t *testing.T) {
	src := []byte(`package x

/**
 * Greeter does the thing.
 */
class Greeter {
    /** Says hi. */
    fun hello() {}

    private fun secret() {}
}

internal fun helper() {}
`)
	e := NewKotlinExtractor()
	result, err := e.Extract("Greeter.kt", src)
	require.NoError(t, err)

	byID := map[string]*graph.Node{}
	for _, n := range result.Nodes {
		byID[n.ID] = n
	}

	greeter := byID["Greeter.kt::Greeter"]
	require.NotNil(t, greeter)
	if greeter.Meta["visibility"] != "public" {
		t.Fatalf("Greeter.vis = %q", greeter.Meta["visibility"])
	}
	if greeter.Meta["doc"] != "Greeter does the thing." {
		t.Fatalf("Greeter.doc = %q", greeter.Meta["doc"])
	}

	hello := byID["Greeter.kt::Greeter.hello"]
	require.NotNil(t, hello)
	if hello.Meta["visibility"] != "public" {
		t.Fatalf("hello.vis = %q", hello.Meta["visibility"])
	}
	if hello.Meta["doc"] != "Says hi." {
		t.Fatalf("hello.doc = %q", hello.Meta["doc"])
	}

	secret := byID["Greeter.kt::Greeter.secret"]
	require.NotNil(t, secret)
	if secret.Meta["visibility"] != "private" {
		t.Fatalf("secret.vis = %q", secret.Meta["visibility"])
	}

	helper := byID["Greeter.kt::helper"]
	require.NotNil(t, helper)
	if helper.Meta["visibility"] != "internal" {
		t.Fatalf("helper.vis = %q", helper.Meta["visibility"])
	}
}

// kotlinCallEdgeTo returns the first EdgeCalls whose target ends in the
// given method name. Mirrors the lookup the type-env tests use.
func kotlinCallEdgeTo(result *parser.ExtractionResult, method string) *graph.Edge {
	for _, c := range edgesOfKind(result.Edges, graph.EdgeCalls) {
		if strings.HasSuffix(c.To, "."+method) {
			return c
		}
	}
	return nil
}

func TestKotlinExtractor_CompanionObject_AnonymousStaticDispatch(t *testing.T) {
	src := []byte(`class Foo {
    companion object {
        fun create(): Foo {
            return Foo()
        }
    }
}

fun main() {
    Foo.create()
}
`)
	e := NewKotlinExtractor()
	result, err := e.Extract("Foo.kt", src)
	require.NoError(t, err)

	byID := map[string]*graph.Node{}
	for _, n := range result.Nodes {
		byID[n.ID] = n
	}

	// The companion function is attributed to the enclosing class Foo.
	create := byID["Foo.kt::Foo.create"]
	require.NotNil(t, create, "companion create should be a member of Foo")
	assert.Equal(t, graph.KindMethod, create.Kind)
	assert.Equal(t, "Foo", create.Meta["receiver"])
	assert.Equal(t, true, create.Meta["static"])
	assert.Equal(t, true, create.Meta["companion"])

	// EdgeMemberOf should point the method at Foo.
	var memberOf bool
	for _, ed := range edgesOfKind(result.Edges, graph.EdgeMemberOf) {
		if ed.From == "Foo.kt::Foo.create" && ed.To == "Foo.kt::Foo" {
			memberOf = true
		}
	}
	assert.True(t, memberOf, "expected create -> Foo member_of edge")

	// The Foo.create() call carries receiver_type=Foo so the resolver
	// can land it on the companion's create method.
	callEdge := kotlinCallEdgeTo(result, "create")
	require.NotNil(t, callEdge, "expected a call edge to create")
	require.NotNil(t, callEdge.Meta, "expected receiver_type meta on Foo.create() call")
	assert.Equal(t, "Foo", callEdge.Meta["receiver_type"])
}

func TestKotlinExtractor_CompanionObject_NamedStaticDispatch(t *testing.T) {
	src := []byte(`class Bar {
    companion object Factory {
        @JvmStatic
        fun thing(): Bar = Bar()
    }
}

fun main() {
    Bar.thing()
    Bar.Factory.thing()
}
`)
	e := NewKotlinExtractor()
	result, err := e.Extract("Bar.kt", src)
	require.NoError(t, err)

	byID := map[string]*graph.Node{}
	for _, n := range result.Nodes {
		byID[n.ID] = n
	}

	// Both the type-receiver method (Bar.thing) and the named-companion
	// alias (Bar.Factory.thing) exist.
	barThing := byID["Bar.kt::Bar.thing"]
	require.NotNil(t, barThing, "expected Bar.thing member")
	assert.Equal(t, "Bar", barThing.Meta["receiver"])
	assert.Equal(t, "Factory", barThing.Meta["companion_name"])

	aliasThing := byID["Bar.kt::Bar.Factory.thing"]
	require.NotNil(t, aliasThing, "expected Bar.Factory.thing alias member")
	assert.Equal(t, "Bar.Factory", aliasThing.Meta["receiver"])
	assert.Equal(t, true, aliasThing.Meta["companion_alias"])

	// Bar.thing() resolves via receiver_type=Bar; Bar.Factory.thing()
	// resolves via receiver_type=Bar.Factory.
	var sawBar, sawFactory bool
	for _, c := range edgesOfKind(result.Edges, graph.EdgeCalls) {
		if !strings.HasSuffix(c.To, ".thing") || c.Meta == nil {
			continue
		}
		switch c.Meta["receiver_type"] {
		case "Bar":
			sawBar = true
		case "Bar.Factory":
			sawFactory = true
		}
	}
	assert.True(t, sawBar, "expected Bar.thing() call with receiver_type=Bar")
	assert.True(t, sawFactory, "expected Bar.Factory.thing() call with receiver_type=Bar.Factory")
}

func TestKotlinExtractor_CompanionObject_ConstProperty(t *testing.T) {
	src := []byte(`class Config {
    companion object {
        const val NAME = "config"
        val version = "1.0"
    }
}
`)
	e := NewKotlinExtractor()
	result, err := e.Extract("Config.kt", src)
	require.NoError(t, err)

	byID := map[string]*graph.Node{}
	for _, n := range result.Nodes {
		byID[n.ID] = n
	}

	name := byID["Config.kt::Config.NAME"]
	require.NotNil(t, name, "companion const should be a member of Config")
	assert.Equal(t, graph.KindConstant, name.Kind)
	assert.Equal(t, "Config", name.Meta["receiver"])
	assert.Equal(t, true, name.Meta["static"])

	version := byID["Config.kt::Config.version"]
	require.NotNil(t, version, "companion val should be a member of Config")
	assert.Equal(t, graph.KindField, version.Kind)
	assert.Equal(t, "Config", version.Meta["receiver"])

	// Companion props must not be emitted as top-level variables.
	for _, n := range nodesOfKind(result.Nodes, graph.KindVariable) {
		assert.NotEqual(t, "NAME", n.Name)
		assert.NotEqual(t, "version", n.Name)
	}
}

func TestKotlinExtractor_LambdaParam_NotMisresolved(t *testing.T) {
	// `item` is also the name of a top-level value of a known type;
	// inside the lambda it is a different, locally-bound parameter. The
	// lambda use of item.toLong() must NOT pick up the outer item's type.
	src := []byte(`class Widget {
    fun render() {}
}

fun process(list: List<Int>) {
    val item = Widget()
    list.map { item -> item.toLong() }
}
`)
	e := NewKotlinExtractor()
	result, err := e.Extract("app.kt", src)
	require.NoError(t, err)

	toLong := kotlinCallEdgeTo(result, "toLong")
	require.NotNil(t, toLong, "expected a call edge to toLong")
	// The lambda parameter shadows the outer `item`, so no outer
	// receiver_type (Widget) may be attached.
	if toLong.Meta != nil {
		assert.Nil(t, toLong.Meta["receiver_type"],
			"lambda-param receiver must not be resolved against the outer type env")
	}
}

func TestKotlinExtractor_LambdaImplicitIt_NotMisresolved(t *testing.T) {
	// A top-level `it` of a known type exists; the implicit lambda `it`
	// must shadow it inside `forEach { it.compute() }`.
	src := []byte(`class Engine {
    fun compute() {}
}

fun run(list: List<Int>) {
    val it = Engine()
    list.forEach { it.compute() }
}
`)
	e := NewKotlinExtractor()
	result, err := e.Extract("run.kt", src)
	require.NoError(t, err)

	compute := kotlinCallEdgeTo(result, "compute")
	require.NotNil(t, compute, "expected a call edge to compute")
	if compute.Meta != nil {
		assert.Nil(t, compute.Meta["receiver_type"],
			"implicit lambda `it` must not be resolved against the outer type env")
	}
}
