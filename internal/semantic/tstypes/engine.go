package tstypes

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/semantic"
)

// maxFileBytes guards the enrichment pass against pathological
// generated sources; files above the cap are skipped, same spirit as
// the indexer's own size gates.
const maxFileBytes = 4 << 20

// astConfidence is the confidence stamped on edges this engine
// confirms or adds. Deliberately below the 1.0 the compiler-grade
// ConfirmEdge uses: tree-sitter scope analysis is structurally
// grounded but not type-checked.
const astConfidence = 0.95

// inferredConfidence is the graded confidence stamped on edges this
// engine derives by a type heuristic rather than a direct structural
// match (e.g. a receiver type narrowed by inference). Honestly weaker
// than astConfidence, yet well above the name-only text-match floor.
// The edge still carries OriginASTResolved provenance — only the
// confidence and the resolution_strategy label distinguish it from the
// direct path.
const inferredConfidence = 0.7

// resolutionStrategy labels how the engine derived an edge it emits. It
// rides on Meta["resolution_strategy"] for graded (non-direct)
// emissions so consumers can see the inference path; the direct path
// carries no label. Extensible: later inference forms add their own
// constants.
type resolutionStrategy string

const (
	// strategyDirect is the default: a structurally grounded
	// tree-sitter resolution. Emitted at astConfidence with no
	// resolution_strategy label (its zero value is the empty string, so
	// the direct path stamps nothing extra).
	strategyDirect resolutionStrategy = ""
	// strategyInferred marks an edge derived by a type heuristic rather
	// than a direct scope match — emitted at inferredConfidence and
	// labelled so it stays honestly distinguishable from a direct
	// resolution.
	strategyInferred resolutionStrategy = "inferred"
)

// extendsWalkDepth bounds the inherited-method lookup walk up the
// resolved EdgeExtends chain.
const extendsWalkDepth = 3

// fileRef is one graph file node selected for analysis plus its
// on-disk location.
type fileRef struct {
	node    *graph.Node
	absPath string
}

// languageFiles selects the graph file nodes for the spec's languages
// that belong to the repo identified by repoPrefix and exist on disk
// under repoRoot.
//
// Disk existence alone is NOT a safe repo-membership test in multi-repo
// mode: the shared graph holds file nodes from every tracked repo, and
// two repos can share a relative path (both have `src/Svc.java`). Joining
// a foreign repo's node onto repoRoot would then stat-hit and read THIS
// repo's bytes for that repo's node, contaminating its graph. Selection is
// therefore gated on the node's own RepoPrefix matching the prefix of the
// repo being enriched. In single-repo mode every real node carries the
// empty prefix, so repoPrefix == "" selects them all.
func languageFiles(g graph.Store, spec *LangSpec, repoPrefix, repoRoot string) []fileRef {
	langs := make(map[string]bool, len(spec.Languages))
	for _, l := range spec.Languages {
		langs[l] = true
	}
	var out []fileRef
	for n := range g.NodesByKind(graph.KindFile) {
		if !langs[n.Language] || n.RepoPrefix != repoPrefix {
			continue
		}
		ref, ok := fileRefFor(n, repoRoot)
		if !ok {
			continue
		}
		out = append(out, ref)
	}
	return out
}

// fileRefFor maps a graph file node to its on-disk location under repoRoot
// (stripping the node's own RepoPrefix from the path) and reports whether
// it is an existing, in-cap regular file. The single point that turns a
// graph file key into bytes-on-disk for both the full and incremental
// passes.
func fileRefFor(n *graph.Node, repoRoot string) (fileRef, bool) {
	rel := n.FilePath
	if n.RepoPrefix != "" {
		rel = strings.TrimPrefix(rel, n.RepoPrefix+"/")
	}
	abs := filepath.Join(repoRoot, filepath.FromSlash(rel))
	if fi, err := os.Stat(abs); err != nil || fi.IsDir() || fi.Size() > maxFileBytes {
		return fileRef{}, false
	}
	return fileRef{node: n, absPath: abs}, true
}

// analyzeFile parses one file and runs the binder walk. Pure with
// respect to the graph — safe to fan out across workers.
func analyzeFile(spec *LangSpec, ref fileRef) (*fileFacts, error) {
	src, err := os.ReadFile(ref.absPath)
	if err != nil {
		return nil, err
	}
	grammar := spec.GrammarFor(ref.node.FilePath)
	if grammar == nil {
		return nil, nil
	}
	tree, err := parser.ParseFile(src, grammar)
	if err != nil {
		return nil, err
	}
	defer tree.Close()

	facts := &fileFacts{file: ref.node.FilePath, repoPrefix: ref.node.RepoPrefix}
	newBinder(spec, src, facts).run(tree.RootNode())
	return facts, nil
}

