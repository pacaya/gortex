package resolver

import (
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/zzet/gortex/internal/graph"
)

// temporalStubPrefix is the placeholder namespace the Go extractor
// emits for a Temporal workflow → activity (or workflow → child
// workflow) dispatch it can't land locally
// (`unresolved::temporal::<kind>::<name>`).
const temporalStubPrefix = unresolvedPrefix + "temporal::"

// temporalEnvDefaultConfidence is stamped on a stub edge whose name was
// resolved through an env-var-with-literal-default variable (the parser
// tags it `temporal_name_origin=env_default`). It sits in the
// speculative band (< 0.5) so the edge lands at the AMBIGUOUS label and,
// together with MetaSpeculative, is hidden from default queries: the
// runtime env override may name a different handler than the default.
const temporalEnvDefaultConfidence = 0.4

// temporalCrossLangConfidence is stamped on a cross-language Temporal link
// (e.g. a Java service that starts a Go workflow, matched by canonical
// name across a type-system boundary with no compiler guarantee the names
// line up). It sits in the speculative band so the edge is hidden from
// default queries, consistent with the env-default tier.
const temporalCrossLangConfidence = 0.4

// Temporal annotation node IDs the Java extractor emits via
// EmitAnnotationEdge. The resolver consumes these to discover
// temporal-tagged interfaces and methods.
const (
	javaActivityIfaceAnnoID = "annotation::java::ActivityInterface"
	javaWorkflowIfaceAnnoID = "annotation::java::WorkflowInterface"
	javaActivityMethodID    = "annotation::java::ActivityMethod"
	javaWorkflowMethodID    = "annotation::java::WorkflowMethod"
	javaSignalMethodID      = "annotation::java::SignalMethod"
	javaQueryMethodID       = "annotation::java::QueryMethod"
	javaUpdateMethodID      = "annotation::java::UpdateMethod"
)

// ResolveTemporalCalls is the graph-wide materialisation pass for the
// Temporal workflow → activity dispatch layer (N35). It performs two
// complementary jobs:
//
//  1. Role tagging. Stamps `temporal_role` (one of "workflow" /
//     "activity" / "activity_interface" / "workflow_interface" /
//     "signal" / "query" / "update") on every node the SDK treats as
//     a workflow / activity. Discovery uses two signals: (a) Go
//     `worker.RegisterActivity(F)` / `RegisterWorkflow(F)` calls,
//     emitted by the Go extractor as EdgeCalls edges carrying
//     `Meta["via"]="temporal.register"` and `Meta["temporal_name"]=<F>`;
//     (b) Java `@ActivityInterface` / `@WorkflowInterface` /
//     `@SignalMethod` / `@QueryMethod` / `@UpdateMethod` annotations,
//     emitted by the Java extractor as EdgeAnnotated edges to a
//     well-known synthetic annotation node. For Java interface
//     annotations the role is propagated to every implementor's
//     matching method via EdgeImplements + name match — that gives
//     queries a flat view of "every activity method in this codebase"
//     without re-walking the interface chain.
//
//  2. Stub-call resolution. Every Go `workflow.ExecuteActivity(ctx, F,
//     ...)` call is emitted as an EdgeCalls edge to a
//     `unresolved::temporal::<kind>::<name>` placeholder carrying
//     `Meta["via"]="temporal.stub"`. This pass rewrites each such edge
//     to point at the function the worker registered under that name.
//     The Java side is already resolved by normal interface dispatch
//     (`stub.someMethod()` is a call on a `@ActivityInterface` type;
//     the existing AST resolver lands it on the interface method, and
//     EdgeImplements connects to the impl); the role tag in step 1 is
//     the only extra surface Java needs.
//
// The pass is a full recompute and idempotent: every temporal.stub
// edge's target is recomputed from its own `temporal_name` meta on
// every call, so it is incremental-safe — a reindex of either the
// workflow or the activity file leaves the meta intact and the next
// pass re-lands (or un-lands) the edge. graph.ReindexEdge keeps the
// out/in buckets consistent. An edge whose target is no longer in the
// graph is reset back to the placeholder and loses its
// resolution-tier metadata.
//
// Runs at every resolver settle point that already runs InferImplements
// (so the Java interface → impl chain has its EdgeImplements edges)
// and after ResolveGRPCStubCalls (so the two SDK passes share the
// same post-condition).
//
// Returns the number of temporal.stub edges pointing at a resolved
// handler after the pass.
// argNameAt reads the positional arg name recorded on a call edge.
//
// PURPOSE — read the positional arg name recorded on a call edge by the extractor
// RATIONALE — arg_names can be []string (most paths) or []any (json-round-tripped)
// KEYWORDS — arg_names, wrapper-following, position
func argNameAt(e *graph.Edge, pos int) string {
	if e == nil || e.Meta == nil || pos < 0 {
		return ""
	}
	switch a := e.Meta["arg_names"].(type) {
	case []string:
		if pos < len(a) {
			return a[pos]
		}
	case []any:
		if pos < len(a) {
			if s, ok := a[pos].(string); ok {
				return s
			}
		}
	}
	return ""
}

// metaIntValue coerces an int-ish meta value to an int.
//
// PURPOSE — coerce various numeric representations of a position to int
// RATIONALE — meta values can be stored as int, int64, float64, or string depending on serialization
// KEYWORDS — position, coercion, param, wrapper-following
func metaIntValue(v any) (int, bool) {
	switch x := v.(type) {
	case int:
		return x, true
	case int64:
		return int(x), true
	case float64:
		return int(x), true
	case string:
		if n, err := strconv.Atoi(x); err == nil {
			return n, true
		}
	}
	return 0, false
}

// temporalWrapperStubExists is the idempotence guard for the wrapper pass.
//
// PURPOSE — prevent duplicate wrapper-synthesized stub edges on repeated resolver runs
// RATIONALE — resolveTemporalWrapperCalls runs on every settle; the guard is O(out-edges of caller)
// KEYWORDS — idempotence, temporal.stub, wrapper
func temporalWrapperStubExists(g graph.Store, from, kind, name string) bool {
	for _, e := range g.GetOutEdges(from) {
		if e == nil || e.Meta == nil {
			continue
		}
		if v, _ := e.Meta["via"].(string); v != "temporal.stub" {
			continue
		}
		if k, _ := e.Meta["temporal_kind"].(string); k != kind {
			continue
		}
		if n, _ := e.Meta["temporal_name"].(string); n == name {
			return true
		}
	}
	return false
}

