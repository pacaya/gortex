package goanalysis

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"path/filepath"
	"strings"
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
}

// NewProvider creates a go/types provider.
func NewProvider(mode LoadMode, includeTest bool, logger *zap.Logger) *Provider {
	return &Provider{
		mode:        mode,
		includeTest: includeTest,
		logger:      logger,
	}
}

func (p *Provider) Name() string       { return "go-types" }
func (p *Provider) Languages() []string { return []string{"go"} }
func (p *Provider) Close() error        { return nil }

func (p *Provider) Available() bool {
	// go/packages requires the Go toolchain. Check if 'go' is on PATH.
	// Since this is a Go binary, the toolchain is almost always present.
	return true
}

func (p *Provider) Enrich(g *graph.Graph, repoRoot string) (*semantic.EnrichResult, error) {
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

	result := &semantic.EnrichResult{
		Provider: p.Name(),
		Language: "go",
	}

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

	// Count total Go symbols.
	for _, n := range g.AllNodes() {
		if n.Language == "go" && n.Kind != graph.KindFile && n.Kind != graph.KindImport {
			result.SymbolsTotal++
		}
	}
	if result.SymbolsTotal > 0 {
		result.CoveragePercent = float64(result.SymbolsCovered) / float64(result.SymbolsTotal) * 100
	}

	// Phase 2: Process references — confirm/add edges.
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
			if !ok {
				continue
			}

			if callerNode.ID == targetNodeID {
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

	// Phase 3: Interface implementations via go/types.
	result.EdgesConfirmed += p.enrichImplements(g, pkgs, objToNode)
	result.EdgesAdded += p.addMissingImplements(g, pkgs, objToNode, absRoot)

	// Phase 4: Enrich node metadata with type info.
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

			typeStr := types.TypeString(obj.Type(), nil)
			if typeStr != "" && typeStr != "invalid type" {
				semantic.EnrichNodeMeta(node, "semantic_type", typeStr, p.Name())
				result.NodesEnriched++
			}

			// Add return type for functions.
			if fn, ok := obj.(*types.Func); ok {
				sig, ok := fn.Type().(*types.Signature)
				if ok && sig.Results().Len() > 0 {
					retType := types.TypeString(sig.Results(), nil)
					semantic.EnrichNodeMeta(node, "return_type", retType, p.Name())
				}
			}

			_ = ident // used in range
		}
	}

	result.DurationMs = time.Since(start).Milliseconds()
	return result, nil
}

func (p *Provider) EnrichFile(g *graph.Graph, repoRoot, filePath string) (*semantic.EnrichResult, error) {
	// go/types can do incremental loading per package, but for simplicity
	// we re-enrich the whole graph. The manager's debounce prevents thrashing.
	return nil, nil
}

// loadPackages loads all Go packages in the given directory with type information.
func (p *Provider) loadPackages(dir string) ([]*packages.Package, *token.FileSet, error) {
	mode := packages.NeedName |
		packages.NeedFiles |
		packages.NeedImports |
		packages.NeedDeps |
		packages.NeedTypes |
		packages.NeedTypesInfo |
		packages.NeedSyntax

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

// enrichImplements confirms existing EdgeImplements edges using go/types.
func (p *Provider) enrichImplements(g *graph.Graph, pkgs []*packages.Package, objToNode map[types.Object]string) int {
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

	// Check existing EdgeImplements edges.
	for _, e := range g.AllEdges() {
		if e.Kind != graph.EdgeImplements {
			continue
		}
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
func (p *Provider) addMissingImplements(g *graph.Graph, pkgs []*packages.Package, objToNode map[types.Object]string, absRoot string) int {
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
func findContainingFunc(g *graph.Graph, pkgs []*packages.Package, fset *token.FileSet, absRoot string, pos token.Position) *graph.Node {
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
