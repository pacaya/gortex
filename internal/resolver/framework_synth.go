package resolver

import "github.com/zzet/gortex/internal/graph"

// Framework-dispatch synthesizer engine.
//
// Direct AST/LSP resolution lands the calls a compiler can see. A large
// class of real call edges, though, is wired by a *framework* at runtime
// and is invisible to static resolution: a gRPC client stub dispatched
// to its server handler, a Temporal workflow proxy to its activity, an
// event published on one side of an in-process channel and handled on
// the other, a JS bridge method routed to its native implementation.
//
// FrameworkSynthesizer is the plugin contract for a pass that
// materialises one such family of edges. Every synthesizer is a
// full-recompute, idempotent pass: it derives each edge it owns from
// durable graph state (placeholder edges plus their Meta markers, shared
// topic nodes, registration call edges) so a reindex of any endpoint
// re-lands or un-lands the edge without leaving a stale one behind —
// graph.AddEdge dedupes by edge key and graph.EvictFile drops a node's
// edges in both directions. Every edge a synthesizer lands is stamped
// with provenance (StampSynthesized) so its origin is auditable and the
// `analyze kind=synthesizers` roll-up can attribute it.
//
// The engine is the single orchestration point: the indexers call
// RunFrameworkSynthesizers at every settle point (full index, watcher
// reindex, incremental reindex) in place of invoking each pass directly,
// so adding a synthesizer (a native-bridge resolver, an event-channel
// pass) is one line in defaultFrameworkSynthesizers rather than an edit
// at six call sites.
type FrameworkSynthesizer interface {
	// Name is the stable provenance tag stamped on every edge the
	// synthesizer lands (lower-kebab, e.g. "grpc-stub", "event-channel").
	Name() string
	// Synthesize runs the pass over g and returns the number of edges the
	// synthesizer owns (landed on a real target) after this run.
	Synthesize(g graph.Store) int
}

// Edge.Meta keys stamped by StampSynthesized.
const (
	// MetaSynthesizedBy names the synthesizer that produced an edge.
	MetaSynthesizedBy = "synthesized_by"
	// MetaProvenance records that an edge is a heuristic materialisation
	// rather than a compiler-verified fact.
	MetaProvenance = "provenance"
	// ProvenanceHeuristic is the MetaProvenance value the string- and
	// name-keyed framework synthesizers stamp — these edges are
	// framework-dispatch inferences correlated by a literal (an event
	// name, a dispatch string, a registry key) with no type evidence.
	ProvenanceHeuristic = "heuristic"
	// ProvenanceFramework is the MetaProvenance value the typed,
	// decorator-, base-list- or type-keyed synthesizers stamp — the
	// framework's own contract (a decorator, a generic base, a typed
	// listener parameter) names the target, so the edge carries more
	// confidence than a string-correlated guess. analyze kind=synthesizers
	// reports the two tiers separately from the same MetaProvenance read.
	ProvenanceFramework = "framework"
)

// Confidence tiers the framework synthesizers stamp on a landed edge.
// Typed/decorator/base-list/type-keyed passes (RTK Query, Celery, Spring,
// MediatR, Sidekiq, Laravel, GoFrame) use ConfidenceTyped; the string-
// and name-keyed passes (Vuex, Redux-thunk, object-registry, fn-pointer,
// Django) use ConfidenceHeuristic.
const (
	// ConfidenceTyped is the confidence for a type-/decorator-/base-list-
	// keyed dispatch edge — the framework contract names the target.
	ConfidenceTyped = 0.85
	// ConfidenceHeuristic is the confidence for a string-/name-keyed
	// dispatch edge — correlated by a literal, not by a type.
	ConfidenceHeuristic = 0.6
)

