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
	SynthGRPCStub          = "grpc-stub"
	SynthTemporalStub      = "temporal-stub"
	SynthEventChannel      = "event-channel"
	SynthSwiftObjC         = "swift-objc-bridge"
	SynthReactNative       = "react-native-bridge"
	SynthReactNativePair   = "react-native-native-pair"
	SynthObserverChannel   = "observer-channel"
	SynthClosureCollection = "closure-collection"
	SynthReactSetState     = "react-setstate"
	SynthFlutterSetState   = "flutter-setstate"
	SynthKMPExpectActual   = "kmp-expect-actual"
	SynthExpoModules       = "expo-modules-bridge"
	SynthFabric            = "fabric-codegen"
	SynthMyBatis           = "mybatis"
	SynthRustScope         = "rust-scope"
	SynthSQLCallsite       = "sql-callsite"
	SynthStoreFactory      = "store-factory"
	SynthReduxThunk        = "redux-thunk"
	SynthObjectRegistry    = "object-registry"
	SynthRTKQuery          = "rtk-query"
	SynthVuexDispatch      = "vuex-dispatch"
	SynthCelery            = "celery-dispatch"
	SynthSpringEvent       = "spring-event"
	SynthGinMiddleware     = "gin-middleware"
	SynthSvelteKitLoad     = "sveltekit-load"
	SynthSpeculative       = "speculative-dispatch"
	SynthFnValue           = SynthFnValueCallback
	SynthPascalFormName    = SynthPascalForm
	SynthValueRefName      = SynthValueRef
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
		// Gin middleware-chain dispatcher → registered handlers. Bridges the
		// `c.handlers[idx](c)` indirection so ServeHTTP→handler reachability
		// flows; repo-scoped, gated on a dispatcher existing.
		synthFunc{name: SynthGinMiddleware, fn: ResolveGinMiddlewareCalls},
		// SvelteKit +page ↔ +page.server load pairing: a route's page component
		// reaches its server data loader so a trace flows page→load. Repo-scoped.
		synthFunc{name: SynthSvelteKitLoad, fn: ResolveSvelteKitLoad},
		// Rust impl-block / self-receiver / module-path resolution
		// completion. Runs in the same settle window so residual
		// unresolved Rust calls land before external-call synthesis
		// classifies the rest as external.
		synthFunc{name: SynthRustScope, fn: ResolveRustScopeCalls},
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
	return rep
}
