package mcp

import (
	"context"
	"testing"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
)

func TestHydrateProxyTargets(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "local/a.go::Foo", Kind: graph.KindFunction, Name: "Foo"})
	proxyID := graph.ProxyNodeID("remoteB", "rb/x.go::Bar")
	g.AddNode(&graph.Node{ID: proxyID, Kind: graph.KindFunction, Name: "Bar", Origin: "remote:remoteB", Stub: true})
	g.AddEdge(&graph.Edge{From: "local/a.go::Foo", To: proxyID, Kind: graph.EdgeCalls})

	eng := query.NewEngine(g)
	srv := NewServer(eng, g, nil, nil, zap.NewNop(), nil)

	var hydrated []string
	srv.SetProxyHydrator(func(_ context.Context, id string) (int, error) {
		hydrated = append(hydrated, id)
		return 0, nil
	})

	// From a local node: hydrate its proxy neighbour.
	srv.hydrateProxyTargets(context.Background(), "local/a.go::Foo")
	if len(hydrated) != 1 || hydrated[0] != proxyID {
		t.Errorf("expected to hydrate the proxy neighbour; got %v", hydrated)
	}

	// From the proxy node itself: hydrate it directly.
	hydrated = nil
	srv.hydrateProxyTargets(context.Background(), proxyID)
	if len(hydrated) != 1 || hydrated[0] != proxyID {
		t.Errorf("expected to hydrate the proxy itself; got %v", hydrated)
	}

	// A local node with no proxy neighbour: no hydration.
	g.AddNode(&graph.Node{ID: "local/a.go::Baz", Kind: graph.KindFunction, Name: "Baz"})
	hydrated = nil
	srv.hydrateProxyTargets(context.Background(), "local/a.go::Baz")
	if len(hydrated) != 0 {
		t.Errorf("a node with no proxy neighbour must not hydrate; got %v", hydrated)
	}
}

func TestHydrateProxyTargets_NoHook_NoOp(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "local/a.go::Foo", Kind: graph.KindFunction, Name: "Foo"})
	eng := query.NewEngine(g)
	srv := NewServer(eng, g, nil, nil, zap.NewNop(), nil)
	// No hook installed: must be a safe no-op (no panic).
	srv.hydrateProxyTargets(context.Background(), "local/a.go::Foo")
}