// Stable per-synthesizer provenance names. Used both as the registry
// label (for the report grouping) and as the value stamped on each
// landed edge, so the two never drift.
const (
	SynthGRPCStub            = "grpc-stub"
	SynthTemporalStub        = "temporal-stub"
	SynthEventChannel        = "event-channel"
	SynthSwiftObjC           = "swift-objc-bridge"
	SynthReactNative         = "react-native-bridge"
	SynthReactNativePair     = "react-native-native-pair"
	SynthObserverChannel     = "observer-channel"
	SynthClosureCollection   = "closure-collection"
	SynthReactSetState       = "react-setstate"
	SynthFlutterSetState     = "flutter-setstate"
	SynthKMPExpectActual     = "kmp-expect-actual"
	SynthExpoModules         = "expo-modules-bridge"
	SynthFabric              = "fabric-codegen"
	SynthMyBatis             = "mybatis"
	SynthRustScope           = "rust-scope"
	SynthFactoryChain        = "factory-chain"
	SynthSQLCallsite         = "sql-callsite"
	SynthStoreFactory        = "store-factory"
	SynthReduxThunk          = "redux-thunk"
	SynthNgRxEffect          = "ngrx-effect"
	SynthObjectRegistry      = "object-registry"
	SynthRTKQuery            = "rtk-query"
	SynthVuexDispatch        = "vuex-dispatch"
	SynthCelery              = "celery-dispatch"
	SynthSpringEvent         = "spring-event"
	SynthMediatR             = "mediatr-dispatch"
	SynthCSharpIfaceDispatch = "csharp-iface-dispatch"
	SynthSidekiq             = "sidekiq-dispatch"
	SynthLaravelEvent        = "laravel-event"
	SynthFnPointerDispatch   = "fn-pointer-dispatch"
	SynthMacroExpansion      = "macro-expansion"
	SynthGoFrameRoute        = "goframe-route"
	SynthDjangoDescriptor    = "django-descriptor"
	SynthExpressResolve      = "express-resolve"
	SynthReactResolve        = "react-resolve"
	SynthFastAPIResolve      = "fastapi-resolve"
	SynthRailsResolve        = "rails-resolve"
	SynthSwiftUIResolve      = "swiftui-resolve"
	SynthUIKitResolve        = "uikit-resolve"
	SynthVaporResolve        = "vapor-resolve"
	SynthGinMiddleware       = "gin-middleware"
	SynthSvelteKitLoad       = "sveltekit-load"
	SynthSpeculative         = "speculative-dispatch"
	SynthFnValue             = SynthFnValueCallback
	SynthPascalFormName      = SynthPascalForm
	SynthValueRefName        = SynthValueRef
)

// StampSynthesized marks an edge as the product of a framework
// synthesizer: which synthesizer produced it (name) and that it is a
// heuristic materialisation. Safe on an edge with a nil Meta map.
func StampSynthesized(e *graph.Edge, name string) {
	if e == nil {
		return
	}
	if e.Meta == nil {
		e.Meta = map[string]any{}
	}
	e.Meta[MetaSynthesizedBy] = name
	if _, ok := e.Meta[MetaProvenance]; !ok {
		e.Meta[MetaProvenance] = ProvenanceHeuristic
	}
}

// StampSynthesizedTyped marks an edge as the product of a typed-tier
// framework synthesizer: like StampSynthesized, but records
// ProvenanceFramework instead of ProvenanceHeuristic so the
// type-/decorator-/base-list-keyed passes (RTK Query, Celery, Spring,
// MediatR, Sidekiq, Laravel, GoFrame) separate from the string-keyed
// ones in analyze kind=synthesizers. Safe on an edge with a nil Meta map.
func StampSynthesizedTyped(e *graph.Edge, name string) {
	if e == nil {
		return
	}
	if e.Meta == nil {
		e.Meta = map[string]any{}
	}
	e.Meta[MetaProvenance] = ProvenanceFramework
	StampSynthesized(e, name)
}

// UnstampSynthesized clears the provenance markers an edge picked up from
// a synthesizer. Called when a pass re-orphans an edge (its target
// disappeared) so the edge reads as a plain placeholder again.
func UnstampSynthesized(e *graph.Edge) {
	if e == nil || e.Meta == nil {
		return
	}
	delete(e.Meta, MetaSynthesizedBy)
	delete(e.Meta, MetaProvenance)
}

// synthFunc adapts a plain pass function into a FrameworkSynthesizer so
// the existing passes (ResolveGRPCStubCalls, …) register without a
// wrapper type each.
type synthFunc struct {
	name string
	fn   func(graph.Store) int
}

func (s synthFunc) Name() string                 { return s.name }
func (s synthFunc) Synthesize(g graph.Store) int { return s.fn(g) }