// resolveTemporalWrapperCalls synthesises temporal.stub edges at callers of
// dispatch wrappers, propagating the caller's literal arg value through the
// wrapper's forwarded parameter.
//
// PURPOSE — synthesize temporal.stub edges at callers of wrapper functions that forward
//
//	a parameter as the dispatch name, propagating the caller's literal arg value
//
// RATIONALE — single-level pass: find edges WITH temporal_name_param, find their callers,
//
//	extract the arg at the wrapper's param position, emit a new stub
//
// KEYWORDS — wrapper-following, temporal.stub, arg_names, single-level
func resolveTemporalWrapperCalls(g graph.Store) {
	type wrapper struct {
		id, kind, name string
		pos            int
	}
	byID := map[string]wrapper{}
	byName := map[string][]wrapper{}

	for e := range g.EdgesByKind(graph.EdgeCalls) {
		if e == nil || e.Meta == nil || e.From == "" {
			continue
		}
		if v, _ := e.Meta["via"].(string); v != "temporal.stub" {
			continue
		}
		param, _ := e.Meta["temporal_name_param"].(string)
		kind, _ := e.Meta["temporal_kind"].(string)
		if param == "" || kind == "" {
			continue
		}
		if _, seen := byID[e.From]; seen {
			continue
		}
		pn := g.GetNode(e.From + "#param:" + param)
		if pn == nil {
			continue
		}
		pos, ok := metaIntValue(pn.Meta["position"])
		if !ok {
			continue
		}
		wname := ""
		if wnode := g.GetNode(e.From); wnode != nil {
			wname = wnode.Name
		}
		w := wrapper{id: e.From, kind: kind, name: wname, pos: pos}
		byID[e.From] = w
		if wname != "" {
			byName[wname] = append(byName[wname], w)
		}
	}
	if len(byID) == 0 {
		return
	}

	type pending struct {
		from, file, kind, name, wrapperName string
		line                                int
	}
	var out []pending
	emit := func(w wrapper, ce *graph.Edge) {
		if ce.From == w.id {
			return
		}
		name := argNameAt(ce, w.pos)
		if name == "" {
			return
		}
		out = append(out, pending{from: ce.From, file: ce.FilePath, line: ce.Line,
			kind: w.kind, name: name, wrapperName: w.name})
	}

	for ce := range g.EdgesByKind(graph.EdgeCalls) {
		if ce == nil || ce.From == "" || ce.Meta == nil {
			continue
		}
		if _, ok := ce.Meta["arg_names"]; !ok {
			continue
		}
		if w, ok := byID[ce.To]; ok {
			emit(w, ce)
			continue
		}
		callee, _ := ce.Meta["callee"].(string)
		if callee == "" {
			continue
		}
		for _, w := range byName[callee] {
			emit(w, ce)
		}
	}

	for _, p := range out {
		if temporalWrapperStubExists(g, p.from, p.kind, p.name) {
			continue
		}
		g.AddEdge(&graph.Edge{
			From: p.from, To: temporalStubPlaceholder(p.kind, p.name),
			Kind: graph.EdgeCalls, FilePath: p.file, Line: p.line,
			Meta: map[string]any{
				"via":                  "temporal.stub",
				"temporal_kind":        p.kind,
				"temporal_name":        p.name,
				"temporal_via_wrapper": p.wrapperName,
			},
		})
	}
}