// resolvedAlias is a trait-use alias resolved against the graph: on the
// using type, `alias` routes to method `method`. When trait is non-nil
// the method is looked up on that specific trait; otherwise it is looked
// up on the using type's own inheritance closure.
type resolvedAlias struct {
	alias  string
	trait  *graph.Node
	method string
}

// applier owns every graph interaction of an enrichment pass. It runs
// single-goroutine so in-place edge mutations never race.
type applier struct {
	g            graph.Store
	spec         *LangSpec
	provider     string
	stampedNodes map[string]*graph.Node // collected for one AddBatch round-trip
	// aliases maps a using type's node ID to its trait-use alias
	// adaptations, built in the alias phase and consulted when a call's
	// method name is not a direct or inherited member.
	aliases map[string][]resolvedAlias
}

func newApplier(g graph.Store, spec *LangSpec, provider string) *applier {
	return &applier{
		g:            g,
		spec:         spec,
		provider:     provider,
		stampedNodes: make(map[string]*graph.Node),
		aliases:      make(map[string][]resolvedAlias),
	}
}

// receiverTypeKinds is the node-kind set a call receiver's type may
// resolve to — methods only hang off types and interfaces.
var receiverTypeKinds = map[graph.NodeKind]bool{
	graph.KindType:      true,
	graph.KindInterface: true,
}

// supertypeKinds returns the node-kind set declared supertypes may
// resolve to: the receiver default unless the spec widens it.
func (a *applier) supertypeKinds() map[graph.NodeKind]bool {
	if a.spec.SupertypeKinds != nil {
		return a.spec.SupertypeKinds
	}
	return receiverTypeKinds
}

// fileIndex is the per-file view of the graph the apply phase joins
// facts against.
type fileIndex struct {
	facts   *fileFacts
	imports map[string]string // local name → path hint
	types   map[string]*graph.Node
	// superTypes additionally holds same-file nodes of the spec's
	// widened supertype kinds (Ruby modules); aliases types when the
	// spec doesn't widen.
	superTypes map[string]*graph.Node
	funcs      []*graph.Node // function/method nodes, for line containment
}

func (a *applier) buildIndex(facts *fileFacts) *fileIndex {
	idx := &fileIndex{
		facts:   facts,
		imports: make(map[string]string, len(facts.imports)),
		types:   make(map[string]*graph.Node),
	}
	idx.superTypes = idx.types
	superKinds := a.supertypeKinds()
	if a.spec.SupertypeKinds != nil {
		idx.superTypes = make(map[string]*graph.Node)
	}
	for _, imp := range facts.imports {
		if imp.Local != "" {
			idx.imports[imp.Local] = imp.Path
		}
	}
	for _, n := range a.g.GetFileNodes(facts.file) {
		if receiverTypeKinds[n.Kind] {
			if _, dup := idx.types[n.Name]; !dup {
				idx.types[n.Name] = n
			}
		}
		if a.spec.SupertypeKinds != nil && superKinds[n.Kind] {
			if _, dup := idx.superTypes[n.Name]; !dup {
				idx.superTypes[n.Name] = n
			}
		}
		if n.Kind == graph.KindFunction || n.Kind == graph.KindMethod {
			idx.funcs = append(idx.funcs, n)
		}
	}
	return idx
}

// applyAll joins every analyzed file's facts against the graph in
// three phases: supertype edges and meta fills first, calls last —
// a call in one file may resolve through an extends edge (or a
// return_type stamp) another file's facts just synthesized.
func (a *applier) applyAll(all []*fileFacts, res *semantic.EnrichResult) {
	sort.Slice(all, func(i, j int) bool { return all[i].file < all[j].file })
	idxs := make([]*fileIndex, len(all))
	for i, facts := range all {
		idxs[i] = a.buildIndex(facts)
	}
	for i, facts := range all {
		for _, sf := range facts.supers {
			a.applySuper(idxs[i], sf, res)
		}
	}
	for i, facts := range all {
		for _, mf := range facts.metas {
			a.applyMeta(idxs[i], mf, res)
		}
	}
	for i, facts := range all {
		for _, af := range facts.aliases {
			a.applyAlias(idxs[i], af)
		}
	}
	for i, facts := range all {
		for _, cf := range facts.calls {
			a.applyCall(idxs[i], cf, res)
		}
	}
}

// flush round-trips the stamped nodes through the store in one batch —
// on disk backends an in-place Meta mutation is otherwise discarded.
func (a *applier) flush() {
	if len(a.stampedNodes) == 0 {
		return
	}
	nodes := make([]*graph.Node, 0, len(a.stampedNodes))
	for _, n := range a.stampedNodes {
		nodes = append(nodes, n)
	}
	a.g.AddBatch(nodes, nil)
}

// --- Type / method resolution ----------------------------------------

