package languages

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// TestCPointerReturnFunctions verifies that pointer-return function
// definitions and prototypes produce nodes, and that call edges from inside a
// pointer-return function body are attributed to the enclosing function (which
// only works once the enclosing node exists).
func TestCPointerReturnFunctions(t *testing.T) {
	src := []byte(`
robj *streamDup(robj *o) {
    return copyObject(o);
}

static inline list *watchedKeyGetClients(int i) {
    return listCreate();
}

robj **vectorDup(robj **v);

int plain(void) { return 0; }
`)
	e := NewCExtractor()
	res, err := e.Extract("t.c", src)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}

	nodes := map[string]*graph.Node{}
	for _, n := range res.Nodes {
		if n.Kind == graph.KindFunction {
			nodes[n.Name] = n
		}
	}

	for _, name := range []string{"streamDup", "watchedKeyGetClients", "vectorDup", "plain"} {
		if nodes[name] == nil {
			t.Errorf("expected function node %q, missing", name)
		}
	}

	// static inline pointer-return keeps its file-local linkage stamp.
	if n := nodes["watchedKeyGetClients"]; n != nil {
		if v, _ := n.Meta["scope_static"].(bool); !v {
			t.Errorf("watchedKeyGetClients should be scope_static, meta=%v", n.Meta)
		}
	}
	// streamDup is not static.
	if n := nodes["streamDup"]; n != nil {
		if _, ok := n.Meta["scope_static"]; ok {
			t.Errorf("streamDup should not be scope_static")
		}
	}
	// The prototype-only vectorDup is marked as a prototype.
	if n := nodes["vectorDup"]; n != nil {
		if v, _ := n.Meta["prototype"].(bool); !v {
			t.Errorf("vectorDup should be a prototype, meta=%v", n.Meta)
		}
	}

	// The call to copyObject inside streamDup's body must be attributed to
	// streamDup — the knock-on fix: without an enclosing node, the call edge
	// is dropped.
	var found bool
	for _, ed := range res.Edges {
		if ed.Kind == graph.EdgeCalls && ed.From == "t.c::streamDup" && ed.To == "unresolved::copyObject" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected call edge streamDup -> copyObject; edges=%v", callEdges(res.Edges))
	}
}

func callEdges(edges []*graph.Edge) []string {
	var out []string
	for _, e := range edges {
		if e.Kind == graph.EdgeCalls {
			out = append(out, e.From+" -> "+e.To)
		}
	}
	return out
}
