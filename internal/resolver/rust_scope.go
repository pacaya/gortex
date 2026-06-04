package resolver

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// ResolveRustScopeCalls is the graph-wide materialisation pass for the
// Rust-specific scope layer. It lands Rust call edges the generic
// resolver leaves unresolved by applying Rust's own scoping rules:
//
//  1. impl-block method owner. `Foo::new()` resolves to the `new`
//     method defined in `impl Foo { fn new(...) }`. The Rust extractor
//     stamps the full scoped path on the call edge as Meta["rust_path"]
//     ("Foo::new"); this pass reads the qualifier ("Foo") and binds the
//     trailing segment to a method whose owner type (Node.Meta
//     ["receiver"]) is that qualifier. Resolved at ast_resolved — the
//     receiver type is named in source, so the binding is structurally
//     unambiguous within the qualifier's type.
//
//  2. self / Self receiver. Inside `impl Foo`, `self.bar()` and
//     `Self::new()` resolve to Foo's methods. The caller is a Rust
//     method node carrying Meta["receiver"]="Foo", so the enclosing
//     impl type is read off the caller and the call binds to a method
//     of that owner. Resolved at ast_resolved.
//
//  3. module-path. `crate::module::func()`, `super::func()`,
//     `self::func()` and `module::func()` resolve to a free function
//     named by the path's trailing segment. Gortex does not model the
//     Rust module tree as graph nodes, so the binding matches the
//     trailing segment against free functions in the caller's repo,
//     preferring a same-file then same-directory candidate. Resolved at
//     ast_inferred — the module prefix is not verified against a real
//     module node, only the trailing name and locality.
//
// Local-shadows-import precedence: before binding a module-path call to
// a free function, the pass checks whether the caller has a parameter
// of the same name (a local binding that, in Rust, shadows an imported
// item of the same identifier). When it does, the call is left
// unresolved rather than bound to the (shadowed) import target.
//
// The pass only ever rewrites an edge whose target is still an
// `unresolved::` placeholder, so it never fights or overrides a binding
// the generic resolver already landed; it strictly fills in the
// residual the generic pass missed. It is a full recompute and
// idempotent — each candidate edge's target is recomputed from its own
// Meta on every run, so a reindex of either endpoint's file leaves the
// edge's resolution stable. graph.ReindexEdges keeps the out/in buckets
// consistent.
//
// Ambiguity is resolved conservatively: when more than one candidate
// matches, the pass skips the edge (zero false positives over breadth).
//
// Out of scope (left for the generic resolver, the cross-repo resolver,
// or future work): cross-repo Rust calls, trait-bound / generic-typed
// receivers, fully-qualified `<T as Trait>::method` UFCS, and resolving
// a module path against a real module-tree node (only the trailing
// segment + locality is used today).
//
// Returns the number of Rust call edges this pass landed on a concrete
// node.
func ResolveRustScopeCalls(g graph.Store) int {
	if g == nil {
		return 0
	}

	idx := buildRustScopeIndex(g)
	if idx == nil {
		return 0
	}

	resolved := 0
	var reindexBatch []graph.EdgeReindex

	// Collect candidate edges (still-unresolved Rust EdgeCalls) plus the
	// caller IDs we need to read receiver type / repo / params off, so
	// the per-edge node lookups collapse to one batch.
	type candEdge struct {
		edge *graph.Edge
	}
	var cands []candEdge
	fromIDs := make(map[string]struct{})
	for e := range g.EdgesByKind(graph.EdgeCalls) {
		if e == nil {
			continue
		}
		if !graph.IsUnresolvedTarget(e.To) {
			continue
		}
		if !rustScopeEdgeCandidate(e) {
			continue
		}
		cands = append(cands, candEdge{edge: e})
		if e.From != "" {
			fromIDs[e.From] = struct{}{}
		}
	}
	if len(cands) == 0 {
		return 0
	}

	fromList := make([]string, 0, len(fromIDs))
	for id := range fromIDs {
		fromList = append(fromList, id)
	}
	callerNodes := g.GetNodesByIDs(fromList)

	for _, c := range cands {
		e := c.edge
		caller := callerNodes[e.From]
		if caller == nil || caller.Language != "rust" {
			continue
		}
		targetID := idx.resolve(e, caller)
		if targetID == "" || targetID == e.To {
			continue
		}
		oldTo := e.To
		e.To = targetID
		e.Origin = idx.lastOrigin
		e.Confidence = idx.lastConfidence
		e.ConfidenceLabel = graph.ConfidenceLabelFor(graph.EdgeCalls, idx.lastConfidence)
		if e.Meta == nil {
			e.Meta = map[string]any{}
		}
		e.Meta["rust_resolution"] = idx.lastReason
		reindexBatch = append(reindexBatch, graph.EdgeReindex{Edge: e, OldTo: oldTo})
		resolved++
	}

	if len(reindexBatch) > 0 {
		g.ReindexEdges(reindexBatch)
	}
	return resolved
}

