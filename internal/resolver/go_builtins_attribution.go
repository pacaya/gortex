package resolver

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// goBuiltinFuncs is the complete set of pre-declared Go built-in
// functions. Source: https://pkg.go.dev/builtin (functions section).
// Kept in sync with the language spec — when a new builtin lands
// (e.g. clear / min / max in Go 1.21) add it here.
var goBuiltinFuncs = map[string]struct{}{
	"append": {}, "cap": {}, "clear": {}, "close": {}, "complex": {},
	"copy": {}, "delete": {}, "imag": {}, "len": {}, "make": {},
	"max": {}, "min": {}, "new": {}, "panic": {}, "print": {},
	"println": {}, "real": {}, "recover": {},
}

// goBuiltinTypes is the complete set of pre-declared Go built-in
// types. Source: https://pkg.go.dev/builtin (types section).
var goBuiltinTypes = map[string]struct{}{
	"any": {}, "bool": {}, "byte": {}, "comparable": {},
	"complex64": {}, "complex128": {}, "error": {},
	"float32": {}, "float64": {},
	"int": {}, "int8": {}, "int16": {}, "int32": {}, "int64": {},
	"rune": {}, "string": {},
	"uint": {}, "uint8": {}, "uint16": {}, "uint32": {}, "uint64": {},
	"uintptr": {},
}

// goBuiltinConsts is the set of pre-declared Go constants (true,
// false, iota, nil). Mostly emitted for completeness — `true` /
// `false` rarely show up as unresolved edge targets in practice
// because the parser handles them inline.
var goBuiltinConsts = map[string]struct{}{
	"true": {}, "false": {}, "iota": {}, "nil": {},
}

// attributeGoBuiltins rewrites `unresolved::<name>` edges whose name
// is a Go language intrinsic onto the canonical `builtin::go::*` ID,
// and materialises a single KindBuiltin node per unique builtin so
// the rewritten edges land at a real graph node instead of a
// rel-table FK stub. Mirrors the existing builtin::py / builtin::ts
// classifier in internal/resolver/builtins.go but completes the
// pattern by also creating nodes for the targets — so
// `find_usages(builtin::go::type::float64)` answers "every variable
// typed as float64 in this codebase", and the on-disk-backend stub
// inflation drops by ~50k rows on a gortex-scale Go codebase.
//
// Three ID namespaces under `builtin::go::`:
//
//	functions: builtin::go::<name>          (append, len, make, ...)
//	types:     builtin::go::type::<name>    (string, int, float64, ...)
//	constants: builtin::go::const::<name>   (true, false, iota, nil)
//
// Functions get the shortest namespace because their fan-in is the
// biggest and the shorter ID is what most downstream `find_usages`
// queries will type.
func (r *Resolver) attributeGoBuiltins() {
	// Go-only pass: skip the scan entirely when the graph has no Go nodes
	// (e.g. a TS/Python repo).
	if !r.graphHasLanguage("go") {
		return
	}
	materialised := map[string]struct{}{}
	var batch []graph.EdgeReindex

	// tryAttributeGoBuiltin only ever acts on an edge whose To has the bare
	// `unresolved::` prefix — every other edge is a guaranteed no-op. This
	// used to scan every edge of each of the 11 candidate kinds below
	// (resolved and unresolved alike) via 11 separate EdgesByKind calls;
	// EdgesWithUnresolvedTarget's is_unresolved index (see isUnresolvedColumnDDL)
	// already collects the exact superset in one indexed scan, so filtering
	// to these kinds in Go is equivalent but skips every resolved edge and
	// every kind this pass never inspects. IsUnresolvedTarget covers both
	// the bare and repo-qualified forms; tryAttributeGoBuiltin's own prefix
	// check still rejects the repo-qualified ones exactly as it always did.
	for e := range r.graph.EdgesWithUnresolvedTarget() {
		if e == nil {
			continue
		}
		if _, ok := attributeGoBuiltinCandidateKinds[e.Kind]; !ok {
			continue
		}
		if old := r.tryAttributeGoBuiltin(e, materialised); old != "" {
			batch = append(batch, graph.EdgeReindex{Edge: e, OldTo: old})
		}
	}
	if len(batch) > 0 {
		r.graph.ReindexEdges(batch)
	}
}

