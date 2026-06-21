package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

// phpTypedAsTargets collects the To-targets of every EdgeTypedAs edge whose From
// matches the given owner ID (empty owner = any).
func phpTypedAsTargets(edges []*graph.Edge, owner string) map[string]bool {
	out := map[string]bool{}
	for _, e := range edges {
		if e.Kind != graph.EdgeTypedAs {
			continue
		}
		if owner != "" && e.From != owner {
			continue
		}
		out[e.To] = true
	}
	return out
}

// TestPHPExtractor_TypedPropertyUseEdge: a typed property `private HttpResponse
// $r;` emits an EdgeTypedAs from the field to unresolved::HttpResponse; a scalar
// property `int $count;` emits no type-use edge.
func TestPHPExtractor_TypedPropertyUseEdge(t *testing.T) {
	src := []byte(`<?php
class Controller {
    private HttpResponse $r;
    public int $count;
    protected ?Logger $logger;
    public Foo|Bar $fb;
}
`)
	e := NewPHPExtractor()
	result, err := e.Extract("c.php", src)
	require.NoError(t, err)

	all := phpTypedAsTargets(result.Edges, "")
	assert.True(t, all["unresolved::HttpResponse"], "typed property HttpResponse should emit EdgeTypedAs")
	assert.True(t, all["unresolved::Logger"], "nullable ?Logger should strip ? and emit EdgeTypedAs")
	assert.True(t, all["unresolved::Foo"], "union member Foo should emit EdgeTypedAs")
	assert.True(t, all["unresolved::Bar"], "union member Bar should emit EdgeTypedAs")

	// Scalars never produce a type-use edge.
	assert.False(t, all["unresolved::int"], "scalar int must NOT emit EdgeTypedAs")

	// The HttpResponse edge is attributed to the field node.
	fieldEdge := phpTypedAsTargets(result.Edges, "c.php::Controller.r")
	assert.True(t, fieldEdge["unresolved::HttpResponse"], "property type edge attributed to the field node")
}

// TestPHPExtractor_TypedParamAndReturnUseEdges: typed parameters emit
// EdgeTypedAs on the owning function/method; a scalar param emits no type-use
// edge; namespaced types reduce to the bare last segment; return types remain
// covered by EdgeReturns (not double-emitted as EdgeTypedAs).
func TestPHPExtractor_TypedParamAndReturnUseEdges(t *testing.T) {
	src := []byte(`<?php
function handle(HttpResponse $r, int $n, \App\Models\User $u): Session {
    return new Session();
}
class Svc {
    public function run(Request $req, string $s): void {}
}
`)
	e := NewPHPExtractor()
	result, err := e.Extract("h.php", src)
	require.NoError(t, err)

	fnEdges := phpTypedAsTargets(result.Edges, "h.php::handle")
	assert.True(t, fnEdges["unresolved::HttpResponse"], "param type HttpResponse should emit EdgeTypedAs")
	assert.True(t, fnEdges["unresolved::User"], "namespaced \\App\\Models\\User reduces to bare User")
	assert.False(t, fnEdges["unresolved::int"], "scalar param int must NOT emit EdgeTypedAs")
	// Return type is carried by EdgeReturns, not duplicated as EdgeTypedAs.
	assert.False(t, fnEdges["unresolved::Session"], "return type must not be double-emitted as EdgeTypedAs")

	methEdges := phpTypedAsTargets(result.Edges, "h.php::Svc.run")
	assert.True(t, methEdges["unresolved::Request"], "method param type Request should emit EdgeTypedAs")
	assert.False(t, methEdges["unresolved::string"], "scalar param string must NOT emit EdgeTypedAs")

	// Return type still arrives as EdgeReturns.
	var sawReturn bool
	for _, ed := range result.Edges {
		if ed.Kind == graph.EdgeReturns && ed.From == "h.php::handle" && ed.To == "unresolved::Session" {
			sawReturn = true
		}
	}
	assert.True(t, sawReturn, "non-builtin return type still emits EdgeReturns")
}

// TestPHPExtractor_TypeUseEdgeOrigin: type-use edges ride at OriginASTInferred,
// matching the TS/C# templates so the resolver weights them as inferences.
func TestPHPExtractor_TypeUseEdgeOrigin(t *testing.T) {
	src := []byte(`<?php
class C {
    private Session $s;
}
`)
	result, err := NewPHPExtractor().Extract("o.php", src)
	require.NoError(t, err)

	var found bool
	for _, ed := range result.Edges {
		if ed.Kind == graph.EdgeTypedAs && ed.To == "unresolved::Session" {
			found = true
			assert.Equal(t, graph.OriginASTInferred, ed.Origin, "type-use edge must be OriginASTInferred")
		}
	}
	require.True(t, found, "expected an EdgeTypedAs to unresolved::Session")
}