func ResolveTemporalCalls(g graph.Store) int {
	if g == nil {
		return 0
	}
	// Serialise against other graph-wide passes that mutate Node.Meta
	// (markTestSymbolsAndEmitEdges, detectClonesAndEmitEdges,
	// reach.BuildIndex). stampTemporalRole below writes n.Meta on
	// existing graph nodes; without this lock a concurrent reader
	// (e.g. clone detection invoked from indexFile) trips the runtime's
	// "concurrent map read and map write" check.
	mu := g.ResolveMutex()
	mu.Lock()
	defer mu.Unlock()

	// Wrapper-following pre-pass: synthesise temporal.stub edges at callers of
	// wrapper functions that forward a parameter as the Temporal dispatch name.
	// Must run before the stub-collection sweep so the freshly synthesised stubs
	// are picked up and resolved by the existing loop below.
	resolveTemporalWrapperCalls(g)

	// Executor-field pre-pass: rewrite struct-field dispatch stubs to the
	// literal name supplied at the executor's construction site. Also runs
	// before the sweep so the rewritten stubs resolve below.
	resolveTemporalExecutorFields(g)

	// Single sweep over EdgeCalls — the largest edge class — collecting
	// both the temporal.register edges (index inputs) and the
	// temporal.stub edges (edges to resolve), instead of scanning it once
	// per concern. The From IDs of stub edges are gathered so the
	// per-edge caller lookup below collapses to one batch fetch.
	type stubEdge struct {
		edge       *graph.Edge
		kind, name string
	}
	var stubs []stubEdge
	var registerEdges []*graph.Edge
	fromIDSet := map[string]struct{}{}
	for e := range g.EdgesByKind(graph.EdgeCalls) {
		if e == nil || e.Meta == nil {
			continue
		}
		switch v, _ := e.Meta["via"].(string); v {
		case "temporal.register":
			registerEdges = append(registerEdges, e)
		case "temporal.stub", "temporal.start":
			// temporal.stub is a workflow→activity / workflow→child-workflow
			// dispatch; temporal.start is a service→workflow start
			// (client.ExecuteWorkflow / SignalWithStartWorkflow). Both
			// resolve the same way — rewrite to the registered handler /
			// workflow found by <kind>::<name>.
			kind, _ := e.Meta["temporal_kind"].(string)
			name, _ := e.Meta["temporal_name"].(string)
			if kind == "" || name == "" {
				continue
			}
			stubs = append(stubs, stubEdge{edge: e, kind: kind, name: name})
			if e.From != "" {
				fromIDSet[e.From] = struct{}{}
			}
		}
	}

	// Probe the (smaller) annotation class for Java temporal tags.
	var annotatedEdges []*graph.Edge
	for e := range g.EdgesByKind(graph.EdgeAnnotated) {
		if e == nil {
			continue
		}
		if r, m := temporalRoleForJavaAnnotation(e.To); r == "" && m == "" {
			continue
		}
		annotatedEdges = append(annotatedEdges, e)
	}

	// Early-out: a graph with no Temporal register / stub / annotation
	// edges (the common case for most repos) skips all node fetches,
	// index building, role stamping, and Java propagation entirely — the
	// pass costs only the two EdgesByKind scans above.
	if len(registerEdges) == 0 && len(stubs) == 0 && len(annotatedEdges) == 0 {
		return 0
	}

	idx := buildTemporalIndex(g, registerEdges, annotatedEdges)
	resolved := 0
	var reindexBatch []graph.EdgeReindex
	fromList := make([]string, 0, len(fromIDSet))
	for id := range fromIDSet {
		fromList = append(fromList, id)
	}
	callerNodes := g.GetNodesByIDs(fromList)

	// Const-dereference map: a dispatch named through a string const
	// (`const ChargeCardActivity = "ChargeCard"`) reaches the resolver as
	// the identifier "ChargeCardActivity"; map it to the literal value so
	// the lookup keys on the registered name. Built once from the
	// queryable constant_values sidecar.
	stubNames := make([]string, 0, len(stubs))
	for _, s := range stubs {
		stubNames = append(stubNames, s.name)
	}
	derefByName := buildConstDerefMap(g, stubNames)

	for _, s := range stubs {
		e := s.edge
		callerRepo := ""
		callerLang := ""
		if from := callerNodes[e.From]; from != nil {
			callerRepo = from.RepoPrefix
			callerLang = from.Language
		}
		handlerID, origin, conf := idx.lookup(s.kind, s.name, callerRepo, callerLang)
		// When the direct name didn't resolve, try dereferencing it as a
		// string constant and re-looking-up under the literal value.
		constDeref := ""
		if handlerID == "" {
			if v, ok := derefByName[s.name]; ok && v != "" {
				if hID, o, c := idx.lookup(s.kind, v, callerRepo, callerLang); hID != "" {
					handlerID, origin, conf = hID, o, c
					constDeref = v
				}
			}
		}
		// Cross-language join: a consumer (typically a temporal.start, e.g.
		// a Java service starting a Go workflow) with no same-language
		// handler is matched to a unique other-language candidate by
		// canonical name, at the speculative tier.
		crossLang := false
		if handlerID == "" {
			matchName := s.name
			if constDeref != "" {
				matchName = constDeref
			}
			if hID, ok := idx.lookupCrossLang(s.kind, matchName, callerLang); ok {
				handlerID = hID
				origin = graph.OriginSpeculative
				conf = temporalCrossLangConfidence
				crossLang = true
			}
		}

		// When the name came from an env-var-with-literal-default
		// variable, the value is a best-guess: land the resolved edge at
		// the speculative tier instead of ast_resolved.
		envDefault := false
		if v, _ := e.Meta["temporal_name_origin"].(string); v == "env_default" {
			envDefault = true
		}
		if handlerID != "" && envDefault {
			origin = graph.OriginSpeculative
			conf = temporalEnvDefaultConfidence
		}

		want := handlerID
		if want == "" {
			want = temporalStubPlaceholder(s.kind, s.name)
		}
		if e.To == want {
			if handlerID != "" {
				resolved++
			}
			continue
		}

		oldTo := e.To
		e.To = want
		if handlerID != "" {
			e.Origin = origin
			e.Confidence = conf
			e.ConfidenceLabel = graph.ConfidenceLabelFor(graph.EdgeCalls, conf)
			e.Meta["temporal_resolution"] = origin
			if envDefault || crossLang {
				e.Meta[graph.MetaSpeculative] = true
			}
			if crossLang {
				e.Meta["temporal_cross_lang"] = true
			} else {
				delete(e.Meta, "temporal_cross_lang")
			}
			if constDeref != "" {
				e.Meta["temporal_const_deref"] = constDeref
			} else {
				delete(e.Meta, "temporal_const_deref")
			}
			StampSynthesized(e, SynthTemporalStub)
			resolved++
		} else {
			e.Origin = ""
			e.Confidence = 0
			e.ConfidenceLabel = ""
			delete(e.Meta, "temporal_resolution")
			delete(e.Meta, graph.MetaSpeculative)
			delete(e.Meta, "temporal_const_deref")
			delete(e.Meta, "temporal_cross_lang")
			UnstampSynthesized(e)
		}
		reindexBatch = append(reindexBatch, graph.EdgeReindex{Edge: e, OldTo: oldTo})
	}
	if len(reindexBatch) > 0 {
		g.ReindexEdges(reindexBatch)
	}
	return resolved
}

// temporalStubPlaceholder is the canonical placeholder target for an
// unresolved Temporal stub call.
func temporalStubPlaceholder(kind, name string) string {
	return temporalStubPrefix + kind + "::" + name
}

// temporalIndex maps (kind, name) to candidate handler nodes plus the
// origin / confidence tier the resolver should stamp on the rewritten
// edge.
type temporalIndex struct {
	// byKindName maps "<kind>::<name>" → handler candidate nodes.
	byKindName map[string][]*graph.Node
}