// attributeGoBuiltinCandidateKinds is every edge kind a builtin can be the
// target of. Type-system edges (typed_as / returns) carry type references;
// call / arg-of / value-flow carry function or const references.
var attributeGoBuiltinCandidateKinds = map[graph.EdgeKind]struct{}{
	graph.EdgeCalls:        {},
	graph.EdgeReferences:   {},
	graph.EdgeReads:        {},
	graph.EdgeArgOf:        {},
	graph.EdgeValueFlow:    {},
	graph.EdgeReturnsTo:    {},
	graph.EdgeTypedAs:      {},
	graph.EdgeReturns:      {},
	graph.EdgeInstantiates: {},
	graph.EdgeCaptures:     {},
	graph.EdgeThrows:       {},
}

// attributeGoBuiltinsForFile is the single-file scope of attributeGoBuiltins:
// it only inspects the edited file's outgoing edges. A builtin reference's
// source endpoint is always inside the file that mentions it, so this
// produces the same rewrites as the whole-graph sweep for a per-save
// resolve without scanning every edge of eleven kinds across the graph.
func (r *Resolver) attributeGoBuiltinsForFile(filePath string) {
	if !r.graphHasLanguage("go") {
		return
	}
	materialised := map[string]struct{}{}
	var batch []graph.EdgeReindex
	for _, e := range r.fileOutEdges(filePath) {
		if old := r.tryAttributeGoBuiltin(e, materialised); old != "" {
			batch = append(batch, graph.EdgeReindex{Edge: e, OldTo: old})
		}
	}
	if len(batch) > 0 {
		r.graph.ReindexEdges(batch)
	}
}

// tryAttributeGoBuiltin checks if e.To is `unresolved::<bareName>`
// where bareName is a Go builtin and the source language is Go (the
// source is inside a Go function / file). On a match it materialises
// the target node (once per unique ID), rewrites e.To, and returns
// the old To value for the batched reindex. Returns "" when the edge
// is left alone.
func (r *Resolver) tryAttributeGoBuiltin(e *graph.Edge, materialised map[string]struct{}) string {
	if e == nil || !strings.HasPrefix(e.To, "unresolved::") {
		return ""
	}
	name := strings.TrimPrefix(e.To, "unresolved::")
	if name == "" || strings.ContainsAny(name, ".*:#") {
		return ""
	}
	// Cheap membership check first: three small map lookups, no graph
	// access. Only ~2% of candidate names are ever a Go builtin in
	// practice, so rejecting the rest here — before the language-origin
	// check and the repo-prefix lookup below, both of which can fall back
	// to a graph node lookup — avoids paying for either on the ~98% that
	// were always going to return "" anyway.
	if !isGoBuiltinName(name) {
		return ""
	}
	// Only attribute when the source is Go. Without this guard a
	// Python reference to a local named `len` would get re-targeted
	// at Go's builtin `len`, which would be obviously wrong. Dataflow
	// edges (arg_of / value_flow) carry an `unresolved::` From placeholder
	// that fromIsGo cannot classify, so fall back to the call-site file
	// extension: a `.go` file's `append` / `make` / `len` argument is the Go
	// builtin regardless of whether the argument side ever bound to a node.
	// (langFromFilePath only classifies js/ts/py, so a `.go` suffix test is
	// the right check here.) e.FilePath is a free struct-field read while
	// fromIsGo's fallback path can hit a node lookup, so try FilePath
	// first — De Morgan's / && being commutative means this is the exact
	// same condition, just evaluated in the cheaper order.
	if !strings.HasSuffix(e.FilePath, ".go") && !r.fromIsGo(e.From) {
		return ""
	}
	newID, kind, builtinKind := goBuiltinTarget(r.callerRepoPrefix(e), name)
	if newID == "" {
		return ""
	}
	if _, ok := materialised[newID]; !ok {
		// AddNode is idempotent on ID, so even a second
		// concurrent pass would not duplicate the row.
		r.graph.AddNode(&graph.Node{
			ID:       newID,
			Kind:     kind,
			Name:     name,
			Language: "go",
			Meta: map[string]any{
				"builtin":      true,
				"builtin_kind": builtinKind,
			},
		})
		materialised[newID] = struct{}{}
	}
	oldTo := e.To
	e.To = newID
	return oldTo
}

