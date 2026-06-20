package goanalysis

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
	"golang.org/x/tools/go/packages"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/semantic"
)

// LoadMode controls how deeply the go/types provider analyzes the code.
type LoadMode int

const (
	// ModeTypeCheck loads types only (~5-10s). Resolves all type information
	// and interface implementations but does not build a call graph.
	ModeTypeCheck LoadMode = iota

	// ModeCallGraph loads SSA and builds a VTA call graph (~15-30s).
	// Most precise but requires more time and memory.
	ModeCallGraph
)

// Provider uses Go's native toolchain (go/packages, go/types) for
// compiler-level precision on Go codebases.
type Provider struct {
	mode        LoadMode
	includeTest bool
	logger      *zap.Logger

	// Cached state from the last Enrich() — used by LookupTypeAtLine
	// to answer per-binding type queries from the contract pipeline
	// without re-loading packages. Guarded by stateMu.
	stateMu sync.RWMutex
	pkgs    []*packages.Package
	fset    *token.FileSet
	absRoot string
}

// NewProvider creates a go/types provider.
func NewProvider(mode LoadMode, includeTest bool, logger *zap.Logger) *Provider {
	return &Provider{
		mode:        mode,
		includeTest: includeTest,
		logger:      logger,
	}
}

func (p *Provider) Name() string        { return "go-types" }
func (p *Provider) Languages() []string { return []string{"go"} }
func (p *Provider) Close() error        { return nil }

func (p *Provider) Available() bool {
	// go/packages requires the Go toolchain. Check if 'go' is on PATH.
	// Since this is a Go binary, the toolchain is almost always present.
	return true
}

func (p *Provider) Enrich(g graph.Store, repoRoot string) (*semantic.EnrichResult, error) {
	return p.EnrichRepo(g, "", repoRoot)
}

