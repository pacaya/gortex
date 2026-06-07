package graph

import "testing"

// TestStats_ExcludesProxyNodes asserts federation Option-B proxy nodes
// are never counted in Graph.Stats (R-FED-7).
func TestStats_ExcludesProxyNodes(t *testing.T) {
	g := New()
	g.AddNode(&Node{ID: "local/a.go::Foo", Kind: KindFunction, Name: "Foo", Language: "go"})
	g.AddNode(&Node{
		ID:     ProxyNodeID("remoteB", "b/x.go::Bar"),
		Kind:   KindFunction,
		Name:   "Bar",
		Origin: "remote:remoteB",
		Stub:   true,
	})

	st := g.Stats()
	if st.TotalNodes != 1 {
		t.Errorf("Stats.TotalNodes = %d, want 1 (proxy node excluded)", st.TotalNodes)
	}
	if st.ByKind["function"] != 1 {
		t.Errorf("ByKind[function] = %d, want 1 (proxy node excluded)", st.ByKind["function"])
	}
}