func (idx *temporalIndex) lookup(kind, name, callerRepo, callerLang string) (id, origin string, confidence float64) {
	all := idx.byKindName[kind+"::"+name]
	if len(all) == 0 {
		return "", "", 0
	}
	// Language gate: a Temporal stub call resolves only within its own
	// language. The candidate set co-mingles Go register targets and Java
	// annotation-tagged methods under the same "<kind>::<name>" key with
	// no language tag, so without this gate a Go workflow.ExecuteActivity
	// stub could land on a Java method node when names collide and that
	// Java entry is the unique overall candidate (pickGoTemporalTarget
	// gates language only on the Go register-indexing path, not here). The
	// intentional Java→Go cross-language join is a separate, explicitly
	// cross-language pass, not this same-language stub resolver.
	cands := all
	if callerLang != "" {
		cands = cands[:0:0]
		for _, n := range all {
			if n.Language == callerLang {
				cands = append(cands, n)
			}
		}
		if len(cands) == 0 {
			return "", "", 0
		}
	}
	// Prefer same-repo, then unique overall.
	var sameRepo []*graph.Node
	for _, n := range cands {
		if callerRepo != "" && n.RepoPrefix == callerRepo {
			sameRepo = append(sameRepo, n)
		}
	}
	if len(sameRepo) == 1 {
		return sameRepo[0].ID, graph.OriginASTResolved, 0.9
	}
	if len(sameRepo) == 0 && len(cands) == 1 {
		return cands[0].ID, graph.OriginASTResolved, 0.9
	}
	return "", "", 0
}

// lookupCrossLang is the cross-language fallback for a Temporal consumer
// whose same-language lookup found no handler: it matches a candidate in a
// DIFFERENT language by canonical name (e.g. a Java service that starts a
// Go workflow, or vice-versa). The match is a by-string name across a
// type-system boundary with no compiler guarantee, so it resolves only
// when there is exactly ONE other-language candidate for the name — and
// the caller lands it at the speculative tier. Returns ("", false) when
// the join is absent or ambiguous.
func (idx *temporalIndex) lookupCrossLang(kind, name, callerLang string) (id string, ok bool) {
	all := idx.byKindName[kind+"::"+name]
	if len(all) == 0 || callerLang == "" {
		return "", false
	}
	var other []*graph.Node
	for _, n := range all {
		if n != nil && n.Language != callerLang {
			other = append(other, n)
		}
	}
	if len(other) == 1 {
		return other[0].ID, true
	}
	return "", false
}

