package resolver

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// temporalStubPrefix is the placeholder namespace the Go extractor
// emits for a Temporal workflow → activity (or workflow → child
// workflow) dispatch it can't land locally
// (`unresolved::temporal::<kind>::<name>`).
const temporalStubPrefix = unresolvedPrefix + "temporal::"

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
	idx := buildTemporalIndex(g)
	resolved := 0
	var reindexBatch []graph.EdgeReindex
	// First sweep: collect stub edges and the From IDs we need so the
	// per-edge GetNode below collapses to one batch lookup.
	type stubEdge struct {
		edge       *graph.Edge
		kind, name string
	}
	var stubs []stubEdge
	fromIDSet := map[string]struct{}{}
	for e := range g.EdgesByKind(graph.EdgeCalls) {
		if e == nil || e.Meta == nil {
			continue
		}
		if v, _ := e.Meta["via"].(string); v != "temporal.stub" {
			continue
		}
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
	fromList := make([]string, 0, len(fromIDSet))
	for id := range fromIDSet {
		fromList = append(fromList, id)
	}
	callerNodes := g.GetNodesByIDs(fromList)

	for _, s := range stubs {
		e := s.edge
		callerRepo := ""
		if from := callerNodes[e.From]; from != nil {
			callerRepo = from.RepoPrefix
		}
		handlerID, origin, conf := idx.lookup(s.kind, s.name, callerRepo)

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
			StampSynthesized(e, SynthTemporalStub)
			resolved++
		} else {
			e.Origin = ""
			e.Confidence = 0
			e.ConfidenceLabel = ""
			delete(e.Meta, "temporal_resolution")
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

func (idx *temporalIndex) lookup(kind, name, callerRepo string) (id, origin string, confidence float64) {
	cands := idx.byKindName[kind+"::"+name]
	if len(cands) == 0 {
		return "", "", 0
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

// buildTemporalIndex walks the graph once and (a) stamps temporal_role
// on every node identifiable as a Temporal workflow / activity via
// either Go `worker.Register*` calls or Java `@ActivityInterface` /
// `@WorkflowInterface` annotations (propagated to interface
// implementors), and (b) returns a name index the stub-call resolver
// consults.
func buildTemporalIndex(g graph.Store) *temporalIndex {
	idx := &temporalIndex{byKindName: map[string][]*graph.Node{}}

	// Phase 1 — Go side. Walk `temporal.register` edges and stamp the
	// registered function's node. The "via" tag lives on EdgeCalls
	// edges, so narrow with EdgesByKind before the Meta filter.
	//
	// Collect every register edge first so we can batch-fetch every
	// caller node and resolve every Go target name in one pair of
	// round-trips, instead of N AllNodes scans + N GetNode calls.
	type goRegister struct {
		edge       *graph.Edge
		kind, name string
	}
	var goRegisters []goRegister
	registerCallerIDs := map[string]struct{}{}
	registerNames := map[string]struct{}{}
	for e := range g.EdgesByKind(graph.EdgeCalls) {
		if e == nil || e.Meta == nil {
			continue
		}
		if v, _ := e.Meta["via"].(string); v != "temporal.register" {
			continue
		}
		kind, _ := e.Meta["temporal_kind"].(string)
		name, _ := e.Meta["temporal_name"].(string)
		if kind == "" || name == "" {
			continue
		}
		goRegisters = append(goRegisters, goRegister{edge: e, kind: kind, name: name})
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
		target := pickGoTemporalTarget(candidatesByName[r.name], caller)
		if target == nil {
			continue
		}
		stampTemporalRole(g, target, r.kind, r.name)
		idx.byKindName[r.kind+"::"+r.name] = append(idx.byKindName[r.kind+"::"+r.name], target)
	}

	// Phase 2 — Java side. Walk `EdgeAnnotated` edges to find
	// temporal-tagged interfaces and methods. As with Phase 1, collect
	// every annotation edge and batch the From-side GetNode calls.
	type javaAnno struct {
		fromID                string
		ifaceRole, methodRole string
	}
	var javaAnnos []javaAnno
	annoFromIDs := map[string]struct{}{}
	for e := range g.EdgesByKind(graph.EdgeAnnotated) {
		if e == nil {
			continue
		}
		role, methodRole := temporalRoleForJavaAnnotation(e.To)
		if role == "" && methodRole == "" {
			continue
		}
		javaAnnos = append(javaAnnos, javaAnno{fromID: e.From, ifaceRole: role, methodRole: methodRole})
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
		ifaceID string
		role    string // "activity_interface" / "workflow_interface"
	}
	var javaIfaces []javaIfaceTag
	for _, a := range javaAnnos {
		from := annoFromNodes[a.fromID]
		if from == nil {
			continue
		}
		// Method-level annotation: stamp directly.
		if a.methodRole != "" && (from.Kind == graph.KindMethod || from.Kind == graph.KindFunction) {
			stampTemporalRole(g, from, a.methodRole, from.Name)
			idx.byKindName[normaliseTemporalKind(a.methodRole)+"::"+from.Name] = append(
				idx.byKindName[normaliseTemporalKind(a.methodRole)+"::"+from.Name], from)
			continue
		}
		// Interface-level annotation: queue for the propagation pass.
		if a.ifaceRole != "" && from.Kind == graph.KindInterface {
			stampTemporalRole(g, from, a.ifaceRole, from.Name)
			javaIfaces = append(javaIfaces, javaIfaceTag{ifaceID: from.ID, role: a.ifaceRole})
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
		ifaceMethods := collectJavaInterfaceMethodsFromIndex(iface, javaMethodsByFile)
		for _, m := range ifaceMethods {
			stampTemporalRole(g, m, methodRole, m.Name)
			idx.byKindName[methodRole+"::"+m.Name] = append(idx.byKindName[methodRole+"::"+m.Name], m)
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
				stampTemporalRole(g, m, methodRole, m.Name)
				idx.byKindName[methodRole+"::"+m.Name] = append(idx.byKindName[methodRole+"::"+m.Name], m)
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