// rustScopeEdgeCandidate reports whether an unresolved call edge is one
// this pass can attempt: a path call (Meta["rust_path"] set) or a
// self/Self selector call (Meta["rust_recv"] in {self, Self}). Every
// other selector call is left to the generic resolver's receiver-type
// cascade.
func rustScopeEdgeCandidate(e *graph.Edge) bool {
	if e.Meta == nil {
		return false
	}
	if p, _ := e.Meta["rust_path"].(string); strings.Contains(p, "::") {
		return true
	}
	if r, _ := e.Meta["rust_recv"].(string); r == "self" || r == "Self" {
		return true
	}
	return false
}

// rustScopeIndex holds the per-repo method/function lookup tables this
// pass binds against. lastOrigin / lastConfidence / lastReason carry the
// provenance of the most recent resolve() call so the edge-rewrite loop
// can stamp it without resolve() returning a struct.
type rustScopeIndex struct {
	// methodsByOwner: (repo, ownerType) → method nodes of that type.
	methodsByOwner map[rustOwnerKey][]*graph.Node
	// freeFuncsByName: (repo, name) → free function nodes.
	freeFuncsByName map[rustNameKey][]*graph.Node
	// paramsByOwner: caller function/method ID → set of param names,
	// for local-shadows-import precedence.
	paramsByOwner map[string]map[string]struct{}

	lastOrigin     string
	lastConfidence float64
	lastReason     string
}

type rustOwnerKey struct {
	repo  string
	owner string
}

type rustNameKey struct {
	repo string
	name string
}

// buildRustScopeIndex walks the graph once and indexes Rust method
// owners, free functions, and caller params. Returns nil when the graph
// has no Rust methods or functions (the pass is a no-op for non-Rust
// graphs).
func buildRustScopeIndex(g graph.Store) *rustScopeIndex {
	idx := &rustScopeIndex{
		methodsByOwner:  map[rustOwnerKey][]*graph.Node{},
		freeFuncsByName: map[rustNameKey][]*graph.Node{},
		paramsByOwner:   map[string]map[string]struct{}{},
	}
	any := false
	for n := range g.NodesByKind(graph.KindMethod) {
		if n == nil || n.Language != "rust" {
			continue
		}
		owner := nodeReceiverType(n)
		if owner == "" {
			continue
		}
		idx.methodsByOwner[rustOwnerKey{repo: n.RepoPrefix, owner: owner}] = append(
			idx.methodsByOwner[rustOwnerKey{repo: n.RepoPrefix, owner: owner}], n)
		any = true
	}
	for n := range g.NodesByKind(graph.KindFunction) {
		if n == nil || n.Language != "rust" {
			continue
		}
		idx.freeFuncsByName[rustNameKey{repo: n.RepoPrefix, name: n.Name}] = append(
			idx.freeFuncsByName[rustNameKey{repo: n.RepoPrefix, name: n.Name}], n)
		any = true
	}
	if !any {
		return nil
	}
	// Params are read lazily-but-once: index every Rust param by its
	// enclosing function/method ID for the shadow check.
	for n := range g.NodesByKind(graph.KindParam) {
		if n == nil || n.Language != "rust" {
			continue
		}
		owner := enclosingFunctionForBinding(n.ID)
		if owner == "" {
			continue
		}
		set := idx.paramsByOwner[owner]
		if set == nil {
			set = map[string]struct{}{}
			idx.paramsByOwner[owner] = set
		}
		set[n.Name] = struct{}{}
	}
	return idx
}