// buildTemporalIndex (a) stamps temporal_role on every node identifiable
// as a Temporal workflow / activity via either Go `worker.Register*`
// calls or Java `@ActivityInterface` / `@WorkflowInterface` annotations
// (propagated to interface implementors), and (b) returns a name index
// the stub-call resolver consults.
//
// registerEdges and annotatedEdges are the temporal.register EdgeCalls
// edges and the temporal-annotation EdgeAnnotated edges, already
// collected by the single ResolveTemporalCalls sweep — passing them in
// avoids re-scanning the (largest) EdgeCalls class and the EdgeAnnotated
// class a second time.
func buildTemporalIndex(g graph.Store, registerEdges, annotatedEdges []*graph.Edge) *temporalIndex {
	idx := &temporalIndex{byKindName: map[string][]*graph.Node{}}

	// Phase 1 — Go side. Walk the pre-collected `temporal.register` edges
	// and stamp the registered function's node.
	//
	// Collect every register edge's targets first so we can batch-fetch
	// every caller node and resolve every Go target name in one pair of
	// round-trips, instead of N AllNodes scans + N GetNode calls.
	type goRegister struct {
		edge *graph.Edge
		kind string
		// name is the function-reference identifier (used to locate the
		// registered node); regName is the canonical registered name (the
		// index key) — they differ only when RegisterActivityWithOptions
		// overrides the name via RegisterOptions{Name: "..."}. For a plural
		// registration name is the struct TYPE name and regName is unused.
		name, regName string
		// plural marks a RegisterActivities(&Struct{}) struct registration:
		// every exported method of the struct is promoted to an activity.
		plural bool
	}
	var goRegisters []goRegister
	registerCallerIDs := map[string]struct{}{}
	registerNames := map[string]struct{}{}
	for _, e := range registerEdges {
		if e == nil || e.Meta == nil {
			continue
		}
		kind, _ := e.Meta["temporal_kind"].(string)
		name, _ := e.Meta["temporal_name"].(string)
		if kind == "" || name == "" {
			continue
		}
		regName, _ := e.Meta["temporal_registered_name"].(string)
		if regName == "" {
			regName = name
		}
		plural, _ := e.Meta["temporal_register_plural"].(bool)
		goRegisters = append(goRegisters, goRegister{edge: e, kind: kind, name: name, regName: regName, plural: plural})
		if e.From != "" {
			registerCallerIDs[e.From] = struct{}{}
		}
		registerNames[name] = struct{}{}
	}
	callerList := make([]string, 0, len(registerCallerIDs))
	for id := range registerCallerIDs {
		callerList = append(callerList, id)
	}
	registerCallers := g.GetNodesByIDs(callerList)
	nameList := make([]string, 0, len(registerNames))
	for n := range registerNames {
		nameList = append(nameList, n)
	}
	candidatesByName := g.FindNodesByNames(nameList)

	for _, r := range goRegisters {
		caller := registerCallers[r.edge.From]
		if caller == nil {
			continue
		}
		if r.plural {
			// RegisterActivities(&MyActivities{}): promote every exported
			// method of the struct to an activity keyed by its method name.
			typeNode := pickGoTypeNode(candidatesByName[r.name], caller)
			if typeNode == nil {
				continue
			}
			for _, m := range exportedGoMethodsOfType(g, typeNode) {
				stampTemporalRole(g, m, r.kind, m.Name)
				idx.byKindName[r.kind+"::"+m.Name] = append(idx.byKindName[r.kind+"::"+m.Name], m)
			}
			continue
		}
		target := pickGoTemporalTarget(candidatesByName[r.name], caller)
		if target == nil {
			continue
		}
		// Stamp + index under the canonical registered name (regName),
		// which is the func-ref name unless a RegisterOptions{Name}
		// override renamed it — that is the name a dispatch matches.
		stampTemporalRole(g, target, r.kind, r.regName)
		idx.byKindName[r.kind+"::"+r.regName] = append(idx.byKindName[r.kind+"::"+r.regName], target)
	}

	// Phase 2 — Java side. Walk the pre-collected temporal-annotation
	// `EdgeAnnotated` edges to find temporal-tagged interfaces and
	// methods. As with Phase 1, batch the From-side GetNode calls.
	type javaAnno struct {
		fromID                string
		ifaceRole, methodRole string
		args                  string // raw annotation inner-parens text
	}
	var javaAnnos []javaAnno
	annoFromIDs := map[string]struct{}{}
	for _, e := range annotatedEdges {
		if e == nil {
			continue
		}
		role, methodRole := temporalRoleForJavaAnnotation(e.To)
		if role == "" && methodRole == "" {
			continue
		}
		args, _ := e.Meta["args"].(string)
		javaAnnos = append(javaAnnos, javaAnno{fromID: e.From, ifaceRole: role, methodRole: methodRole, args: args})
		if e.From != "" {
			annoFromIDs[e.From] = struct{}{}
		}
	}
	annoFromList := make([]string, 0, len(annoFromIDs))
	for id := range annoFromIDs {
		annoFromList = append(annoFromList, id)
	}
	annoFromNodes := g.GetNodesByIDs(annoFromList)

	type javaIfaceTag struct {
		ifaceID    string
		role       string // "activity_interface" / "workflow_interface"
		namePrefix string // @ActivityInterface(namePrefix = "...")
	}
	var javaIfaces []javaIfaceTag
	for _, a := range javaAnnos {
		from := annoFromNodes[a.fromID]
		if from == nil {
			continue
		}
		// Method-level annotation: stamp + index under the canonical
		// Temporal name (explicit @XxxMethod(name=) > activity Capitalize >
		// bare method name) so it keys off the same string a matching Go
		// registration uses.
		if a.methodRole != "" && (from.Kind == graph.KindMethod || from.Kind == graph.KindFunction) {
			canonical := javaMethodCanonicalName(a.methodRole, from.Name, a.args)
			stampTemporalRole(g, from, a.methodRole, canonical)
			key := normaliseTemporalKind(a.methodRole) + "::" + canonical
			idx.byKindName[key] = append(idx.byKindName[key], from)
			continue
		}
		// Interface-level annotation: queue for the propagation pass.
		if a.ifaceRole != "" && from.Kind == graph.KindInterface {
			stampTemporalRole(g, from, a.ifaceRole, from.Name)
			javaIfaces = append(javaIfaces, javaIfaceTag{
				ifaceID:    from.ID,
				role:       a.ifaceRole,
				namePrefix: javaAnnotationStringArg(a.args, "namePrefix"),
			})
		}
	}

	// Phase 3 — Java propagation. For each tagged interface, find its
	// methods (flat nodes living in the same file, within the
	// interface's line range) and stamp them. Then walk EdgeImplements
	// from each implementor and tag its same-named methods.
	//
	// Build a single Java method index up front via NodesByKind, then
	// project it into the two views the propagation needs:
	//   - methodsByFile: file path → []*method (used for interface
	//     methods, which the Java extractor emits as flat
	//     <file>::<name> nodes whose StartLine sits inside the
	//     interface's line range).
	//   - methodsByReceiver: receiver class name → []*method (used for
	//     impl-class methods, which carry Meta["receiver"]).
	// One pass beats AllNodes() per interface.
	javaMethodsByFile, javaMethodsByReceiver := buildJavaMethodViews(g, len(javaIfaces))

	// Prefetch the interface nodes + the implementing-type nodes for
	// the entire iface set so the propagation loop never issues an
	// inline GetNode.
	ifaceIDs := make([]string, 0, len(javaIfaces))
	for _, t := range javaIfaces {
		ifaceIDs = append(ifaceIDs, t.ifaceID)
	}
	ifaceNodes := g.GetNodesByIDs(ifaceIDs)
	implTypeIDSet := map[string]struct{}{}
	implIDsByIface := map[string][]string{}
	for _, t := range javaIfaces {
		for _, ie := range g.GetInEdges(t.ifaceID) {
			if ie == nil || ie.Kind != graph.EdgeImplements {
				continue
			}
			implIDsByIface[t.ifaceID] = append(implIDsByIface[t.ifaceID], ie.From)
			if ie.From != "" {
				implTypeIDSet[ie.From] = struct{}{}
			}
		}
	}
	implTypeIDList := make([]string, 0, len(implTypeIDSet))
	for id := range implTypeIDSet {
		implTypeIDList = append(implTypeIDList, id)
	}
	implTypeNodes := g.GetNodesByIDs(implTypeIDList)

	for _, t := range javaIfaces {
		methodRole := "activity"
		if t.role == "workflow_interface" {
			methodRole = "workflow"
		}
		iface := ifaceNodes[t.ifaceID]
		if iface == nil {
			continue
		}
		// Canonical Temporal name for a method of this interface: a
		// workflow's type is the interface simple name; an activity's type
		// is its method name capitalized, with the @ActivityInterface
		// namePrefix prepended. Keyed the same for interface and impl
		// methods (same method name) so a dispatch lands on either.
		canonicalFor := func(m *graph.Node) string {
			if t.role == "workflow_interface" {
				return iface.Name
			}
			return t.namePrefix + capitalizeASCII(m.Name)
		}
		ifaceMethods := collectJavaInterfaceMethodsFromIndex(iface, javaMethodsByFile)
		for _, m := range ifaceMethods {
			canonical := canonicalFor(m)
			stampTemporalRole(g, m, methodRole, canonical)
			idx.byKindName[methodRole+"::"+canonical] = append(idx.byKindName[methodRole+"::"+canonical], m)
		}
		// Propagate to implementing classes' methods.
		implMethodNames := map[string]struct{}{}
		for _, m := range ifaceMethods {
			implMethodNames[m.Name] = struct{}{}
		}
		for _, implTypeID := range implIDsByIface[t.ifaceID] {
			implType := implTypeNodes[implTypeID]
			if implType == nil {
				continue
			}
			for _, m := range methodsOfJavaTypeFromIndex(implType, javaMethodsByReceiver) {
				if _, ok := implMethodNames[m.Name]; !ok {
					continue
				}
				canonical := canonicalFor(m)
				stampTemporalRole(g, m, methodRole, canonical)
				idx.byKindName[methodRole+"::"+canonical] = append(idx.byKindName[methodRole+"::"+canonical], m)
			}
		}
	}

	return idx
}