// defaultFrameworkSynthesizers returns the registered framework
// synthesizers in run order. Order is load-bearing: every synthesizer
// here runs after InferImplements/InferOverrides (some depend on the
// EdgeImplements edges they produce) and before DetectCrossRepoEdges (so
// a cross-repo synthesized call gets its parallel cross_repo_calls edge).
// Native-bridge resolvers append to this slice.
func defaultFrameworkSynthesizers() []FrameworkSynthesizer {
	return []FrameworkSynthesizer{
		synthFunc{name: SynthGRPCStub, fn: ResolveGRPCStubCalls},
		synthFunc{name: SynthTemporalStub, fn: ResolveTemporalCalls},
		synthFunc{name: SynthEventChannel, fn: ResolveEventChannelCalls},
		synthFunc{name: SynthSwiftObjC, fn: ResolveSwiftObjCBridge},
		synthFunc{name: SynthReactNative, fn: ResolveReactNativeBridge},
		synthFunc{name: SynthReactNativePair, fn: ResolveReactNativeNativePairing},
		synthFunc{name: SynthObserverChannel, fn: ResolveObserverChannelCalls},
		synthFunc{name: SynthClosureCollection, fn: ResolveClosureCollectionCalls},
		synthFunc{name: SynthReactSetState, fn: ResolveReactSetStateCalls},
		synthFunc{name: SynthFlutterSetState, fn: ResolveFlutterSetStateCalls},
		synthFunc{name: SynthKMPExpectActual, fn: ResolveKMPExpectActual},
		synthFunc{name: SynthExpoModules, fn: ResolveExpoModuleBridge},
		synthFunc{name: SynthFabric, fn: ResolveFabricComponents},
		synthFunc{name: SynthMyBatis, fn: ResolveMyBatisCalls},
		synthFunc{name: SynthSQLCallsite, fn: ResolveSQLCallsites},
		// Store-factory (Zustand/Redux/Pinia/MobX) indirect action calls —
		// binds getState()-chain and destructured calls to the action node.
		synthFunc{name: SynthStoreFactory, fn: ResolveStoreFactoryCalls},
		// Redux Toolkit createAsyncThunk dispatch chains: a thunk →
		// each action/thunk it dispatches from its payload-creator body.
		// After store-factory so its action nodes are indexed for the
		// thunk → reducer cross-link.
		synthFunc{name: SynthReduxThunk, fn: ResolveReduxThunkCalls},
		// NgRx effects: a createEffect(() => actions$.pipe(ofType(X))) effect ->
		// the action X it reacts to. After the store/thunk passes so action
		// creator nodes are indexed.
		synthFunc{name: SynthNgRxEffect, fn: ResolveNgRxEffects},
		// Object-literal command/handler registry dispatch →
		// `new registry[key]().execute()`. Runs before the speculative
		// pass so a claimed dispatch site suppresses the hidden best-guess.
		synthFunc{name: SynthObjectRegistry, fn: ResolveObjectRegistryCalls},
		// RTK Query generated-hook → createApi endpoint, and component →
		// generated hook. Typed tier: the hook naming is RTK-contractual.
		synthFunc{name: SynthRTKQuery, fn: ResolveRTKQueryCalls},
		// Vuex string-keyed dispatch/commit → action/mutation, with
		// module-namespace disambiguation.
		synthFunc{name: SynthVuexDispatch, fn: ResolveVuexDispatchCalls},
		// Celery task dispatch: `task.delay()` / `send_task("name")` →
		// the decorator-gated task function. Typed tier.
		synthFunc{name: SynthCelery, fn: ResolveCeleryCalls},
		// Spring application events: publishEvent(new X()) → every
		// @EventListener / ApplicationListener<X>, type-keyed fan-out.
		synthFunc{name: SynthSpringEvent, fn: ResolveSpringEventCalls},
		// MediatR CQRS dispatch: Send(new X()) → the IRequestHandler<X>
		// Handle, Publish(new X()) → every INotificationHandler<X>.
		synthFunc{name: SynthMediatR, fn: ResolveMediatRCalls},
		// C# member-level interface dispatch: a call bound to an interface
		// member fans out to the same-named member on each in-repo
		// implementation, at the speculative (hidden-by-default) tier. After
		// the implements-producing passes so the impl fan-out is complete.
		synthFunc{name: SynthCSharpIfaceDispatch, fn: ResolveCSharpInterfaceDispatch},
		// Sidekiq job dispatch: Worker.perform_async(...) → the worker's
		// perform, namespace-aware. Include-gated, typed tier.
		synthFunc{name: SynthSidekiq, fn: ResolveSidekiqCalls},
		// Laravel events: event(new X()) / X::dispatch() → every listener
		// handle(X), from the Listeners convention and the $listen map.
		synthFunc{name: SynthLaravelEvent, fn: ResolveLaravelEventCalls},
		// C/C++ function-pointer dispatch: a fn registered into a struct's
		// fn-pointer field → the indirect recv->field() call, keyed by
		// (struct type, field) with a field-copy fixpoint.
		synthFunc{name: SynthFnPointerDispatch, fn: ResolveFnPointerDispatch},
		// C/C++ function-like macro expansion: a macro invocation
		// `CALL_M(o)` → each call hidden in the macro's replacement list,
		// attributed to the use-site line so a forward call walk shows the
		// call where the macro is invoked, not at its `#define`.
		synthFunc{name: SynthMacroExpansion, fn: ResolveMacroExpansionCalls},
		// Gin middleware-chain dispatcher → registered handlers. Bridges the
		// `c.handlers[idx](c)` indirection so ServeHTTP→handler reachability
		// flows; repo-scoped, gated on a dispatcher existing.
		synthFunc{name: SynthGinMiddleware, fn: ResolveGinMiddlewareCalls},
		// Express named-handler resolution: middleware idents and
		// XController.method args bound by directory convention.
		synthFunc{name: SynthExpressResolve, fn: ResolveExpressHandlers},
		// React custom-hook / context resolution: a `useAuth()` call binds to
		// its /hooks/ definition; a `*Context`/`*Provider` reference binds to
		// /context/ or /providers/, with the suffix-strip fallback.
		synthFunc{name: SynthReactResolve, fn: ResolveReactHooksContext},
		// FastAPI dependency / router fallback: a residual `Depends(get_db)`
		// binds to a /dependencies/ provider, an `include_router(api_router)`
		// to a /routers/ definition — only when reference resolution left the
		// target unresolved.
		synthFunc{name: SynthFastAPIResolve, fn: ResolveFastAPIDeps},
		// Rails receiver-constant resolution: a `UserService.perform` /
		// `User.find` / `ApplicationHelper.fmt` call binds to the directory-
		// located service / model / helper definition named by its receiver.
		synthFunc{name: SynthRailsResolve, fn: ResolveRailsRefs},
		// SwiftUI directory-convention fallback: a residual `*View` /
		// `*ViewModel` / `*Store` / `*Manager` / PascalCase-model reference
		// binds to its /Views/ /ViewModels/ /Stores/ /Models/ definition.
		synthFunc{name: SynthSwiftUIResolve, fn: ResolveSwiftUIRefs},
		// UIKit directory-convention fallback: a residual `*ViewController` /
		// `*Cell` / `*Delegate` / `*DataSource` reference binds to its
		// /ViewControllers/ /Cells/ /Delegates/ definition.
		synthFunc{name: SynthUIKitResolve, fn: ResolveUIKitRefs},
		// Vapor directory-convention fallback: a residual `*Controller` /
		// `*Middleware` reference binds to its /Controllers/ /Middleware/
		// definition. After UIKit so `*ViewController` binds there first.
		synthFunc{name: SynthVaporResolve, fn: ResolveVaporRefs},
		// GoFrame reflective route → controller method, joined by the
		// method's request-struct type rather than its name.
		synthFunc{name: SynthGoFrameRoute, fn: ResolveGoFrameRoutes},
		// SvelteKit +page ↔ +page.server load pairing: a route's page component
		// reaches its server data loader so a trace flows page→load. Repo-scoped.
		synthFunc{name: SynthSvelteKitLoad, fn: ResolveSvelteKitLoad},
		// Rust impl-block / self-receiver / module-path resolution
		// completion. Runs in the same settle window so residual
		// unresolved Rust calls land before external-call synthesis
		// classifies the rest as external.
		synthFunc{name: SynthRustScope, fn: ResolveRustScopeCalls},
		// After rust-scope and the implements/extends-producing passes so the
		// cross-file factory-chain walk + conformance hop see settled edges.
		synthFunc{name: SynthFactoryChain, fn: ResolveFactoryChains},
		// Function-as-value callback registration — binds each captured
		// value-position function identifier to its same-file definition and
		// drops unbound candidates. The per-language capture feeds it via
		// placeholder edges; the pass is inert until those land.
		synthFunc{name: SynthFnValue, fn: ResolveFnValueCallbacks},
		// Pascal unit ↔ form (.pas/.dfm) pairing by same-dir basename.
		synthFunc{name: SynthPascalFormName, fn: ResolvePascalForms},
		// Same-file distinctive value references → EdgeReads to the constant,
		// so a config constant's blast radius reaches every reader.
		synthFunc{name: SynthValueRefName, fn: ResolveValueRefs},
	}
}