// resolve returns the target node ID an unresolved Rust call edge should
// bind to, or "" when the call can't be resolved unambiguously. It also
// records the provenance (origin / confidence / reason) of a successful
// binding on the index for the caller to stamp.
func (idx *rustScopeIndex) resolve(e *graph.Edge, caller *graph.Node) string {
	repo := caller.RepoPrefix

	// Selector self/Self call: bind to a method of the caller's owner
	// type. The caller is the enclosing impl method, so its receiver is
	// the impl type.
	if recv, _ := e.Meta["rust_recv"].(string); recv == "self" || recv == "Self" {
		owner := nodeReceiverType(caller)
		if owner == "" {
			return ""
		}
		name := selectorCallName(e.To)
		if name == "" {
			return ""
		}
		if id := idx.uniqueMethod(repo, owner, name); id != "" {
			idx.set(graph.OriginASTResolved, 0.92, "self_receiver")
			return id
		}
		return ""
	}

	path, _ := e.Meta["rust_path"].(string)
	if !strings.Contains(path, "::") {
		return ""
	}
	segments := strings.Split(path, "::")
	last := segments[len(segments)-1]
	if last == "" {
		return ""
	}
	qualifier := segments[len(segments)-2]

	switch {
	case qualifier == "Self":
		// Self::method() — same binding as the self selector case.
		owner := nodeReceiverType(caller)
		if owner == "" {
			return ""
		}
		if id := idx.uniqueMethod(repo, owner, last); id != "" {
			idx.set(graph.OriginASTResolved, 0.92, "self_path")
			return id
		}
		return ""

	case isRustTypeName(qualifier):
		// Type::method() — bind to a method whose owner type is the
		// qualifier. The receiver type is named explicitly in source, so
		// this is structurally resolved within that type.
		if id := idx.uniqueMethod(repo, qualifier, last); id != "" {
			idx.set(graph.OriginASTResolved, 0.9, "impl_owner")
			return id
		}
		return ""

	default:
		// Module path: crate::/super::/self::/<module>::func(). Gortex
		// doesn't model the module tree, so bind the trailing segment to
		// a free function in the same repo, preferring locality. Skipped
		// when a same-named caller param shadows the import.
		if idx.callerShadows(e.From, last) {
			return ""
		}
		if id := idx.uniqueFreeFunc(repo, last, caller.FilePath); id != "" {
			idx.set(graph.OriginASTInferred, 0.75, "module_path")
			return id
		}
		return ""
	}
}

func (idx *rustScopeIndex) set(origin string, conf float64, reason string) {
	idx.lastOrigin = origin
	idx.lastConfidence = conf
	idx.lastReason = reason
}

// uniqueMethod returns the ID of the single method named `name` owned by
// (repo, owner), or "" when there is no match or the choice is
// ambiguous (more than one).
func (idx *rustScopeIndex) uniqueMethod(repo, owner, name string) string {
	cands := idx.methodsByOwner[rustOwnerKey{repo: repo, owner: owner}]
	var hit string
	for _, m := range cands {
		if m.Name != name {
			continue
		}
		if hit != "" && hit != m.ID {
			return "" // ambiguous
		}
		hit = m.ID
	}
	return hit
}

// uniqueFreeFunc returns the ID of a free function named `name` in repo,
// preferring a same-file candidate, then a same-directory candidate,
// then a unique candidate overall. Returns "" when nothing matches or
// the choice is ambiguous (more than one across different files with no
// locality tie-break).
func (idx *rustScopeIndex) uniqueFreeFunc(repo, name, callerFile string) string {
	cands := idx.freeFuncsByName[rustNameKey{repo: repo, name: name}]
	if len(cands) == 0 {
		return ""
	}
	if len(cands) == 1 {
		return cands[0].ID
	}
	callerDir := rustParentDir(callerFile)
	var sameFile, sameDir []*graph.Node
	for _, f := range cands {
		if f.FilePath == callerFile {
			sameFile = append(sameFile, f)
		}
		if rustParentDir(f.FilePath) == callerDir {
			sameDir = append(sameDir, f)
		}
	}
	if len(sameFile) == 1 {
		return sameFile[0].ID
	}
	if len(sameFile) == 0 && len(sameDir) == 1 {
		return sameDir[0].ID
	}
	return "" // ambiguous across files
}

// callerShadows reports whether the calling function/method declares a
// parameter named `name` — a local binding that shadows an import of
// the same identifier under Rust's name-resolution rules.
func (idx *rustScopeIndex) callerShadows(callerID, name string) bool {
	set := idx.paramsByOwner[callerID]
	if set == nil {
		return false
	}
	_, ok := set[name]
	return ok
}

// selectorCallName extracts the method name from a selector-call
// placeholder target of the form `unresolved::*.<name>` (or the
// per-repo `<repo>::unresolved::*.<name>` form).
func selectorCallName(to string) string {
	name := graph.UnresolvedName(to)
	if name == "" {
		return ""
	}
	name = strings.TrimPrefix(name, "*.")
	if i := strings.LastIndex(name, "."); i >= 0 {
		name = name[i+1:]
	}
	return name
}

// isRustTypeName reports whether s looks like a Rust type path qualifier
// (UpperCamelCase) rather than a module/path keyword. Crate-relative
// keywords (crate/super/self) and lowercase module names are not types.
func isRustTypeName(s string) bool {
	switch s {
	case "", "crate", "super", "self", "Self":
		return false
	}
	c := s[0]
	return c >= 'A' && c <= 'Z'
}

// rustParentDir returns the slash-separated parent directory of a graph
// file path. Graph paths are slash-normalised, so a plain byte scan is
// correct on every OS.
func rustParentDir(path string) string {
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		return path[:i]
	}
	return ""
}