// temporalRoleForJavaAnnotation maps a Java annotation node ID to a
// (interface-role, method-role) pair. Only one is non-empty per
// annotation; the caller uses whichever fits the annotated node kind.
func temporalRoleForJavaAnnotation(annoID string) (ifaceRole, methodRole string) {
	switch annoID {
	case javaActivityIfaceAnnoID:
		return "activity_interface", ""
	case javaWorkflowIfaceAnnoID:
		return "workflow_interface", ""
	case javaActivityMethodID:
		return "", "activity"
	case javaWorkflowMethodID:
		return "", "workflow"
	case javaSignalMethodID:
		return "", "signal"
	case javaQueryMethodID:
		return "", "query"
	case javaUpdateMethodID:
		return "", "update"
	}
	return "", ""
}

// javaAnnotationStringArg extracts the value of a `key = "value"` argument
// from an annotation's raw inner-parens text (the EdgeAnnotated Meta
// "args"), e.g. javaAnnotationStringArg(`name = "ChargeCard"`, "name") ==
// "ChargeCard". Matched on a word boundary so a "name" lookup does not
// match "namePrefix". Returns "" when the key is absent or unquoted.
func javaAnnotationStringArg(args, key string) string {
	for i := 0; i+len(key) <= len(args); i++ {
		if args[i:i+len(key)] != key {
			continue
		}
		if i > 0 {
			if b := args[i-1]; b != ' ' && b != ',' && b != '(' {
				continue
			}
		}
		j := i + len(key)
		for j < len(args) && args[j] == ' ' {
			j++
		}
		if j >= len(args) || args[j] != '=' {
			continue
		}
		rest := args[j+1:]
		q := strings.IndexByte(rest, '"')
		if q < 0 {
			return ""
		}
		rest = rest[q+1:]
		end := strings.IndexByte(rest, '"')
		if end < 0 {
			return ""
		}
		return rest[:end]
	}
	return ""
}

// capitalizeASCII upper-cases the first rune of s (Temporal's Java SDK
// derives an activity's default type from the method name with the first
// letter capitalized).
func capitalizeASCII(s string) string {
	if s == "" {
		return s
	}
	r, size := utf8.DecodeRuneInString(s)
	return string(unicode.ToUpper(r)) + s[size:]
}

// javaMethodCanonicalName computes the canonical Temporal name a Java
// method-level annotation registers under, so the resolver keys it off the
// same string a matching Go registration would use:
//   - an explicit @XxxMethod(name = "...") always wins;
//   - an activity method defaults to its name with the first letter
//     capitalized (the Java SDK default activity type);
//   - signal / query / update / workflow methods default to the bare
//     method name (signal/query/update names match by string at runtime;
//     a workflow's type is usually the interface name, handled in Phase 3).
func javaMethodCanonicalName(role, methodName, args string) string {
	if explicit := javaAnnotationStringArg(args, "name"); explicit != "" {
		return explicit
	}
	if role == "activity" {
		return capitalizeASCII(methodName)
	}
	return methodName
}

// normaliseTemporalKind collapses the seven role tags down to the two
// kinds that drive stub-call lookup ("activity" / "workflow"). Signal
// / query / update handlers are workflow methods, not separate kinds.
func normaliseTemporalKind(role string) string {
	switch role {
	case "workflow", "signal", "query", "update":
		return "workflow"
	default:
		return "activity"
	}
}

// stampTemporalRole writes `temporal_role` and `temporal_name` into a
// node's Meta. Idempotent: re-stamping the same role is a no-op. When
// a previously-stamped node is re-stamped with a different role the
// new role wins (the resolver runs as a full recompute, so this lets
// the latest registration take precedence).
func stampTemporalRole(g graph.Store, n *graph.Node, role, name string) {
	if n == nil || role == "" {
		return
	}
	// Skip the write-back entirely when the role + name are already what
	// we would stamp. ResolveTemporalCalls is a full recompute that runs
	// on every incremental edit, so without this guard every Temporal-role
	// node is re-AddNode'd (a serialised single-row write on the sqlite
	// backend) on every pass even when nothing changed. The common steady
	// state — re-running the pass after an unrelated edit — then costs no
	// node writes at all.
	if cur, _ := n.Meta["temporal_role"].(string); cur == role {
		if name == "" {
			return
		}
		if curName, _ := n.Meta["temporal_name"].(string); curName == name {
			return
		}
	}
	if n.Meta == nil {
		n.Meta = map[string]any{}
	}
	n.Meta["temporal_role"] = role
	if name != "" {
		n.Meta["temporal_name"] = name
	}
	// Round-trip the stamp back through the store. On the in-memory
	// backend n is canonical so this is an idempotent re-insert; on disk
	// backends n is a per-call GetNode/AllNodes reconstruction,
	// so without the write-back temporal_role/temporal_name would be
	// discarded the moment this pass returns. ResolveTemporalCalls runs
	// from RunGlobalGraphPasses, which can execute after the bulk-load
	// buffer is flushed, so the in-place mutation is not otherwise
	// captured. Matches reach / coverage / blame / releases / churn.
	g.AddNode(n)
}

// pickGoTemporalTarget selects the Go function or method that a
// `worker.Register*(F)` call refers to from a name-matched candidate
// set. The register call lives at `caller`; the function `F` is
// either declared in the same file or imported. The search order is:
//
//  1. Same-file function whose name matches.
//  2. Same-repo function whose name matches.
//  3. Unique workspace-wide function whose name matches.
//
// Returns nil when no unambiguous match exists. The candidate list
// MUST be pre-filtered to Name == registered name (FindNodesByNames
// already does that); this helper applies the Go-kind and language
// gates plus the locality tie-break.
func pickGoTemporalTarget(candidates []*graph.Node, caller *graph.Node) *graph.Node {
	if caller == nil {
		return nil
	}
	var sameFile, sameRepo, all []*graph.Node
	for _, n := range candidates {
		if n == nil {
			continue
		}
		if n.Language != "go" {
			continue
		}
		if n.Kind != graph.KindFunction && n.Kind != graph.KindMethod {
			continue
		}
		all = append(all, n)
		if caller.RepoPrefix != "" && n.RepoPrefix == caller.RepoPrefix {
			sameRepo = append(sameRepo, n)
		}
		if n.FilePath == caller.FilePath {
			sameFile = append(sameFile, n)
		}
	}
	if len(sameFile) == 1 {
		return sameFile[0]
	}
	if len(sameRepo) == 1 {
		return sameRepo[0]
	}
	if len(all) == 1 {
		return all[0]
	}
	return nil
}