// EnrichRepo runs the go/types enrichment pass with its graph scans scoped
// to repoPrefix (the multi-repo scope key; "" for a single-repo / in-memory
// graph). The go/packages load is already scoped to repoRoot; scoping the
// graph-side symbol count and implements-edge scan to one repo stops a
// multi-repo warmup from paying a whole-graph AllNodes / AllEdges walk per
// repo. Implementing this makes the provider a semantic.RepoScopedProvider,
// so the manager dispatches it per repo with the repo's prefix.
func (p *Provider) EnrichRepo(g graph.Store, repoPrefix, repoRoot string) (*semantic.EnrichResult, error) {
	start := time.Now()

	absRoot, err := filepath.Abs(repoRoot)
	if err != nil {
		return nil, fmt.Errorf("absolute path: %w", err)
	}

	// Load all packages with type information.
	pkgs, fset, err := p.loadPackages(absRoot)
	if err != nil {
		return nil, fmt.Errorf("load packages: %w", err)
	}

	// Stash the loaded state so LookupTypeAtLine can serve per-binding
	// type queries from the contract pipeline without paying the
	// 5-10s loadPackages cost again. The state survives until the
	// next Enrich call (which replaces it).
	p.stateMu.Lock()
	p.pkgs = pkgs
	p.fset = fset
	p.absRoot = absRoot
	p.stateMu.Unlock()

	result := &semantic.EnrichResult{
		Provider: p.Name(),
		Language: "go",
	}

	// Serialise the graph-touching work below on the backend resolve mutex —
	// the same lock every other edge-mutating pass holds — so this pass can run
	// concurrently with other repos' enrichment. loadPackages (the expensive
	// go/packages load) already ran above, outside the lock, so it still
	// overlaps across repos; only the in-memory graph build is serialised.
	rmu := g.ResolveMutex()
	rmu.Lock()
	defer rmu.Unlock()

	// Build symbol map: go/types objects → Gortex node IDs.
	symMap := semantic.NewSymbolMap()
	objToNode := make(map[types.Object]string) // types.Object → Gortex node ID

	// Phase 1: Map definitions.
	for _, pkg := range pkgs {
		if pkg.TypesInfo == nil {
			continue
		}

		for ident, obj := range pkg.TypesInfo.Defs {
			if obj == nil || ident.Pos() == token.NoPos {
				continue
			}

			pos := fset.Position(ident.Pos())
			relPath := relativePath(pos.Filename, absRoot)
			if relPath == "" {
				continue
			}

			node := semantic.MatchNodeByFileLine(g, relPath, pos.Line)
			if node == nil {
				node = semantic.MatchNodeByNameInFile(g, ident.Name, relPath)
			}
			if node != nil {
				objID := objectID(obj)
				symMap.Add(objID, node.ID)
				objToNode[obj] = node.ID
				result.SymbolsCovered++
			}
		}
	}

	// Count total Go symbols in this repo via the indexed repo-scoped scan
	// rather than a whole-graph AllNodes walk (which, in a multi-repo graph,
	// also wrongly counted every other repo's Go nodes against this repo's
	// coverage).
	for _, n := range repoGoNodes(g, repoPrefix) {
		if n.Kind != graph.KindFile && n.Kind != graph.KindImport {
			result.SymbolsTotal++
		}
	}
	if result.SymbolsTotal > 0 {
		result.CoveragePercent = float64(result.SymbolsCovered) / float64(result.SymbolsTotal) * 100
	}

	// Externals attribution: every Use of an external symbol becomes
	// an EdgeCalls / EdgeReferences targeting a freshly materialised
	// `ext::go:<importPath>::<name>` node, which itself carries an
	// EdgeDependsOnModule to the owning KindModule. Previously the
	// resolver left these calls pointing at stub strings
	// (`stdlib::fmt::Println`, `dep::github.com/.../foo::Bar`) that no
	// node holds; goanalysis upgrades them to real graph nodes with
	// LSP-grade origin.
	externals := newExternalsAttribution(g, pkgs, p.Name())

	// Phase 2: Process references — confirm/add edges. External symbols
	// are routed through externals.resolveSymbol so calls into stdlib
	// and module-cache packages land on real graph nodes rather than
	// the resolver's stub strings.
	for _, pkg := range pkgs {
		if pkg.TypesInfo == nil {
			continue
		}

		for ident, obj := range pkg.TypesInfo.Uses {
			if obj == nil || ident.Pos() == token.NoPos {
				continue
			}

			pos := fset.Position(ident.Pos())
			relPath := relativePath(pos.Filename, absRoot)
			if relPath == "" {
				continue
			}

			// Find the containing Gortex node (the caller).
			callerNode := findContainingFunc(g, pkgs, fset, absRoot, pos)
			if callerNode == nil {
				continue
			}

			// Find the target Gortex node (the definition being used).
			targetNodeID, ok := objToNode[obj]
			external := false
			if !ok {
				targetNodeID = externals.resolveSymbol(obj)
				if targetNodeID == "" {
					continue
				}
				external = true
			}

			if callerNode.ID == targetNodeID {
				continue
			}

			// External: claim a resolver-stub edge if one exists, else
			// add a fresh edge. Internal: confirm or add as before.
			if external {
				importPath := obj.Pkg().Path()
				if upgraded := externals.claimAndUpgradeStub(callerNode.ID, importPath, obj, targetNodeID, pos.Line); upgraded != nil {
					result.EdgesConfirmed++
					continue
				}
				existing := semantic.FindEdgeByTarget(g, callerNode.ID, targetNodeID)
				if existing != nil {
					if existing.Confidence < 1.0 {
						semantic.ConfirmEdge(existing, p.Name())
						result.EdgesConfirmed++
					}
					continue
				}
				kind := inferEdgeKindFromObj(obj)
				if kind != "" {
					semantic.AddSemanticEdge(g, callerNode.ID, targetNodeID, kind,
						relPath, pos.Line, p.Name())
					result.EdgesAdded++
				}
				continue
			}

			// Check if an edge already exists.
			existing := semantic.FindEdgeByTarget(g, callerNode.ID, targetNodeID)
			if existing != nil {
				if existing.Confidence < 1.0 {
					semantic.ConfirmEdge(existing, p.Name())
					result.EdgesConfirmed++
				}
			} else {
				// Determine edge kind.
				kind := inferEdgeKindFromObj(obj)
				if kind != "" {
					semantic.AddSemanticEdge(g, callerNode.ID, targetNodeID, kind,
						relPath, pos.Line, p.Name())
					result.EdgesAdded++
				}
			}
		}
	}

	// Stitch the externals counters into the standard result. NodesEnriched
	// previously only incremented for in-repo type-meta enrichment; here
	// we surface the synthetic external + module nodes the externals
	// pass added so callers can see the full graph delta in one number.
	result.EdgesAdded += externals.edgesAdded + externals.edgesUpgraded
	result.NodesEnriched += externals.nodesAdded

	// Phase 3: Interface implementations via go/types.
	result.EdgesConfirmed += p.enrichImplements(g, pkgs, objToNode)
	result.EdgesAdded += p.addMissingImplements(g, pkgs, objToNode, absRoot)

	// Phase 4: Enrich node metadata with type info.
	// EnrichNodeMeta mutates Node.Meta in place; on disk backends the
	// node is a per-call GetNode reconstruction, so collect every stamped
	// node and round-trip it through the store at the end (one AddBatch)
	// or the semantic_type / return_type stamps are silently discarded on
	// the disk backend. See semantic.EnrichNodeMeta.
	var stampedNodes []*graph.Node
	for _, pkg := range pkgs {
		if pkg.TypesInfo == nil {
			continue
		}
		for ident, obj := range pkg.TypesInfo.Defs {
			if obj == nil {
				continue
			}
			nodeID, ok := objToNode[obj]
			if !ok {
				continue
			}
			node := g.GetNode(nodeID)
			if node == nil {
				continue
			}

			didStamp := false
			typeStr := types.TypeString(obj.Type(), nil)
			if typeStr != "" && typeStr != "invalid type" {
				semantic.EnrichNodeMeta(node, "semantic_type", typeStr, p.Name())
				result.NodesEnriched++
				didStamp = true
			}

			// Add return type for functions.
			if fn, ok := obj.(*types.Func); ok {
				sig, ok := fn.Type().(*types.Signature)
				if ok && sig.Results().Len() > 0 {
					retType := types.TypeString(sig.Results(), nil)
					semantic.EnrichNodeMeta(node, "return_type", retType, p.Name())
					didStamp = true
				}
			}
			if didStamp {
				stampedNodes = append(stampedNodes, node)
			}

			_ = ident // used in range
		}
	}
	if len(stampedNodes) > 0 {
		g.AddBatch(stampedNodes, nil)
	}

	result.DurationMs = time.Since(start).Milliseconds()
	return result, nil
}