// resolveTypeNode grounds a bare type name to a graph type node:
// same-file declaration first, then import-hinted cross-file match,
// then a repo-unique name match. Returns nil when the name stays
// ambiguous — the engine never guesses among candidates.
func (a *applier) resolveTypeNode(idx *fileIndex, name string) *graph.Node {
	return a.resolveNodeOfKinds(idx, name, idx.types, receiverTypeKinds)
}

// resolveSuperNode is resolveTypeNode over the spec's supertype kind
// set — identical strategy, wider target kinds where the language
// needs it (Ruby modules).
func (a *applier) resolveSuperNode(idx *fileIndex, name string) *graph.Node {
	return a.resolveNodeOfKinds(idx, name, idx.superTypes, a.supertypeKinds())
}

func (a *applier) resolveNodeOfKinds(idx *fileIndex, name string, sameFile map[string]*graph.Node, kinds map[graph.NodeKind]bool) *graph.Node {
	if name == "" {
		return nil
	}
	if n, ok := sameFile[name]; ok {
		return n
	}
	candidates := a.typeCandidates(idx, name, kinds)
	if len(candidates) == 0 {
		return nil
	}
	if hint, ok := idx.imports[name]; ok && hint != "" {
		var matched []*graph.Node
		for _, c := range candidates {
			if importMatches(c.FilePath, c.RepoPrefix, hint, idx.facts.file) {
				matched = append(matched, c)
			}
		}
		if len(matched) == 1 {
			return matched[0]
		}
		// The hint named a definition site; when it matches several
		// candidates the receiver stays ambiguous, and when it matches
		// none the real target is an external / stdlib dependency the
		// graph doesn't hold. Either way the engine must not fall back
		// to a repo-local same-named type — that would mint a false edge
		// shadowing the dependency. A missing edge beats a wrong one.
		return nil
	}
	if len(candidates) == 1 {
		return candidates[0]
	}
	return nil
}

func (a *applier) typeCandidates(idx *fileIndex, name string, kinds map[graph.NodeKind]bool) []*graph.Node {
	var raw []*graph.Node
	if idx.facts.repoPrefix != "" {
		raw = a.g.FindNodesByNameInRepo(name, idx.facts.repoPrefix)
	} else {
		raw = a.g.FindNodesByName(name)
	}
	lang := a.languageSet()
	var out []*graph.Node
	for _, c := range raw {
		if !kinds[c.Kind] {
			continue
		}
		if !lang[c.Language] {
			continue
		}
		out = append(out, c)
	}
	return out
}

func (a *applier) languageSet() map[string]bool {
	set := make(map[string]bool, len(a.spec.Languages))
	for _, l := range a.spec.Languages {
		set[l] = true
	}
	return set
}

// importMatches reports whether a candidate definition file plausibly
// backs the import-path hint. Relative hints resolve against the
// importing file's directory; absolute (package-style) hints match as
// a path-segment suffix of the candidate's extension-less path.
func importMatches(candidateFile, candidatePrefix, hint, importerFile string) bool {
	cand := strings.TrimSuffix(candidateFile, filepath.Ext(candidateFile))
	if candidatePrefix != "" {
		cand = strings.TrimPrefix(cand, candidatePrefix+"/")
	}
	if strings.HasPrefix(hint, "./") || strings.HasPrefix(hint, "../") {
		base := importerFile
		if i := strings.LastIndex(base, "/"); i >= 0 {
			base = base[:i]
		} else {
			base = ""
		}
		resolved := filepath.ToSlash(filepath.Join(base, hint))
		return cand == resolved || cand == resolved+"/index"
	}
	hint = strings.Trim(hint, "/")
	if hint == "" {
		return false
	}
	// Package files: Python's __init__.py and Rust's mod.rs name the
	// directory, not the file.
	return pathSegSuffix(cand, hint) ||
		pathSegSuffix(cand, hint+"/__init__") ||
		pathSegSuffix(cand, hint+"/mod")
}

// pathSegSuffix reports whether want equals cand or a slash-aligned
// suffix of it.
func pathSegSuffix(cand, want string) bool {
	return cand == want || strings.HasSuffix(cand, "/"+want)
}