// pickGoTypeNode selects the Go type node a `RegisterActivities(&T{})`
// struct registration refers to, from a name-matched candidate set, using
// the same same-file → same-repo → unique-overall locality tie-break as
// pickGoTemporalTarget. Returns nil when no unambiguous Go type matches.
func pickGoTypeNode(candidates []*graph.Node, caller *graph.Node) *graph.Node {
	if caller == nil {
		return nil
	}
	var sameFile, sameRepo, all []*graph.Node
	for _, n := range candidates {
		if n == nil || n.Language != "go" {
			continue
		}
		if n.Kind != graph.KindType && n.Kind != graph.KindInterface {
			continue
		}
		all = append(all, n)
		if caller.RepoPrefix != "" && n.RepoPrefix == caller.RepoPrefix {
			sameRepo = append(sameRepo, n)
		}
		if n.FilePath == caller.FilePath {
			sameFile = append(sameFile, n)
		}
	}
	if len(sameFile) == 1 {
		return sameFile[0]
	}
	if len(sameRepo) == 1 {
		return sameRepo[0]
	}
	if len(all) == 1 {
		return all[0]
	}
	return nil
}

// exportedGoMethodsOfType returns the exported Go method nodes of a type,
// found via the EdgeMemberOf in-edges the Go extractor emits from each
// method to its receiver type. Used to promote every method of a
// RegisterActivities(&Struct{}) registration to a temporal activity.
func exportedGoMethodsOfType(g graph.Store, typeNode *graph.Node) []*graph.Node {
	if typeNode == nil {
		return nil
	}
	var memberIDs []string
	for _, ie := range g.GetInEdges(typeNode.ID) {
		if ie == nil || ie.Kind != graph.EdgeMemberOf || ie.From == "" {
			continue
		}
		memberIDs = append(memberIDs, ie.From)
	}
	if len(memberIDs) == 0 {
		return nil
	}
	members := g.GetNodesByIDs(memberIDs)
	var out []*graph.Node
	for _, id := range memberIDs {
		m := members[id]
		if m == nil || m.Language != "go" || m.Kind != graph.KindMethod {
			continue
		}
		if !isExportedGoName(m.Name) {
			continue
		}
		out = append(out, m)
	}
	return out
}

// isExportedGoName reports whether a Go identifier is exported (its first
// rune is an uppercase letter) — Temporal registers only exported methods
// of a struct passed to RegisterActivities.
func isExportedGoName(name string) bool {
	if name == "" {
		return false
	}
	r, _ := utf8.DecodeRuneInString(name)
	return unicode.IsUpper(r)
}

// buildConstDerefMap resolves the names of string constants used as
// Temporal dispatch identifiers to their literal values, read from the
// queryable constant_values sidecar. Returns name → value for every name
// that is a string const with a single unambiguous value across the
// workspace; a name with conflicting values in different files (e.g. the
// same const name defined twice with different literals) is dropped so a
// dereference is never a wrong guess. Returns nil when the backend does
// not implement ConstantValueReader.
func buildConstDerefMap(g graph.Store, names []string) map[string]string {
	reader, ok := g.(graph.ConstantValueReader)
	if !ok || len(names) == 0 {
		return nil
	}
	nameSet := make(map[string]struct{}, len(names))
	for _, n := range names {
		nameSet[n] = struct{}{}
	}
	uniq := make([]string, 0, len(nameSet))
	for n := range nameSet {
		uniq = append(uniq, n)
	}
	candByName := g.FindNodesByNames(uniq)
	idToName := map[string]string{}
	var constIDs []string
	for name, cands := range candByName {
		for _, n := range cands {
			if n == nil || (n.Kind != graph.KindConstant && n.Kind != graph.KindFunction && n.Kind != graph.KindMethod) {
				continue
			}
			constIDs = append(constIDs, n.ID)
			idToName[n.ID] = name
		}
	}
	if len(constIDs) == 0 {
		return nil
	}
	vals, err := reader.ConstantValuesByNodeIDs(constIDs)
	if err != nil || len(vals) == 0 {
		return nil
	}
	out := make(map[string]string, len(vals))
	ambiguous := map[string]struct{}{}
	for id, v := range vals {
		name := idToName[id]
		if name == "" || v == "" {
			continue
		}
		if existing, seen := out[name]; seen && existing != v {
			ambiguous[name] = struct{}{}
			continue
		}
		out[name] = v
	}
	for name := range ambiguous {
		delete(out, name)
	}
	return out
}

// buildJavaMethodViews materialises two indexes over every Java
// method node in the graph: methodsByFile groups nodes whose Meta has
// NO "receiver" (interface methods, per the Java extractor's
// convention); methodsByReceiver groups nodes whose Meta carries a
// non-empty receiver. One NodesByKind scan replaces the N AllNodes()
// passes the old collectJavaInterfaceMethods + methodsOfJavaType
// helpers ran inside the per-interface propagation loop.
//
// ifaceCount == 0 is a fast no-op; with no tagged interfaces the
// indexes are unused so we skip the scan.
func buildJavaMethodViews(g graph.Store, ifaceCount int) (map[string][]*graph.Node, map[string][]*graph.Node) {
	if ifaceCount == 0 {
		return nil, nil
	}
	methodsByFile := map[string][]*graph.Node{}
	methodsByReceiver := map[string][]*graph.Node{}
	for n := range g.NodesByKind(graph.KindMethod) {
		if n == nil || n.Language != "java" {
			continue
		}
		recv, _ := n.Meta["receiver"].(string)
		if recv == "" {
			methodsByFile[n.FilePath] = append(methodsByFile[n.FilePath], n)
		} else {
			methodsByReceiver[recv] = append(methodsByReceiver[recv], n)
		}
	}
	return methodsByFile, methodsByReceiver
}