func (p *Provider) EnrichFile(g graph.Store, repoRoot, filePath string) (*semantic.EnrichResult, error) {
	// go/types can do incremental loading per package, but for simplicity
	// we re-enrich the whole graph. The manager's debounce prevents thrashing.
	return nil, nil
}

// LookupTypeAtLine returns the resolved type name of the first
// short_var_declaration / var_spec / typed declaration whose start
// line matches `line` in the file at `filePath`. Returns ("", false)
// when:
//   - Enrich hasn't been called (no cached state)
//   - filePath isn't in any loaded package
//   - no typed declaration is found at `line`
//   - the type can't be resolved via go/types
//
// This is the lsp_resolved upgrade tier referenced in
// spec-contract-extraction.md §4.5: when the goanalysis provider
// has run, the contract pipeline can ask for compiler-grade type
// resolution at any line in the indexed source.
func (p *Provider) LookupTypeAtLine(filePath string, line int) (string, bool) {
	p.stateMu.RLock()
	pkgs := p.pkgs
	fset := p.fset
	absRoot := p.absRoot
	p.stateMu.RUnlock()
	if len(pkgs) == 0 || fset == nil || absRoot == "" {
		return "", false
	}
	target := normalizeRelPath(filePath)
	for _, pkg := range pkgs {
		if pkg.TypesInfo == nil {
			continue
		}
		for _, syntax := range pkg.Syntax {
			if syntax == nil {
				continue
			}
			pos := fset.Position(syntax.Pos())
			if normalizeRelPath(relativePath(pos.Filename, absRoot)) != target {
				continue
			}
			if t, ok := lookupTypeAtLineInFile(syntax, pkg.TypesInfo, fset, line); ok {
				return t, true
			}
		}
	}
	return "", false
}

