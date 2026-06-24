package resolver

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

func goframeRoute(g *graph.Graph, routeID, file, method, path, reqType string) {
	g.AddNode(&graph.Node{ID: routeID, Kind: graph.KindContract, Name: method + " " + path, FilePath: file,
		Meta: map[string]any{"type": "http", "method": method, "path": path, "framework": "goframe", "goframe_request_type": reqType}})
	g.AddEdge(&graph.Edge{From: routeID, To: "unresolved::*." + reqType, Kind: graph.EdgeCalls, FilePath: file,
		Meta: map[string]any{"via": goframeRouteVia, "goframe_request_type": reqType, "goframe_route": routeID}})
}

func goframeMethod(g *graph.Graph, id, file, reqType string, bound bool) {
	meta := map[string]any{"goframe_request_type": reqType}
	if bound {
		meta["goframe_bound"] = true
	}
	g.AddNode(&graph.Node{ID: id, Kind: graph.KindMethod, Name: lastSeg(id), FilePath: file, Language: "go", Meta: meta})
}

func synthGoFrameCall(g graph.Store, from, to string) *graph.Edge {
	for e := range g.EdgesByKind(graph.EdgeCalls) {
		if e == nil || e.From != from || e.To != to || e.Meta == nil {
			continue
		}
		if by, _ := e.Meta[MetaSynthesizedBy].(string); by == SynthGoFrameRoute {
			return e
		}
	}
	return nil
}

func goframeHandlesRoute(g graph.Store, from, to string) bool {
	for e := range g.EdgesByKind(graph.EdgeHandlesRoute) {
		if e != nil && e.From == from && e.To == to {
			return true
		}
	}
	return false
}

func TestResolveGoFrameRoutes_JoinByRequestType(t *testing.T) {
	g := graph.New()
	goframeRoute(g, "route::goframe::POST::/users", "ctrl/user.go", "POST", "/users", "CreateReq")
	goframeMethod(g, "ctrl/user.go::UserCtrl.Create", "ctrl/user.go", "CreateReq", true)

	n := ResolveGoFrameRoutes(g)
	require.Equal(t, 1, n)
	// Call edge route → handler (for get_callers), joined by request type
	// even though the method name (Create) is not the route path.
	e := synthGoFrameCall(g, "route::goframe::POST::/users", "ctrl/user.go::UserCtrl.Create")
	require.NotNil(t, e)
	assert.Equal(t, ConfidenceTyped, e.Confidence)
	assert.Equal(t, ProvenanceFramework, e.Meta[MetaProvenance])
	// handles_route edge handler → route (for analyze routes).
	assert.True(t, goframeHandlesRoute(g, "ctrl/user.go::UserCtrl.Create", "route::goframe::POST::/users"))
}

func TestResolveGoFrameRoutes_AddonRootTiebreak(t *testing.T) {
	// Two controllers define a method taking the same request type; the one
	// bound via g.Bind(new(Ctrl)) wins.
	g := graph.New()
	goframeRoute(g, "route::goframe::POST::/users", "a/route.go", "POST", "/users", "CreateReq")
	goframeMethod(g, "a/ctrl.go::BoundCtrl.Create", "a/ctrl.go", "CreateReq", true)
	goframeMethod(g, "b/ctrl.go::OtherCtrl.Create", "b/ctrl.go", "CreateReq", false)

	ResolveGoFrameRoutes(g)
	assert.NotNil(t, synthGoFrameCall(g, "route::goframe::POST::/users", "a/ctrl.go::BoundCtrl.Create"),
		"the bound controller wins the request-type collision")
	assert.Nil(t, synthGoFrameCall(g, "route::goframe::POST::/users", "b/ctrl.go::OtherCtrl.Create"))
}

