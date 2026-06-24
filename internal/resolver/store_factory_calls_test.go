package resolver

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func storeAction(g *graph.Graph, id, file, binding, member string) {
	g.AddNode(&graph.Node{
		ID: id, Kind: graph.KindFunction, Name: member, FilePath: file,
		Meta: map[string]any{"store_factory": binding, "store_member": member},
	})
}

func storeCall(g *graph.Graph, callerID, callerFile, binding, action string) {
	g.AddNode(&graph.Node{ID: callerID, Kind: graph.KindFunction, Name: callerID, FilePath: callerFile})
	g.AddEdge(&graph.Edge{
		From: callerID, To: "unresolved::*." + action, Kind: graph.EdgeCalls, FilePath: callerFile,
		Meta: map[string]any{"via": "store-factory", "store_binding": binding, "store_action": action},
	})
}

func TestResolveStoreFactoryCalls_SingleBinding(t *testing.T) {
	g := graph.New()
	storeAction(g, "store.ts::useStore.reset@3", "store.ts", "useStore", "reset")
	storeCall(g, "caller.ts::hardReset", "caller.ts", "useStore", "reset")

	n := ResolveStoreFactoryCalls(g)
	if n != 1 {
		t.Fatalf("expected 1 resolved, got %d", n)
	}
	// hardReset should now call the reset action.
	var bound bool
	for _, e := range g.GetOutEdges("caller.ts::hardReset") {
		if e.To == "store.ts::useStore.reset@3" && e.Kind == graph.EdgeCalls {
			bound = true
			if e.Meta["synthesized_by"] != SynthStoreFactory {
				t.Errorf("expected synthesized_by=%s, got %v", SynthStoreFactory, e.Meta["synthesized_by"])
			}
		}
	}
	if !bound {
		t.Errorf("hardReset edge not rebound to the reset action")
	}
}

func TestResolveStoreFactoryCalls_CollisionPrefersSameFile(t *testing.T) {
	g := graph.New()
	// Two stores both bound to `useStore`, each with a reset, in different files.
	storeAction(g, "a.ts::useStore.reset@1", "a.ts", "useStore", "reset")
	storeAction(g, "b.ts::useStore.reset@1", "b.ts", "useStore", "reset")
	// Caller lives in a.ts → must bind to a's reset, never b's.
	storeCall(g, "a.ts::hardReset", "a.ts", "useStore", "reset")

	ResolveStoreFactoryCalls(g)
	for _, e := range g.GetOutEdges("a.ts::hardReset") {
		if e.Kind == graph.EdgeCalls && e.To == "b.ts::useStore.reset@1" {
			t.Fatalf("cross-store mis-bind: hardReset bound to b.ts reset")
		}
	}
	found := false
	for _, e := range g.GetOutEdges("a.ts::hardReset") {
		if e.To == "a.ts::useStore.reset@1" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected same-file bind to a.ts reset")
	}
}

func TestResolveStoreFactoryCalls_AmbiguousNotBound(t *testing.T) {
	g := graph.New()
	storeAction(g, "a.ts::useStore.reset@1", "a.ts", "useStore", "reset")
	storeAction(g, "b.ts::useStore.reset@1", "b.ts", "useStore", "reset")
	// Caller in a third file that can't be disambiguated.
	storeCall(g, "c.ts::hardReset", "c.ts", "useStore", "reset")

	ResolveStoreFactoryCalls(g)
	for _, e := range g.GetOutEdges("c.ts::hardReset") {
		if e.Kind == graph.EdgeCalls && (e.To == "a.ts::useStore.reset@1" || e.To == "b.ts::useStore.reset@1") {
			t.Fatalf("ambiguous call should not bind, but bound to %s", e.To)
		}
	}
}

func TestResolveStoreFactoryCalls_Idempotent(t *testing.T) {
	g := graph.New()
	storeAction(g, "store.ts::useStore.reset@3", "store.ts", "useStore", "reset")
	storeCall(g, "caller.ts::hardReset", "caller.ts", "useStore", "reset")

	first := ResolveStoreFactoryCalls(g)
	second := ResolveStoreFactoryCalls(g)
	if first != 1 || second != 1 {
		t.Errorf("expected idempotent resolve count 1/1, got %d/%d", first, second)
	}
}

// storeGetter adds the `useXStore` getter node a `defineStore`/`create` call is
// assigned to — the store DEFINITION anchor. It is NOT a store-factory action.
func storeGetter(g *graph.Graph, id, file, name string) {
	g.AddNode(&graph.Node{ID: id, Kind: graph.KindConstant, Name: name, FilePath: file})
}

func TestResolveStoreFactoryCalls_DefinitionFilePrefersGetterStore(t *testing.T) {
	// Two stores in DIFFERENT files both reuse the binding name `useUserStore`
	// and both define `login`, so the candidate list collides on (binding,
	// member). The User store's getter is defined in user.ts; a
	// useUserStore().login() call — from a component in neither store file —
	// must bind to user.ts's login, never the other store's.
	g := graph.New()
	storeGetter(g, "user.ts::useUserStore", "user.ts", "useUserStore")
	storeAction(g, "user.ts::useUserStore.login@5", "user.ts", "useUserStore", "login")
	storeAction(g, "legacy.ts::useUserStore.login@5", "legacy.ts", "useUserStore", "login")
	storeCall(g, "Login.vue::submit", "Login.vue", "useUserStore", "login")

	ResolveStoreFactoryCalls(g)
	var bound string
	for _, e := range g.GetOutEdges("Login.vue::submit") {
		if e.Kind == graph.EdgeCalls && e.Meta["synthesized_by"] == SynthStoreFactory {
			bound = e.To
		}
	}
	if bound != "user.ts::useUserStore.login@5" {
		t.Fatalf("useUserStore().login() bound to %q (want user.ts login defined beside the getter)", bound)
	}
}

func TestResolveStoreFactoryCalls_PiniaGetterDisambiguates(t *testing.T) {
	// Two stores each define `login`, keyed by their getter name. A
	// useUserStore-bound call must reach the user store's login, never the
	// cart store's — even with the caller in neither store file.
	g := graph.New()
	storeAction(g, "user.ts::useUserStore.login@3", "user.ts", "useUserStore", "login")
	storeAction(g, "cart.ts::useCartStore.login@3", "cart.ts", "useCartStore", "login")
	storeCall(g, "Profile.vue::go", "Profile.vue", "useUserStore", "login")

	ResolveStoreFactoryCalls(g)
	var bound string
	for _, e := range g.GetOutEdges("Profile.vue::go") {
		if e.Kind == graph.EdgeCalls && e.Meta["synthesized_by"] == SynthStoreFactory {
			bound = e.To
		}
	}
	if bound != "user.ts::useUserStore.login@3" {
		t.Fatalf("useUserStore.login() bound to %q (want user.ts login)", bound)
	}
}
