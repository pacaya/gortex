package mcp

import (
	"context"

	"github.com/zzet/gortex/internal/graph"
)

// SetProxyHydrator installs the federation Option-B lazy hydration hook.
// The daemon wires it to ProxyHydrator.Hydrate when federation.edges is
// enabled; nil (the default) makes hydrateProxyTargets a no-op, so
// pure-local and Option-C-only installs pay nothing.
func (s *Server) SetProxyHydrator(h func(ctx context.Context, proxyID string) (int, error)) {
	s.proxyHydrate = h
}

// hydrateProxyTargets pulls one neighbour ring for any federation proxy
// node directly reachable from id (or id itself, when id is already a
// proxy), so a traversal that crosses into a proxy node sees a ring of the
// remote's neighbours rather than a dead end. A no-op when no hydrator is
// installed or there are no proxy targets. Errors are swallowed — a failed
// hydration degrades to the un-hydrated single-hop view, never a tool
// failure.
func (s *Server) hydrateProxyTargets(ctx context.Context, id string) {
	if s.proxyHydrate == nil || s.graph == nil || id == "" {
		return
	}
	if graph.IsProxyID(id) {
		_, _ = s.proxyHydrate(ctx, id)
		return
	}
	for _, e := range s.graph.GetOutEdges(id) {
		if graph.IsProxyID(e.To) {
			_, _ = s.proxyHydrate(ctx, e.To)
		}
	}
}