// goframeRoutePkg adds a route whose request struct is qualified by its
// declaring package (the `goframe_request_pkg` stamp the extractor derives from
// the struct file's package clause).
func goframeRoutePkg(g *graph.Graph, routeID, file, method, path, reqType, pkg string) {
	g.AddNode(&graph.Node{ID: routeID, Kind: graph.KindContract, Name: method + " " + path, FilePath: file,
		Meta: map[string]any{"type": "http", "method": method, "path": path, "framework": "goframe",
			"goframe_request_type": reqType, "goframe_request_pkg": pkg}})
	g.AddEdge(&graph.Edge{From: routeID, To: "unresolved::*." + reqType, Kind: graph.EdgeCalls, FilePath: file,
		Meta: map[string]any{"via": goframeRouteVia, "goframe_request_type": reqType,
			"goframe_request_pkg": pkg, "goframe_route": routeID}})
}

// goframeMethodPkg adds a handler whose request param is qualified
// (`*pkg.CreateReq`), so the resolver can join by (package, type), never by a
// bare same-named type across packages.
func goframeMethodPkg(g *graph.Graph, id, file, reqType, pkg string, bound bool) {
	meta := map[string]any{"goframe_request_type": reqType, "goframe_request_pkg": pkg}
	if bound {
		meta["goframe_bound"] = true
	}
	g.AddNode(&graph.Node{ID: id, Kind: graph.KindMethod, Name: lastSeg(id), FilePath: file, Language: "go", Meta: meta})
}

func TestResolveGoFrameRoutes_CrossPackageSameTypeName(t *testing.T) {
	// Two packages each declare their own `CreateReq` request struct (each
	// tagged via g.Meta) and a handler for it. The req struct (route source)
	// and the controller live in different directories — the realistic GoFrame
	// layout (api/ vs controller/) — so the same-directory tiebreak cannot
	// disambiguate. Each route must bind to ITS OWN package's handler; a route
	// must never cross-bind to the other package's same-named handler.
	g := graph.New()
	// Package a: route declared in package "a", handler takes *a.CreateReq.
	goframeRoutePkg(g, "route::goframe::POST::/a/create", "a/api/create.go", "POST", "/a/create", "CreateReq", "a")
	goframeMethodPkg(g, "a/controller/create.go::ACtrl.Create", "a/controller/create.go", "CreateReq", "a", false)
	// Package b: route declared in package "b", handler takes *b.CreateReq.
	goframeRoutePkg(g, "route::goframe::POST::/b/create", "b/api/create.go", "POST", "/b/create", "CreateReq", "b")
	goframeMethodPkg(g, "b/controller/create.go::BCtrl.Create", "b/controller/create.go", "CreateReq", "b", false)

	ResolveGoFrameRoutes(g)

	assert.NotNil(t, synthGoFrameCall(g, "route::goframe::POST::/a/create", "a/controller/create.go::ACtrl.Create"),
		"package a's route binds to package a's handler")
	assert.Nil(t, synthGoFrameCall(g, "route::goframe::POST::/a/create", "b/controller/create.go::BCtrl.Create"),
		"package a's route must NOT cross-bind to package b's handler")

	assert.NotNil(t, synthGoFrameCall(g, "route::goframe::POST::/b/create", "b/controller/create.go::BCtrl.Create"),
		"package b's route binds to package b's handler")
	assert.Nil(t, synthGoFrameCall(g, "route::goframe::POST::/b/create", "a/controller/create.go::ACtrl.Create"),
		"package b's route must NOT cross-bind to package a's handler")
}

func TestResolveGoFrameRoutes_NoHandlerStaysPlaceholder(t *testing.T) {
	g := graph.New()
	goframeRoute(g, "route::goframe::GET::/x", "r.go", "GET", "/x", "XReq")
	goframeMethod(g, "c.go::C.Other", "c.go", "YReq", false) // different request type

	assert.Equal(t, 0, ResolveGoFrameRoutes(g))
	assert.False(t, goframeHandlesRoute(g, "c.go::C.Other", "route::goframe::GET::/x"))
}
