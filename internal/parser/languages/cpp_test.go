package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestCppExtractor_Function(t *testing.T) {
	src := []byte(`#include <iostream>

void greet(const std::string& name) {
    std::cout << "Hello " << name << std::endl;
}
`)
	e := NewCppExtractor()
	result, err := e.Extract("main.cpp", src)
	require.NoError(t, err)

	funcs := nodesOfKind(result.Nodes, graph.KindFunction)
	assert.GreaterOrEqual(t, len(funcs), 1)
	assert.Equal(t, "greet", funcs[0].Name)
}

func TestCppExtractor_Class(t *testing.T) {
	src := []byte(`class Point {
public:
    int x, y;

    Point(int x, int y) : x(x), y(y) {}

    int distance() {
        return x * x + y * y;
    }
};
`)
	e := NewCppExtractor()
	result, err := e.Extract("point.cpp", src)
	require.NoError(t, err)

	types := nodesOfKind(result.Nodes, graph.KindType)
	assert.GreaterOrEqual(t, len(types), 1)
	assert.Equal(t, "Point", types[0].Name)

	methods := nodesOfKind(result.Nodes, graph.KindMethod)
	assert.GreaterOrEqual(t, len(methods), 1)

	// Check MemberOf edges point to class.
	memberEdges := edgesOfKind(result.Edges, graph.EdgeMemberOf)
	assert.GreaterOrEqual(t, len(memberEdges), 1)
	for _, edge := range memberEdges {
		assert.Equal(t, "point.cpp::Point", edge.To)
	}
}

func TestCppExtractor_Struct(t *testing.T) {
	src := []byte(`struct Vec3 {
    float x, y, z;
};
`)
	e := NewCppExtractor()
	result, err := e.Extract("vec.cpp", src)
	require.NoError(t, err)

	types := nodesOfKind(result.Nodes, graph.KindType)
	require.Len(t, types, 1)
	assert.Equal(t, "Vec3", types[0].Name)
}

func TestCppExtractor_Include(t *testing.T) {
	src := []byte(`#include <iostream>
#include "mylib.h"
`)
	e := NewCppExtractor()
	result, err := e.Extract("main.cpp", src)
	require.NoError(t, err)

	imports := edgesOfKind(result.Edges, graph.EdgeImports)
	assert.Len(t, imports, 2)
}

func TestCppExtractor_Namespace(t *testing.T) {
	src := []byte(`namespace math {
    int add(int a, int b) {
        return a + b;
    }
}
`)
	e := NewCppExtractor()
	result, err := e.Extract("math.cpp", src)
	require.NoError(t, err)

	pkgs := nodesOfKind(result.Nodes, graph.KindPackage)
	require.Len(t, pkgs, 1)
	assert.Equal(t, "math", pkgs[0].Name)
}

func TestCppExtractor_Enum(t *testing.T) {
	src := []byte(`enum class Color {
    Red,
    Green,
    Blue
};
`)
	e := NewCppExtractor()
	result, err := e.Extract("color.cpp", src)
	require.NoError(t, err)

	types := nodesOfKind(result.Nodes, graph.KindType)
	require.Len(t, types, 1)
	assert.Equal(t, "Color", types[0].Name)
}

func TestCppExtractor_Calls(t *testing.T) {
	src := []byte(`void greet() {}

void run() {
    greet();
}
`)
	e := NewCppExtractor()
	result, err := e.Extract("main.cpp", src)
	require.NoError(t, err)

	calls := edgesOfKind(result.Edges, graph.EdgeCalls)
	assert.GreaterOrEqual(t, len(calls), 1)
}

func TestCppExtractor_Extensions(t *testing.T) {
	e := NewCppExtractor()
	assert.Equal(t, "cpp", e.Language())
	exts := e.Extensions()
	assert.Contains(t, exts, ".cpp")
	assert.Contains(t, exts, ".cc")
	assert.Contains(t, exts, ".cxx")
	assert.Contains(t, exts, ".hpp")
	assert.NotContains(t, exts, ".h")
}

func TestCppExtractor_FnValueAddressOf(t *testing.T) {
	src := []byte("void handler() {}\n" +
		"struct Foo { void method() {} };\n" +
		"void run() {\n" +
		"  reg(&handler);\n" +
		"  bind(&Foo::method);\n" +
		"}\n")
	res, err := NewCppExtractor().Extract("s.cpp", src)
	require.NoError(t, err)

	forms := map[string]string{} // fn_value_name -> fn_ref_form
	for _, e := range res.Edges {
		if e.Meta == nil {
			continue
		}
		if v, _ := e.Meta["via"].(string); v != "callback_candidate" {
			continue
		}
		if name, _ := e.Meta["fn_value_name"].(string); name != "" {
			form, _ := e.Meta["fn_ref_form"].(string)
			forms[name] = form
			assert.Equal(t, "s.cpp::run", e.From, "captured in the enclosing function")
		}
	}
	if _, ok := forms["handler"]; !ok {
		t.Errorf("register(&handler) should capture handler as a function value (got %v)", forms)
	}
	if _, ok := forms["Foo::method"]; !ok {
		t.Errorf("bind(&Foo::method) should capture Foo::method as a function value (got %v)", forms)
	}
	assert.Equal(t, "address_of", forms["handler"], "&handler is an address-of form")
	assert.Equal(t, "address_of", forms["Foo::method"], "&Foo::method is an address-of form")
}

func TestCppExtractor_FactoryChainReceiver(t *testing.T) {
	src := []byte("struct Widget { Widget withX() { return *this; } };\n" +
		"Widget builder() { return Widget(); }\n" +
		"void run() {\n" +
		"  builder().withX().build();\n" +
		"}\n")
	res, err := NewCppExtractor().Extract("w.cpp", src)
	require.NoError(t, err)

	var withX, build *graph.Edge
	for _, e := range res.Edges {
		if e.Kind != graph.EdgeCalls {
			continue
		}
		switch e.To {
		case "unresolved::*.withX":
			withX = e
		case "unresolved::*.build":
			build = e
		}
	}
	require.NotNil(t, withX, "withX() call edge")
	require.NotNil(t, build, "build() call edge")
	// builder() is a typed factory, so withX()'s receiver resolves to Widget.
	assert.Equal(t, "Widget", withX.Meta["receiver_type"], "factory base resolves the chained receiver type")
	// build()'s hop (withX) is not a typed node here, so the chain receiver
	// expression is preserved for the graph-aware resolver to complete.
	if got, _ := build.Meta["receiver_expr"].(string); got != "builder().withX()" {
		t.Errorf("receiver_expr = %q, want builder().withX()", got)
	}
}