// methodOn resolves a method name against a type's member set,
// following resolved EdgeExtends links for inherited methods. Returns
// nil when the type (and its ancestry) declares zero or several
// same-named members — overload sets stay untouched rather than
// half-guessed.
func (a *applier) methodOn(typeNode *graph.Node, method string, depth int) *graph.Node {
	if typeNode == nil || depth > extendsWalkDepth {
		return nil
	}
	var fromIDs []string
	for _, e := range a.g.GetInEdges(typeNode.ID) {
		if e.Kind == graph.EdgeMemberOf {
			fromIDs = append(fromIDs, e.From)
		}
	}
	var matches []*graph.Node
	if len(fromIDs) > 0 {
		for _, n := range a.g.GetNodesByIDs(fromIDs) {
			if n.Kind == graph.KindMethod && n.Name == method {
				matches = append(matches, n)
			}
		}
	}
	switch len(matches) {
	case 1:
		return matches[0]
	case 0:
		// Climb every inheritance edge the spec recognises — the
		// supertype chain plus, where the language widens it, the
		// mixin / include edges that pull a module's methods in. A
		// member contributed by exactly one ancestor resolves; one
		// contributed by several distinct ancestors (ambiguous across
		// mixins or supers) stays unresolved rather than half-guessed.
		// The depth bound above guards diamonds and mutual mixins from
		// looping.
		inheritKinds := a.spec.inheritEdgeKinds()
		parentKinds := a.supertypeKinds()
		var found *graph.Node
		for _, e := range a.g.GetOutEdges(typeNode.ID) {
			if graph.IsUnresolvedTarget(e.To) || !edgeKindIn(e.Kind, inheritKinds) {
				continue
			}
			parent := a.g.GetNode(e.To)
			if parent == nil || !parentKinds[parent.Kind] {
				continue
			}
			m := a.methodOn(parent, method, depth+1)
			if m == nil {
				continue
			}
			if found != nil && found.ID != m.ID {
				return nil
			}
			found = m
		}
		return found
	}
	return nil
}

// edgeKindIn reports whether k is one of kinds.
func edgeKindIn(k graph.EdgeKind, kinds []graph.EdgeKind) bool {
	for _, want := range kinds {
		if k == want {
			return true
		}
	}
	return false
}

// callableReturnType resolves a bare callee name to its graph
// return_type: same-file declaration first, then a repo-unique
// function. The returned name is normalized to the bare type name.
func (a *applier) callableReturnType(idx *fileIndex, callee string) string {
	var match *graph.Node
	for _, n := range idx.funcs {
		if n.Name == callee {
			if match != nil {
				return "" // same-file overloads: ambiguous
			}
			match = n
		}
	}
	if match == nil {
		var raw []*graph.Node
		if idx.facts.repoPrefix != "" {
			raw = a.g.FindNodesByNameInRepo(callee, idx.facts.repoPrefix)
		} else {
			raw = a.g.FindNodesByName(callee)
		}
		lang := a.languageSet()
		for _, c := range raw {
			if c.Kind != graph.KindFunction && c.Kind != graph.KindMethod {
				continue
			}
			if !lang[c.Language] {
				continue
			}
			if match != nil {
				return ""
			}
			match = c
		}
	}
	if match == nil || match.Meta == nil {
		return ""
	}
	rt, _ := match.Meta["return_type"].(string)
	return a.spec.normalize(rt)
}

// enclosingCallable returns the innermost function/method node
// containing line.
func (idx *fileIndex) enclosingCallable(line int) *graph.Node {
	var best *graph.Node
	bestSize := int(^uint(0) >> 1)
	for _, n := range idx.funcs {
		if n.StartLine <= line && line <= n.EndLine {
			if size := n.EndLine - n.StartLine; size < bestSize {
				best = n
				bestSize = size
			}
		}
	}
	return best
}

// --- Call application -------------------------------------------------

func (a *applier) applyCall(idx *fileIndex, cf callFact, res *semantic.EnrichResult) {
	typeNode, inferred := a.callReceiverType(idx, &cf)
	if typeNode == nil {
		return
	}
	target := a.methodOn(typeNode, cf.method, 0)
	if target == nil {
		// A trait-use alias renames the member onto the using type; the
		// alias name is not a member of the type or its ancestry, so the
		// direct climb misses it. The alias map routes it through.
		target = a.resolveAlias(typeNode, cf.method)
	}
	if target == nil {
		return
	}
	caller := idx.enclosingCallable(cf.line)
	if caller == nil || caller.ID == target.ID {
		return
	}
	strategy, confidence := strategyDirect, astConfidence
	if inferred {
		// The receiver type was derived through a chained return-type
		// rewrite rather than a direct binding — grade the edge honestly.
		strategy, confidence = strategyInferred, inferredConfidence
	}
	a.upgradeOrCreateCall(caller, target, cf, idx.facts.file, res, strategy, confidence)
}

// callReceiverType resolves a call's receiver to a graph type node. The
// bool reports whether the type was derived by inference — a chained
// return-type rewrite — rather than a direct binding; an inferred
// receiver lands its call edge at the graded confidence band.
func (a *applier) callReceiverType(idx *fileIndex, cf *callFact) (*graph.Node, bool) {
	if cf.recvChain != nil {
		return a.chainReturnType(idx, cf.recvChain), true
	}
	recvType := cf.recvType
	if recvType == "" && cf.recvPendingCallee != "" {
		recvType = a.callableReturnType(idx, cf.recvPendingCallee)
	}
	if recvType != "" {
		return a.resolveTypeNode(idx, recvType), false
	}
	if cf.recvIdent != "" {
		// Static / type-qualified call: only when the identifier is a
		// real type in scope of this file's imports.
		return a.resolveTypeNode(idx, cf.recvIdent), false
	}
	return nil, false
}