// SynthCount is the per-synthesizer result row in a FrameworkSynthReport.
type SynthCount struct {
	Name  string `json:"name"`
	Edges int    `json:"edges"`
}

// FrameworkSynthReport is the aggregate result of one
// RunFrameworkSynthesizers invocation.
type FrameworkSynthReport struct {
	Total int          `json:"total"`
	Per   []SynthCount `json:"per_synthesizer"`
	// Gated counts synthesized reference/import edges dropped by the
	// cross-language-family gate (coincidental PascalCase collisions across
	// two known, different families; bridge synthesizers are exempt).
	Gated int `json:"gated_cross_family,omitempty"`
	// ReceiverGated counts C# member-call edges demoted to the speculative
	// tier because they attach to a same-named member of a type unrelated to
	// the edge's receiver_type.
	ReceiverGated int `json:"receiver_type_gated,omitempty"`
}

// RunFrameworkSynthesizers runs every registered framework synthesizer
// over g, in registration order, and returns the per-synthesizer and
// total landed-edge counts. A nil graph is a no-op.
func RunFrameworkSynthesizers(g graph.Store) FrameworkSynthReport {
	rep := FrameworkSynthReport{}
	if g == nil {
		return rep
	}
	for _, s := range defaultFrameworkSynthesizers() {
		n := s.Synthesize(g)
		rep.Per = append(rep.Per, SynthCount{Name: s.Name(), Edges: n})
		rep.Total += n
	}
	// Drop coincidental cross-language-family reference/import results before
	// the claiming resolvers run, so a gated edge cannot be mistaken for a
	// resolved placeholder downstream. Bridge synthesizers are exempt.
	rep.Gated = applyFrameworkFamilyGate(g)
	// Claiming resolvers run last — after every framework synthesizer has
	// had its chance to consume a pre-stamped placeholder, but before
	// external-call synthesis classifies the residual unresolved refs as
	// external. Reported in registration order for determinism.
	claimed := RunClaimingResolvers(g)
	for _, r := range defaultClaimingResolvers() {
		n := claimed[r.Name()]
		rep.Per = append(rep.Per, SynthCount{Name: r.Name(), Edges: n})
		rep.Total += n
	}
	// Receiver-type gate runs last: it corrects (demotes) already-bound C#
	// member calls, so it must see the settled call graph.
	rep.ReceiverGated = demoteCSharpMisattributedMemberCalls(g)
	return rep
}