// lookupTypeAtLineInFile walks the file's AST and returns the type
// name of the first declaration at `line` whose LHS the type info
// table has a type for.
func lookupTypeAtLineInFile(file *ast.File, info *types.Info, fset *token.FileSet, line int) (string, bool) {
	var found string
	ast.Inspect(file, func(n ast.Node) bool {
		if n == nil || found != "" {
			return false
		}
		startLine := fset.Position(n.Pos()).Line
		if startLine != line {
			// Keep descending if this node spans the target.
			endLine := fset.Position(n.End()).Line
			return startLine <= line && endLine >= line
		}
		// We're at the target line. Try to extract a type from the
		// most common declaration shapes.
		switch d := n.(type) {
		case *ast.AssignStmt:
			if name := typeNameFromAssign(d, info); name != "" {
				found = name
			}
		case *ast.GenDecl:
			if name := typeNameFromGenDecl(d, info); name != "" {
				found = name
			}
		case *ast.DeclStmt:
			if gd, ok := d.Decl.(*ast.GenDecl); ok {
				if name := typeNameFromGenDecl(gd, info); name != "" {
					found = name
				}
			}
		}
		return found == ""
	})
	return found, found != ""
}

// typeNameFromAssign reads the LHS type from a short var declaration
// (`x := f()` or `x := Foo{...}`). Returns the underlying named
// type's name.
func typeNameFromAssign(stmt *ast.AssignStmt, info *types.Info) string {
	if len(stmt.Lhs) == 0 || len(stmt.Rhs) == 0 {
		return ""
	}
	for i, lhs := range stmt.Lhs {
		ident, ok := lhs.(*ast.Ident)
		if !ok || ident.Name == "_" {
			continue
		}
		obj := info.Defs[ident]
		if obj == nil {
			obj = info.Uses[ident]
		}
		if obj != nil {
			if name := unwrapTypeName(obj.Type()); name != "" {
				return name
			}
		}
		// Fall back to the RHS expression's type.
		var rhs ast.Expr
		if i < len(stmt.Rhs) {
			rhs = stmt.Rhs[i]
		} else if len(stmt.Rhs) == 1 {
			rhs = stmt.Rhs[0]
		}
		if rhs != nil {
			if t, ok := info.Types[rhs]; ok && t.Type != nil {
				if name := unwrapTypeName(t.Type); name != "" {
					return name
				}
			}
		}
	}
	return ""
}

// typeNameFromGenDecl handles `var x Foo` / `var x = Foo{...}`.
func typeNameFromGenDecl(decl *ast.GenDecl, info *types.Info) string {
	for _, spec := range decl.Specs {
		vs, ok := spec.(*ast.ValueSpec)
		if !ok {
			continue
		}
		for i, name := range vs.Names {
			if name.Name == "_" {
				continue
			}
			obj := info.Defs[name]
			if obj != nil {
				if t := unwrapTypeName(obj.Type()); t != "" {
					return t
				}
			}
			if vs.Type != nil {
				if t, ok := info.Types[vs.Type]; ok && t.Type != nil {
					if u := unwrapTypeName(t.Type); u != "" {
						return u
					}
				}
			}
			if i < len(vs.Values) {
				if t, ok := info.Types[vs.Values[i]]; ok && t.Type != nil {
					if u := unwrapTypeName(t.Type); u != "" {
						return u
					}
				}
			}
		}
	}
	return ""
}