// chainReturnType types the result of a method call standing in receiver
// position (`a.step().done()`): it resolves the inner receiver and method,
// reads the inner method's declared return type, and applies the fluent
// self / trait return rewrite so a trait method returning the trait type,
// called on a using class, types as that class. Returns nil when any link
// fails to ground — a missing edge beats a wrong one.
func (a *applier) chainReturnType(idx *fileIndex, inner *callFact) *graph.Node {
	recv, _ := a.callReceiverType(idx, inner)
	if recv == nil {
		return nil
	}
	m := a.methodOn(recv, inner.method, 0)
	if m == nil {
		m = a.resolveAlias(recv, inner.method)
	}
	if m == nil {
		return nil
	}
	return a.effectiveReturnType(idx, m, recv)
}

// effectiveReturnType resolves a method's declared return type to a graph
// type node, applying the fluent return rewrite: a method returning
// `self` / `static` types as the receiver, and a TRAIT method that
// returns its own trait name, reached through a using class, rebinds to
// that using class. Any other named return type resolves normally.
func (a *applier) effectiveReturnType(idx *fileIndex, m, receiver *graph.Node) *graph.Node {
	rt := a.spec.normalize(methodReturnTypeName(m))
	if rt == "" {
		return nil
	}
	if isSelfReturn(rt) {
		return receiver
	}
	// A trait method whose return type IS the trait itself, reached
	// through a class that uses the trait, fluently returns the using
	// class — rebind. Restricted to trait owners (Meta kind == "trait")
	// and to the case where the method was inherited (owner != receiver),
	// so a class method returning its own class is left to resolve
	// normally (it already lands on the right type).
	if owner := a.ownerType(m); owner != nil && owner.ID != receiver.ID &&
		isTraitNode(owner) && rt == owner.Name {
		return receiver
	}
	return a.resolveTypeNode(idx, rt)
}

// ownerType returns the type a method is a member of, following its
// EdgeMemberOf link; nil when the method has no resolved owner.
func (a *applier) ownerType(m *graph.Node) *graph.Node {
	for _, e := range a.g.GetOutEdges(m.ID) {
		if e.Kind == graph.EdgeMemberOf {
			if owner := a.g.GetNode(e.To); owner != nil {
				return owner
			}
		}
	}
	return nil
}

// applyAlias resolves one trait-use alias adaptation against the graph
// and records it under the using type's node ID for the call phase. A
// qualified alias whose trait cannot be resolved is dropped rather than
// guessed.
func (a *applier) applyAlias(idx *fileIndex, af aliasFact) {
	typeNode := idx.types[af.typeName]
	if typeNode == nil {
		return
	}
	var traitNode *graph.Node
	if af.trait != "" {
		if traitNode = a.resolveSuperNode(idx, af.trait); traitNode == nil {
			return
		}
	}
	a.aliases[typeNode.ID] = append(a.aliases[typeNode.ID], resolvedAlias{
		alias: af.alias, trait: traitNode, method: af.method,
	})
}

// resolveAlias routes a method name through a trait-use alias on the
// type: the aliased name resolves to the original trait member. Returns
// nil when no alias matches or the original member does not ground.
func (a *applier) resolveAlias(typeNode *graph.Node, method string) *graph.Node {
	for _, al := range a.aliases[typeNode.ID] {
		if al.alias != method {
			continue
		}
		owner := al.trait
		if owner == nil {
			owner = typeNode
		}
		if m := a.methodOn(owner, al.method, 0); m != nil {
			return m
		}
	}
	return nil
}

// upgradeOrCreateCall lands a grounded call resolution on the graph:
// confirm the edge when it already points at the target, claim a
// weaker-tier or still-unresolved edge at the same line, otherwise add
// a fresh edge. Edges that already carry compiler/AST-grade provenance
// pointing elsewhere are never overridden.
func (a *applier) upgradeOrCreateCall(caller, target *graph.Node, cf callFact, file string, res *semantic.EnrichResult, strategy resolutionStrategy, confidence float64) {
	outs := a.g.GetOutEdges(caller.ID)
	for _, e := range outs {
		if e.Kind == graph.EdgeCalls && e.To == target.ID {
			if a.confirmCall(e, strategy, confidence) {
				res.EdgesConfirmed++
			}
			return
		}
	}
	for _, e := range outs {
		if e.Kind != graph.EdgeCalls || e.Line != cf.line {
			continue
		}
		if !trailingNameMatches(e.To, cf.method) {
			continue
		}
		if !a.claimable(e) {
			// A same-line edge for this name already carries
			// equal-or-stronger evidence for a different target —
			// leave it alone and don't double the call site.
			return
		}
		oldTo := e.To
		e.To = target.ID
		a.g.ReindexEdge(e, oldTo)
		a.confirmCall(e, strategy, confidence)
		res.EdgesConfirmed++
		return
	}
	a.addASTEdge(caller.ID, target.ID, graph.EdgeCalls, file, cf.line, strategy, confidence)
	res.EdgesAdded++
}