// ClaimingResolver retroactively claims a residual unresolved reference —
// one naming no declared symbol — that the extractor could not pre-tag, and
// rewrites it to a framework-known target. This is the generic
// claimsReference hook: a resolver offers a cheap name-vocabulary pre-filter
// (Claims) and, when it wins, rebinds the edge (Resolve). It runs before
// external-call synthesis would otherwise discard the reference as external.
type ClaimingResolver interface {
	// Name is the stable provenance label stamped on the rebound edge.
	Name() string
	// Claims reports whether this resolver wants the unresolved edge — a
	// cheap pre-filter on the reference's vocabulary, no graph work.
	Claims(e *graph.Edge) bool
	// Resolve rebinds e.To to a concrete target, returning true on a hit.
	Resolve(g graph.Store, e *graph.Edge) bool
}

// defaultClaimingResolvers returns the registered claiming resolvers, in
// offer order.
func defaultClaimingResolvers() []ClaimingResolver {
	return []ClaimingResolver{
		DjangoDescriptorResolver{},
	}
}

// RunClaimingResolvers offers every residual unresolved EdgeCalls /
// EdgeReferences to the claiming resolvers; the first whose Claims pre-filter
// passes and whose Resolve lands a target wins. Returns the per-resolver
// count of claimed edges. Unresolved edges are collected before resolving so
// a resolver's ReindexEdges does not mutate a live iteration.
func RunClaimingResolvers(g graph.Store) map[string]int {
	out := map[string]int{}
	if g == nil {
		return out
	}
	resolvers := defaultClaimingResolvers()
	if len(resolvers) == 0 {
		return out
	}
	var pending []*graph.Edge
	for _, kind := range []graph.EdgeKind{graph.EdgeCalls, graph.EdgeReferences} {
		for e := range g.EdgesByKind(kind) {
			if e != nil && e.To != "" && graph.IsUnresolvedTarget(e.To) {
				pending = append(pending, e)
			}
		}
	}
	for _, e := range pending {
		for _, r := range resolvers {
			if r.Claims(e) && r.Resolve(g, e) {
				out[r.Name()]++
				break
			}
		}
	}
	return out
}
