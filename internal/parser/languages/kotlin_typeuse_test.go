package languages

import (
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// kotlinTypeUseSrc exercises every type-use surface the tree-sitter-only
// projection must emit: a typed parameter (`url: String` — primitive,
// skipped), a nullable named parameter (`opts: RequestOptions?`), a
// declared return type (`: HttpResponse`), a local `val` annotation
// (`val client: OkHttpClient`), and a primitive local annotation
// (`var count: Int` — skipped).
const kotlinTypeUseSrc = `package x

class Service {
    fun fetch(url: String, opts: RequestOptions?): HttpResponse {
        val client: OkHttpClient = OkHttpClient()
        var count: Int = 0
        return HttpResponse()
    }
}
`

func TestKotlinTypeUse_LocalAnnotationAndParam(t *testing.T) {
	nodes, edges := runKotlinExtract(t, "x/Service.kt", kotlinTypeUseSrc)

	typedAs := edgesByKind(edges, graph.EdgeTypedAs)
	returns := edgesByKind(edges, graph.EdgeReturns)

	// The enclosing method node id.
	methodID := "x/Service.kt::Service.fetch"
	if nodeByID(nodes, methodID) == nil {
		t.Fatalf("expected method node %q; got %v", methodID, nodeNames(nodesOfKind(nodes, graph.KindMethod)))
	}

	// 1. Local `val client: OkHttpClient` must emit EdgeTypedAs →
	//    unresolved::OkHttpClient, attributed to the enclosing method
	//    (variable-annotation edges hang off the enclosing function).
	foundLocal := false
	for _, e := range typedAs {
		if e.To == "unresolved::OkHttpClient" {
			foundLocal = true
			if e.From != methodID {
				t.Errorf("local val type-use should attribute to enclosing method %q; got From=%q", methodID, e.From)
			}
			if e.Origin != graph.OriginASTInferred {
				t.Errorf("local val type-use Origin = %v; want OriginASTInferred", e.Origin)
			}
		}
	}
	if !foundLocal {
		t.Errorf("expected EdgeTypedAs → unresolved::OkHttpClient; got %v", edgeTargets(typedAs))
	}

	// 2. The typed parameter `opts: RequestOptions?` must emit a KindParam
	//    node and an EdgeTypedAs → unresolved::RequestOptions (nullable `?`
	//    stripped) from the param node.
	var paramID string
	for _, n := range nodesOfKind(nodes, graph.KindParam) {
		if n.Name == "opts" {
			paramID = n.ID
		}
	}
	if paramID == "" {
		t.Fatalf("expected KindParam node for `opts`; got %v", nodeNames(nodesOfKind(nodes, graph.KindParam)))
	}
	foundParamType := false
	for _, e := range typedAs {
		if e.From == paramID && e.To == "unresolved::RequestOptions" {
			foundParamType = true
		}
	}
	if !foundParamType {
		t.Errorf("expected EdgeTypedAs %s → unresolved::RequestOptions; got %v", paramID, edgeTargets(typedAs))
	}

	// 3. The declared return type must emit EdgeReturns → unresolved::HttpResponse.
	foundReturn := false
	for _, e := range returns {
		if e.From == methodID && e.To == "unresolved::HttpResponse" {
			foundReturn = true
		}
	}
	if !foundReturn {
		t.Errorf("expected EdgeReturns %s → unresolved::HttpResponse; got %v", methodID, edgeTargets(returns))
	}

	// 4. Primitives must NOT emit type-use edges: neither the `url: String`
	//    parameter nor the `var count: Int` local annotation.
	for _, e := range typedAs {
		if strings.HasSuffix(e.To, "::String") || strings.HasSuffix(e.To, "::Int") {
			t.Errorf("primitive type %q must not emit EdgeTypedAs", e.To)
		}
	}
	for _, e := range returns {
		if strings.HasSuffix(e.To, "::Int") || strings.HasSuffix(e.To, "::String") {
			t.Errorf("primitive type %q must not emit EdgeReturns", e.To)
		}
	}
}

// TestKotlinTypeUse_TopLevelFunctionAndProperty checks a free function's
// parameter/return edges and a top-level property annotation falling back
// to the file node as owner.
func TestKotlinTypeUse_TopLevelFunctionAndProperty(t *testing.T) {
	src := `package x

val shared: OkHttpClient = OkHttpClient()

fun build(cfg: Config): Builder {
    return Builder()
}
`
	nodes, edges := runKotlinExtract(t, "x/top.kt", src)
	typedAs := edgesByKind(edges, graph.EdgeTypedAs)
	returns := edgesByKind(edges, graph.EdgeReturns)

	funcID := "x/top.kt::build"
	if nodeByID(nodes, funcID) == nil {
		t.Fatalf("expected function node %q", funcID)
	}

	// Top-level property annotation falls back to the file node.
	foundTopProp := false
	for _, e := range typedAs {
		if e.To == "unresolved::OkHttpClient" && e.From == "x/top.kt" {
			foundTopProp = true
		}
	}
	if !foundTopProp {
		t.Errorf("expected top-level property EdgeTypedAs file → unresolved::OkHttpClient; got %v", edgeTargets(typedAs))
	}

	// Parameter type edge.
	foundParam := false
	for _, e := range typedAs {
		if e.To == "unresolved::Config" {
			foundParam = true
		}
	}
	if !foundParam {
		t.Errorf("expected param EdgeTypedAs → unresolved::Config; got %v", edgeTargets(typedAs))
	}

	// Return type edge.
	foundReturn := false
	for _, e := range returns {
		if e.From == funcID && e.To == "unresolved::Builder" {
			foundReturn = true
		}
	}
	if !foundReturn {
		t.Errorf("expected EdgeReturns %s → unresolved::Builder; got %v", funcID, edgeTargets(returns))
	}
}