// isGoBuiltinName reports whether name is in any of the three builtin
// namespaces, without needing a repo prefix — the cheap pre-check
// tryAttributeGoBuiltin runs before the (potentially graph-lookup-backed)
// language-origin check and repo-prefix resolution. Mirrors goBuiltinTarget's
// own membership tests exactly; kept as a separate, repo-prefix-free
// function so the common "not a builtin" case never has to compute
// anything else first.
func isGoBuiltinName(name string) bool {
	if _, ok := goBuiltinFuncs[name]; ok {
		return true
	}
	if _, ok := goBuiltinTypes[name]; ok {
		return true
	}
	_, ok := goBuiltinConsts[name]
	return ok
}

// goBuiltinTarget classifies a bare identifier as one of Go's
// intrinsics. Returns the canonical builtin::go:: ID (per-repo
// prefixed via graph.StubID — see internal/graph/stub.go for why
// two repos can disagree on what's a builtin), the NodeKind to
// materialise it under (always KindBuiltin), and a meta tag
// recording which subspace (func / type / const) it belongs to.
// Returns ("", "", "") when the name is not a Go builtin.
// repoPrefix is the owning repo's RepoPrefix (empty in
// single-repo / legacy callers). Callers on the tryAttributeGoBuiltin path
// have already confirmed isGoBuiltinName(name) before calling this, so the
// repeated map lookups here run only on the small matching subset.
func goBuiltinTarget(repoPrefix, name string) (id string, kind graph.NodeKind, builtinKind string) {
	if _, ok := goBuiltinFuncs[name]; ok {
		return graph.StubID(repoPrefix, graph.StubKindBuiltin, "go", name), graph.KindBuiltin, "func"
	}
	if _, ok := goBuiltinTypes[name]; ok {
		return graph.StubID(repoPrefix, graph.StubKindBuiltin, "go", "type", name), graph.KindBuiltin, "type"
	}
	if _, ok := goBuiltinConsts[name]; ok {
		return graph.StubID(repoPrefix, graph.StubKindBuiltin, "go", "const", name), graph.KindBuiltin, "const"
	}
	return "", "", ""
}

// fromIsGo reports whether the source endpoint of an edge sits
// inside Go code. Uses the From's enclosing function (via the same
// suffix-stripping helper bare-name binding uses) — Go is the only
// language whose IDs follow the `file.go::Func` convention with a
// `.go` extension, so a path-based check is both cheap and reliable.
func (r *Resolver) fromIsGo(fromID string) bool {
	owner := enclosingFunctionForBinding(fromID)
	if owner == "" {
		return false
	}
	if i := strings.Index(owner, "::"); i > 0 {
		// `pkg/foo.go::Func` shape — peek at the file extension.
		head := owner[:i]
		if strings.HasSuffix(head, ".go") {
			return true
		}
	}
	// Fall back to looking up the owner node and checking its
	// Language. More expensive but covers edge cases where the ID
	// doesn't follow the `.go::Func` pattern.
	if n := r.cachedGetNode(owner); n != nil && n.Language == "go" {
		return true
	}
	return false
}