// confirmCall lands the provenance of a resolved call edge at the band
// the resolution earned: the direct path raises it to the AST ceiling
// (confirmAST), the inferred path stamps the graded band and the
// resolution_strategy label without ever downgrading a stronger edge.
func (a *applier) confirmCall(e *graph.Edge, strategy resolutionStrategy, confidence float64) bool {
	if strategy == strategyDirect {
		return a.confirmAST(e)
	}
	return a.confirmInferred(e, confidence)
}

// confirmInferred stamps the graded inferred band on an edge the engine
// resolved by a return-type rewrite: OriginASTResolved provenance at the
// honest inferred confidence, the inferred resolution_strategy label,
// and the provider — never downgrading an edge that already carries
// stronger provenance or a higher confidence.
func (a *applier) confirmInferred(e *graph.Edge, confidence float64) bool {
	if graph.OriginRank(effectiveOrigin(e)) > graph.OriginRank(graph.OriginASTResolved) {
		return false
	}
	changed := false
	if effectiveOrigin(e) != graph.OriginASTResolved {
		a.g.SetEdgeProvenance(e, graph.OriginASTResolved)
		changed = true
	}
	if e.Meta == nil {
		e.Meta = make(map[string]any)
	}
	if s, _ := e.Meta["semantic_source"].(string); s == "" {
		e.Meta["semantic_source"] = a.provider
		changed = true
	}
	if rs, _ := e.Meta["resolution_strategy"].(string); rs == "" {
		e.Meta["resolution_strategy"] = string(strategyInferred)
		changed = true
	}
	if e.Confidence < confidence {
		e.Confidence = confidence
		e.ConfidenceLabel = graph.ConfidenceLabelFor(e.Kind, confidence)
		changed = true
	}
	if changed {
		a.persistEdgeRow(e)
	}
	return changed
}

// methodReturnTypeName returns a method node's declared return type as
// recorded in Meta["return_type"], "" when absent.
func methodReturnTypeName(m *graph.Node) string {
	if m == nil || m.Meta == nil {
		return ""
	}
	rt, _ := m.Meta["return_type"].(string)
	return rt
}

// isTraitNode reports whether a type node was extracted as a trait
// (Meta kind == "trait"), the marker the PHP extractor stamps.
func isTraitNode(n *graph.Node) bool {
	if n == nil || n.Meta == nil {
		return false
	}
	k, _ := n.Meta["kind"].(string)
	return k == "trait"
}

// isSelfReturn reports whether a normalized return type names the
// receiver's own type — the fluent self / late-static-binding forms.
func isSelfReturn(t string) bool {
	switch t {
	case "self", "static", "$this", "this":
		return true
	}
	return false
}

// --- Supertype application --------------------------------------------

func (a *applier) applySuper(idx *fileIndex, sf superFact, res *semantic.EnrichResult) {
	typeNode, ok := idx.superTypes[sf.typeName]
	if !ok {
		return
	}
	superNode := a.resolveSuperNode(idx, sf.superName)
	if superNode == nil || superNode.ID == typeNode.ID {
		return
	}
	kind := sf.kind
	if kind == "" {
		// Syntax didn't discriminate (C# base list): the resolved
		// target's node kind decides.
		if superNode.Kind == graph.KindInterface {
			kind = graph.EdgeImplements
		} else {
			kind = graph.EdgeExtends
		}
	}
	outs := a.g.GetOutEdges(typeNode.ID)
	for _, e := range outs {
		if e.Kind == kind && e.To == superNode.ID {
			if a.confirmAST(e) {
				res.EdgesConfirmed++
			}
			return
		}
	}
	for _, e := range outs {
		if e.Kind != graph.EdgeExtends && e.Kind != graph.EdgeImplements {
			continue
		}
		if !a.claimable(e) || !trailingNameMatches(e.To, sf.superName) {
			continue
		}
		if e.Kind == kind {
			// Same relation kind, only the target changes — an in-place
			// retarget + ReindexEdge is safe because the edge's logical
			// key (which folds Kind) keeps the same Kind on both sides.
			oldTo := e.To
			e.To = superNode.ID
			a.g.ReindexEdge(e, oldTo)
			a.confirmAST(e)
			res.EdgesConfirmed++
			return
		}
		// The relation kind itself changes (a C#-style base list whose
		// member turned out to be an interface, not a base class).
		// Mutating Kind in place corrupts the adjacency index: ReindexEdge
		// reconstructs the old logical key from the already-mutated Kind,
		// so the original entry is never removed — the in-memory store
		// leaks a stale index slot and the sqlite store ends up with two
		// contradictory rows. Drop the old edge and add a fresh one of the
		// correct kind instead, mirroring how the compiler-grade providers
		// only ever add new edges rather than flip an existing one's kind.
		a.g.RemoveEdge(e.From, e.To, e.Kind)
		a.addASTEdge(typeNode.ID, superNode.ID, kind, idx.facts.file, sf.line, strategyDirect, astConfidence)
		res.EdgesAdded++
		return
	}
	a.addASTEdge(typeNode.ID, superNode.ID, kind, idx.facts.file, sf.line, strategyDirect, astConfidence)
	res.EdgesAdded++
}