// unwrapTypeName strips slice/pointer/array wrappers and returns the
// underlying named type's bare name. Returns "" for primitives,
// interfaces, and untyped expressions.
func unwrapTypeName(t types.Type) string {
	if t == nil {
		return ""
	}
	for {
		switch x := t.(type) {
		case *types.Pointer:
			t = x.Elem()
		case *types.Slice:
			t = x.Elem()
		case *types.Array:
			t = x.Elem()
		default:
			named, ok := t.(*types.Named)
			if !ok {
				return ""
			}
			return named.Obj().Name()
		}
	}
}

// normalizeRelPath collapses a/./b → a/b and uses forward slashes,
// so OS-dependent path separators don't trip the comparison.
func normalizeRelPath(p string) string {
	if p == "" {
		return ""
	}
	return filepath.ToSlash(filepath.Clean(p))
}

// loadPackages loads all Go packages in the given directory with type information.
func (p *Provider) loadPackages(dir string) ([]*packages.Package, *token.FileSet, error) {
	mode := packages.NeedName |
		packages.NeedFiles |
		packages.NeedImports |
		packages.NeedDeps |
		packages.NeedTypes |
		packages.NeedTypesInfo |
		packages.NeedSyntax |
		// NeedModule populates pkg.Module so the externals pass can
		// classify imports as stdlib (Module == nil), module_cache
		// (Module != nil && !Main), or main (Module.Main). Without
		// it the loader returns nil for every Module field and we
		// can't tell stdlib calls from internal-package calls.
		packages.NeedModule

	cfg := &packages.Config{
		Mode:  mode,
		Dir:   dir,
		Tests: p.includeTest,
		Fset:  token.NewFileSet(),
	}

	patterns := []string{"./..."}
	pkgs, err := packages.Load(cfg, patterns...)
	if err != nil {
		return nil, nil, err
	}

	// Filter out packages with errors (they may have partial type info).
	var valid []*packages.Package
	for _, pkg := range pkgs {
		if len(pkg.Errors) > 0 {
			p.logger.Debug("package has errors, using partial info",
				zap.String("pkg", pkg.PkgPath),
				zap.Int("errors", len(pkg.Errors)),
			)
		}
		if pkg.TypesInfo != nil {
			valid = append(valid, pkg)
		}
	}

	return valid, cfg.Fset, nil
}

// repoGoNodes returns the repo's Go-language nodes via the indexed
// GetRepoNodes scan, falling back to a language-filtered AllNodes pass for
// the embedded single-repo ("") path where GetRepoNodes can come back empty.
func repoGoNodes(g graph.Store, repoPrefix string) []*graph.Node {
	filter := func(nodes []*graph.Node) []*graph.Node {
		out := make([]*graph.Node, 0, len(nodes))
		for _, n := range nodes {
			if n.Language == "go" && n.RepoPrefix == repoPrefix {
				out = append(out, n)
			}
		}
		return out
	}
	out := filter(g.GetRepoNodes(repoPrefix))
	if len(out) == 0 && repoPrefix == "" {
		return filter(g.AllNodes())
	}
	return out
}

// enrichImplements confirms existing EdgeImplements edges using go/types.
func (p *Provider) enrichImplements(g graph.Store, pkgs []*packages.Package, objToNode map[types.Object]string) int {
	confirmed := 0

	// Collect all interfaces from the loaded packages.
	ifaceTypes := make(map[string]*types.Interface) // Gortex node ID → interface type
	for obj, nodeID := range objToNode {
		if tn, ok := obj.(*types.TypeName); ok {
			if iface, ok := tn.Type().Underlying().(*types.Interface); ok {
				ifaceTypes[nodeID] = iface
			}
		}
	}

	// Check existing EdgeImplements edges. Iterate the kind-indexed edge set
	// (not a whole-graph AllEdges scan, but still graph-wide for this kind) so
	// a cross-repo implements edge — concrete type in another repo, interface
	// in this repo's loaded packages — is still confirmed, matching the
	// original behavior.
	for e := range g.EdgesByKind(graph.EdgeImplements) {
		fromNode := g.GetNode(e.From)
		if fromNode == nil || fromNode.Language != "go" {
			continue
		}
		if e.Confidence >= 1.0 {
			continue
		}

		// If we have type info for both sides, verify.
		if _, ok := ifaceTypes[e.To]; ok {
			semantic.ConfirmEdge(e, p.Name())
			confirmed++
		}
	}

	return confirmed
}