// collectJavaInterfaceMethodsFromIndex returns the interface's method
// nodes — flat KindMethod nodes in the interface's file whose
// StartLine sits inside the interface's line range. Consumes the
// methodsByFile view built by buildJavaMethodViews so the scan is
// O(methods in this file) rather than O(every node).
func collectJavaInterfaceMethodsFromIndex(iface *graph.Node, methodsByFile map[string][]*graph.Node) []*graph.Node {
	if iface == nil {
		return nil
	}
	var out []*graph.Node
	for _, n := range methodsByFile[iface.FilePath] {
		if n.StartLine < iface.StartLine || (iface.EndLine > 0 && n.StartLine > iface.EndLine) {
			continue
		}
		out = append(out, n)
	}
	return out
}

// methodsOfJavaTypeFromIndex returns the method nodes whose
// Meta["receiver"] matches the type's name (or the receiver-suffix
// shape on the class node's ID). Consumes the methodsByReceiver view
// built by buildJavaMethodViews so the scan is O(methods of this
// receiver) rather than O(every node).
func methodsOfJavaTypeFromIndex(t *graph.Node, methodsByReceiver map[string][]*graph.Node) []*graph.Node {
	if t == nil {
		return nil
	}
	out := methodsByReceiver[t.Name]
	// Honour the legacy id-suffix tie-break: a class node's id is
	// `<filePath>::<ClassName>`; a method whose receiver matches that
	// trailing component is still a member even when the receiver
	// Meta carries a fully-qualified name.
	for recv, candidates := range methodsByReceiver {
		if recv == t.Name {
			continue
		}
		if !strings.HasSuffix(t.ID, "::"+recv) {
			continue
		}
		out = append(out, candidates...)
	}
	return out
}

// resolveTemporalExecutorFields rewrites the dispatch name of a method
// stub that reads a receiver field to the string literal the struct was
// constructed with at its (possibly remote) construction site.
//
// PURPOSE — when a struct method reads a field to dispatch an activity/workflow
// (e.g. `workflow.ExecuteActivity(ctx, e.ActivityName)`) and the struct was
// constructed with a string literal for that field
// (`ActivityExecutor{ActivityName: "ChargeCard"}`), this pass rewrites the
// method stub's `temporal_name` from the field name to that literal, so the
// main resolver sweep lands it on the registered handler. The dispatch happens
// IN the method, so the call edge stays anchored to the method (get_callers on
// the activity surfaces the dispatching method, not the construction site).
// RATIONALE — two-edge join: the method-stub edge carries (recvType, field)
// from the dispatch site; the executor-field marker edge carries
// (type, field, value) from the construction site. The join key is
// `recvType::fieldName`. The rewrite is re-derived from the marker edges on
// every pass (never relying on the prior pass's mutation surviving), so it is
// recompute-safe under the full-recompute contract of ResolveTemporalCalls:
// the parser re-emits the stub with `temporal_name=<field>` on reindex, and
// this pass re-applies the literal before the main sweep runs. A
// recvType::field with conflicting construction-site literals is left
// unresolved — same unique-or-nothing policy as the const-deref join.
// KEYWORDS — temporal, executor-field, resolver
func resolveTemporalExecutorFields(g graph.Store) {
	// Phase 1: collect the method-stub edges that read a receiver field,
	// grouped by `recvType::field`.
	type dispatch struct {
		stubs []*graph.Edge
	}
	byField := map[string]*dispatch{}
	for e := range g.EdgesByKind(graph.EdgeCalls) {
		if e == nil || e.Meta == nil {
			continue
		}
		if v, _ := e.Meta["via"].(string); v != "temporal.stub" {
			continue
		}
		field, _ := e.Meta["temporal_name_field"].(string)
		rtype, _ := e.Meta["temporal_recv_type"].(string)
		kind, _ := e.Meta["temporal_kind"].(string)
		if field == "" || rtype == "" || kind == "" {
			continue
		}
		key := rtype + "::" + field
		d := byField[key]
		if d == nil {
			d = &dispatch{}
			byField[key] = d
		}
		d.stubs = append(d.stubs, e)
	}
	if len(byField) == 0 {
		return
	}

	// Phase 2: for each executor-field marker edge, collect the literal
	// construction value per `recvType::field`. A key with conflicting
	// values across construction sites is ambiguous and dropped.
	valByField := map[string]string{}
	ambiguous := map[string]struct{}{}
	for e := range g.EdgesByKind(graph.EdgeCalls) {
		if e == nil || e.Meta == nil || e.From == "" {
			continue
		}
		if v, _ := e.Meta["via"].(string); v != "temporal.executor-field" {
			continue
		}
		rtype, _ := e.Meta["executor_type"].(string)
		field, _ := e.Meta["executor_field"].(string)
		value, _ := e.Meta["executor_value"].(string)
		if rtype == "" || field == "" || value == "" {
			continue
		}
		key := rtype + "::" + field
		if _, ok := byField[key]; !ok {
			continue
		}
		if existing, seen := valByField[key]; seen && existing != value {
			ambiguous[key] = struct{}{}
			continue
		}
		valByField[key] = value
	}
	for key := range ambiguous {
		delete(valByField, key)
	}

	// Phase 3: rewrite each matched method stub's dispatch name to the
	// construction literal. e.To is left for the main sweep to recompute
	// from the new temporal_name; temporal_name_field / temporal_recv_type
	// are preserved as the join key for the next full-recompute pass.
	for key, value := range valByField {
		d := byField[key]
		if d == nil {
			continue
		}
		for _, e := range d.stubs {
			e.Meta["temporal_name"] = value
			e.Meta["temporal_via_executor"] = true
		}
	}
}