// --- Node meta application --------------------------------------------

func (a *applier) applyMeta(idx *fileIndex, mf metaFact, res *semantic.EnrichResult) {
	var node *graph.Node
	if mf.owner != "" {
		node = a.findMember(idx, mf.owner, mf.name)
	} else if mf.line > 0 {
		node = idx.enclosingCallable(mf.line)
		if node != nil && node.StartLine != mf.line {
			node = nil
		}
	}
	if node == nil {
		return
	}
	if node.Meta != nil {
		if existing, ok := node.Meta[mf.key].(string); ok && existing != "" {
			return // never overwrite an existing (possibly stronger) stamp
		}
	}
	semantic.EnrichNodeMeta(node, mf.key, mf.value, a.provider)
	a.stampedNodes[node.ID] = node
	res.NodesEnriched++
}

// findMember locates the field/variable node for owner.name in the
// file (extractor convention: Meta["receiver"] carries the owner).
func (a *applier) findMember(idx *fileIndex, owner, name string) *graph.Node {
	for _, n := range a.g.GetFileNodes(idx.facts.file) {
		if n.Name != name {
			continue
		}
		if n.Kind != graph.KindField && n.Kind != graph.KindVariable {
			continue
		}
		if recv, _ := n.Meta["receiver"].(string); recv == owner {
			return n
		}
	}
	return nil
}

// --- Edge provenance helpers -------------------------------------------

// confirmAST stamps tree-sitter-grade provenance on an edge the engine
// grounded: OriginASTResolved (deliberately NOT the lsp_* tiers —
// these resolutions are scope-grounded but not compiler-verified),
// confidence raised to the AST ceiling, and the provider recorded as
// semantic_source. Never downgrades an edge that already carries
// AST-or-better provenance; returns whether anything changed.
func (a *applier) confirmAST(e *graph.Edge) bool {
	// Never downgrade. The comparison runs against the EFFECTIVE origin,
	// which backfills legacy edges that carry their compiler-grade
	// provenance only in Meta["semantic_source"] (Origin unset). Requiring
	// a non-empty Origin here would wrongly let those edges through and
	// clobber both their tier and their semantic_source — so the only
	// gate is the effective-rank comparison.
	if graph.OriginRank(effectiveOrigin(e)) >= graph.OriginRank(graph.OriginASTResolved) {
		// Origin is already AST-or-better — never downgrade it. But an edge the
		// extractor emitted carries OriginASTResolved with NO semantic_source
		// (e.g. an AST-level extends/implements reference form); the engine
		// grounded this relation, so still credit the provider and raise
		// confidence to the AST ceiling when those are missing, without
		// touching the origin/tier.
		changed := false
		if e.Meta == nil {
			e.Meta = make(map[string]any)
		}
		if s, _ := e.Meta["semantic_source"].(string); s == "" {
			e.Meta["semantic_source"] = a.provider
			changed = true
		}
		if e.Confidence < astConfidence {
			e.Confidence = astConfidence
			e.ConfidenceLabel = graph.ConfidenceLabelFor(e.Kind, e.Confidence)
			changed = true
		}
		if changed {
			a.persistEdgeRow(e)
		}
		return changed
	}
	a.persistConfirmedAST(e)
	return true
}

// persistConfirmedAST stamps the AST-grade provenance bundle (origin,
// confidence, label, semantic_source) on e and makes it durable on every
// backend. SetEdgeProvenance only writes origin+tier; on a disk backend e
// is a detached row copy, so the confidence / label / Meta mutations would
// be lost unless the full edge is round-tripped — persistEdgeRow does that
// through the backend's edge-attribute write path.
func (a *applier) persistConfirmedAST(e *graph.Edge) {
	a.g.SetEdgeProvenance(e, graph.OriginASTResolved)
	if e.Confidence < astConfidence {
		e.Confidence = astConfidence
	}
	e.ConfidenceLabel = graph.ConfidenceLabelFor(e.Kind, e.Confidence)
	if e.Meta == nil {
		e.Meta = make(map[string]any)
	}
	e.Meta["semantic_source"] = a.provider
	a.persistEdgeRow(e)
}