// addMissingImplements discovers interface implementations that tree-sitter missed.
func (p *Provider) addMissingImplements(g graph.Store, pkgs []*packages.Package, objToNode map[types.Object]string, absRoot string) int {
	added := 0

	// Collect interfaces and concrete types.
	type ifaceEntry struct {
		nodeID string
		iface  *types.Interface
	}
	type concreteEntry struct {
		nodeID string
		typ    types.Type
		obj    types.Object
	}

	var ifaces []ifaceEntry
	var concretes []concreteEntry

	for obj, nodeID := range objToNode {
		tn, ok := obj.(*types.TypeName)
		if !ok {
			continue
		}
		if iface, ok := tn.Type().Underlying().(*types.Interface); ok {
			ifaces = append(ifaces, ifaceEntry{nodeID: nodeID, iface: iface})
		} else {
			concretes = append(concretes, concreteEntry{nodeID: nodeID, typ: tn.Type(), obj: obj})
		}
	}

	// Check each (concrete, interface) pair.
	for _, c := range concretes {
		for _, i := range ifaces {
			if c.nodeID == i.nodeID {
				continue
			}
			// Check both T and *T.
			if types.Implements(c.typ, i.iface) || types.Implements(types.NewPointer(c.typ), i.iface) {
				existing := semantic.FindMatchingEdge(g, c.nodeID, i.nodeID, graph.EdgeImplements)
				if existing == nil {
					cNode := g.GetNode(c.nodeID)
					if cNode != nil {
						semantic.AddSemanticEdge(g, c.nodeID, i.nodeID, graph.EdgeImplements,
							cNode.FilePath, cNode.StartLine, p.Name())
						added++
					}
				}
			}
		}
	}

	return added
}

// findContainingFunc finds the Gortex function/method node that contains the given position.
func findContainingFunc(g graph.Store, pkgs []*packages.Package, fset *token.FileSet, absRoot string, pos token.Position) *graph.Node {
	relPath := relativePath(pos.Filename, absRoot)
	if relPath == "" {
		return nil
	}

	nodes := g.GetFileNodes(relPath)
	var best *graph.Node
	bestSize := int(^uint(0) >> 1)
	for _, n := range nodes {
		if n.Kind != graph.KindFunction && n.Kind != graph.KindMethod {
			continue
		}
		if n.StartLine <= pos.Line && pos.Line <= n.EndLine {
			size := n.EndLine - n.StartLine
			if size < bestSize {
				best = n
				bestSize = size
			}
		}
	}
	return best
}

// inferEdgeKindFromObj determines the edge kind from a go/types object.
func inferEdgeKindFromObj(obj types.Object) graph.EdgeKind {
	switch obj.(type) {
	case *types.Func:
		return graph.EdgeCalls
	case *types.TypeName:
		return graph.EdgeReferences
	case *types.Var:
		return graph.EdgeReferences
	case *types.Const:
		return graph.EdgeReferences
	default:
		return ""
	}
}

// objectID creates a stable string ID for a go/types object.
func objectID(obj types.Object) string {
	if obj.Pkg() != nil {
		return obj.Pkg().Path() + "." + obj.Name()
	}
	return obj.Name()
}

// relativePath converts an absolute file path to a repo-relative path.
func relativePath(absPath, repoRoot string) string {
	// Skip files outside the repo (stdlib, dependencies).
	if !strings.HasPrefix(absPath, repoRoot) {
		return ""
	}
	rel, err := filepath.Rel(repoRoot, absPath)
	if err != nil {
		return ""
	}
	return filepath.ToSlash(rel)
}

// Ensure ast is used.
var _ = (*ast.File)(nil)