// persistEdgeRow makes a confirmed edge's full attribute bundle durable.
// On the in-memory backend GetOutEdges returns the live *Edge pointer, so
// the field mutations are already persisted and this is a no-op. A disk
// backend returns a detached row copy; SetEdgeProvenance only wrote
// origin+tier, so the confidence / label / Meta mutations need an explicit
// round-trip through the backend's edge-attribute write path.
func (a *applier) persistEdgeRow(e *graph.Edge) {
	if w, ok := a.g.(graph.EdgePersister); ok {
		w.PersistEdgeAttributes(e)
	}
}

// addASTEdge mints an AST-grade resolution edge. The default direct
// path (strategyDirect, astConfidence) keeps the structurally-grounded
// confidence and carries no resolution_strategy label — its callers
// have already arbitrated the edge state before reaching here, so it
// adds unconditionally exactly as before. A graded path (e.g.
// strategyInferred, inferredConfidence) emits the same OriginASTResolved
// provenance at a lower, honest confidence and stamps
// Meta["resolution_strategy"] with its label. A graded emission never
// clobbers or downgrades a pre-existing equal-or-stronger edge on the
// same (from,to,kind): on contention the stronger edge is returned
// untouched.
func (a *applier) addASTEdge(from, to string, kind graph.EdgeKind, file string, line int, strategy resolutionStrategy, confidence float64) *graph.Edge {
	if strategy != strategyDirect {
		if existing := a.strongerEdge(from, to, kind, confidence); existing != nil {
			return existing
		}
	}
	e := &graph.Edge{
		From:            from,
		To:              to,
		Kind:            kind,
		FilePath:        file,
		Line:            line,
		Confidence:      confidence,
		ConfidenceLabel: graph.ConfidenceLabelFor(kind, confidence),
		Origin:          graph.OriginASTResolved,
		Meta: map[string]any{
			"semantic_source": a.provider,
		},
	}
	if strategy != strategyDirect {
		e.Meta["resolution_strategy"] = string(strategy)
	}
	a.g.AddEdge(e)
	return e
}

// strongerEdge returns an existing (from->to, kind) edge whose
// provenance outranks the AST-grade origin a graded emission would
// stamp — or whose confidence is equal-or-higher at the same rank — so
// a lower-confidence inferred edge yields to it instead of downgrading
// it. Returns nil when no such edge exists. Graded emissions stay at
// OriginASTResolved, so the rank floor is that tier: a pre-existing LSP
// edge (higher rank) or a direct AST edge (same rank, higher
// confidence) both win.
func (a *applier) strongerEdge(from, to string, kind graph.EdgeKind, confidence float64) *graph.Edge {
	gradedRank := graph.OriginRank(graph.OriginASTResolved)
	for _, e := range a.g.GetOutEdges(from) {
		if e.Kind != kind || e.To != to {
			continue
		}
		rank := graph.OriginRank(effectiveOrigin(e))
		if rank > gradedRank {
			return e
		}
		if rank == gradedRank && e.Confidence >= confidence {
			return e
		}
	}
	return nil
}

// claimable reports whether the engine may rewire this edge's target:
// still-unresolved / external stub targets always are; resolved
// targets only when their effective provenance ranks below AST-grade
// (a name-locality guess this engine's type evidence outranks).
func (a *applier) claimable(e *graph.Edge) bool {
	if isStubTarget(e.To) {
		return true
	}
	return graph.OriginRank(effectiveOrigin(e)) < graph.OriginRank(graph.OriginASTResolved)
}

// effectiveOrigin returns the edge's provenance tier, backfilling the
// legacy default for edges minted before Origin stamping.
func effectiveOrigin(e *graph.Edge) string {
	if e.Origin != "" {
		return e.Origin
	}
	sem := ""
	if e.Meta != nil {
		sem, _ = e.Meta["semantic_source"].(string)
	}
	return graph.DefaultOriginFor(e.Kind, e.Confidence, sem)
}

func isStubTarget(to string) bool {
	if graph.IsUnresolvedTarget(to) {
		return true
	}
	for _, p := range []string{"external::", "stdlib::", "dep::"} {
		if strings.HasPrefix(to, p) || strings.Contains(to, "::"+p) {
			return true
		}
	}
	return false
}

// trailingNameMatches reports whether a target id's final name segment
// equals name — across the unresolved / stub / resolved id shapes
// (`unresolved::*.m`, `unresolved::m`, `a/b.go::T.m`).
func trailingNameMatches(to, name string) bool {
	if name == "" {
		return false
	}
	s := to
	if i := strings.LastIndex(s, "::"); i >= 0 {
		s = s[i+2:]
	}
	if i := strings.LastIndex(s, "."); i >= 0 {
		s = s[i+1:]
	}
	return s == name
}
