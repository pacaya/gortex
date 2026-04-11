package scip

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/semantic"
)

// Provider runs a SCIP indexer and imports the results into the graph.
type Provider struct {
	command   string
	args      []string
	languages []string
	timeout   time.Duration
	logger    *zap.Logger
}

// NewProvider creates a SCIP provider for the given command and languages.
func NewProvider(command string, args []string, languages []string, timeoutSec int, logger *zap.Logger) *Provider {
	if timeoutSec <= 0 {
		timeoutSec = 120
	}
	return &Provider{
		command:   command,
		args:      args,
		languages: languages,
		timeout:   time.Duration(timeoutSec) * time.Second,
		logger:    logger,
	}
}

func (p *Provider) Name() string       { return "scip-" + p.languages[0] }
func (p *Provider) Languages() []string { return p.languages }
func (p *Provider) Close() error        { return nil }

func (p *Provider) Available() bool {
	_, err := exec.LookPath(p.command)
	return err == nil
}

func (p *Provider) Enrich(g *graph.Graph, repoRoot string) (*semantic.EnrichResult, error) {
	start := time.Now()

	// Run the SCIP indexer.
	indexFile, err := p.runIndexer(repoRoot)
	if err != nil {
		return nil, fmt.Errorf("scip indexer failed: %w", err)
	}
	defer func() { _ = os.Remove(indexFile) }()

	// Parse the SCIP index.
	index, err := ParseSCIPFile(indexFile)
	if err != nil {
		return nil, fmt.Errorf("scip parse failed: %w", err)
	}

	// Build symbol map and enrich the graph.
	result := p.enrichFromIndex(g, index, repoRoot)
	result.Provider = p.Name()
	result.Language = p.languages[0]
	result.DurationMs = time.Since(start).Milliseconds()

	return result, nil
}

func (p *Provider) EnrichFile(g *graph.Graph, repoRoot, filePath string) (*semantic.EnrichResult, error) {
	// SCIP doesn't support incremental indexing well — re-run full enrichment.
	// For large repos, this should be gated by the watch debounce.
	return nil, nil
}

// runIndexer executes the SCIP indexer and returns the path to the output file.
func (p *Provider) runIndexer(repoRoot string) (string, error) {
	tmpDir, err := os.MkdirTemp("", "gortex-scip-*")
	if err != nil {
		return "", err
	}

	outputPath := filepath.Join(tmpDir, "index.scip")

	args := make([]string, len(p.args))
	copy(args, p.args)
	args = append(args, "--output", outputPath)

	cmd := exec.Command(p.command, args...)
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(), "SCIP_OUTPUT="+outputPath)

	output, err := cmd.CombinedOutput()
	if err != nil {
		_ = os.RemoveAll(tmpDir)
		return "", fmt.Errorf("%s failed: %w\noutput: %s", p.command, err, string(output))
	}

	// Check if the output file exists.
	if _, err := os.Stat(outputPath); os.IsNotExist(err) {
		// Some SCIP indexers use different output conventions.
		// Try common alternatives.
		alternatives := []string{
			filepath.Join(repoRoot, "index.scip"),
			filepath.Join(repoRoot, "dump.scip"),
		}
		for _, alt := range alternatives {
			if _, err := os.Stat(alt); err == nil {
				// Move to our tmp dir.
				data, err := os.ReadFile(alt)
				if err == nil {
					_ = os.WriteFile(outputPath, data, 0644)
					_ = os.Remove(alt)
					return outputPath, nil
				}
			}
		}
		_ = os.RemoveAll(tmpDir)
		return "", fmt.Errorf("scip output not found at %s", outputPath)
	}

	return outputPath, nil
}

// enrichFromIndex maps SCIP data to the Gortex graph.
func (p *Provider) enrichFromIndex(g *graph.Graph, index *SCIPIndex, repoRoot string) *semantic.EnrichResult {
	result := &semantic.EnrichResult{}
	symMap := semantic.NewSymbolMap()

	// Phase 1: Build symbol mapping from definitions.
	for _, doc := range index.Documents {
		relPath := doc.RelativePath
		for _, occ := range doc.Occurrences {
			if !occ.IsDefinition() {
				continue
			}
			line := occ.StartLine()
			node := semantic.MatchNodeByFileLine(g, relPath, line)
			if node == nil {
				// Try by name.
				symName := extractSymbolName(occ.Symbol)
				if symName != "" {
					node = semantic.MatchNodeByNameInFile(g, symName, relPath)
				}
			}
			if node != nil {
				symMap.Add(occ.Symbol, node.ID)
				result.SymbolsCovered++
			}
		}
	}

	// Count total symbols for coverage.
	for _, n := range g.AllNodes() {
		if n.Kind == graph.KindFile || n.Kind == graph.KindImport {
			continue
		}
		langMatch := false
		for _, lang := range p.languages {
			if n.Language == lang {
				langMatch = true
				break
			}
		}
		if langMatch {
			result.SymbolsTotal++
		}
	}

	if result.SymbolsTotal > 0 {
		result.CoveragePercent = float64(result.SymbolsCovered) / float64(result.SymbolsTotal) * 100
	}

	// Phase 2: Process reference occurrences — confirm/add edges.
	for _, doc := range index.Documents {
		relPath := doc.RelativePath
		for _, occ := range doc.Occurrences {
			if occ.IsDefinition() {
				continue
			}

			// Find the Gortex node at the reference site.
			refLine := occ.StartLine()
			refNode := findContainingNode(g, relPath, refLine)
			if refNode == nil {
				continue
			}

			// Find the Gortex node for the definition being referenced.
			defNodeID, ok := symMap.GortexID(occ.Symbol)
			if !ok {
				continue
			}

			// Check if an edge already exists between these nodes.
			existing := semantic.FindEdgeByTarget(g, refNode.ID, defNodeID)
			if existing != nil {
				if existing.Confidence < 1.0 {
					semantic.ConfirmEdge(existing, p.Name())
					result.EdgesConfirmed++
				}
			} else {
				// Determine edge kind from context.
				kind := inferEdgeKind(refNode, g.GetNode(defNodeID))
				if kind != "" {
					semantic.AddSemanticEdge(g, refNode.ID, defNodeID, kind, relPath, refLine, p.Name())
					result.EdgesAdded++
				}
			}
		}
	}

	// Phase 3: Process implementation relationships.
	for _, doc := range index.Documents {
		for _, sym := range doc.Symbols {
			for _, rel := range sym.Relationships {
				if !rel.IsImplementation {
					continue
				}
				implID, ok := symMap.GortexID(sym.Symbol)
				if !ok {
					continue
				}
				ifaceID, ok := symMap.GortexID(rel.Symbol)
				if !ok {
					continue
				}

				existing := semantic.FindMatchingEdge(g, implID, ifaceID, graph.EdgeImplements)
				if existing != nil {
					semantic.ConfirmEdge(existing, p.Name())
					result.EdgesConfirmed++
				} else {
					implNode := g.GetNode(implID)
					if implNode != nil {
						semantic.AddSemanticEdge(g, implID, ifaceID, graph.EdgeImplements,
							implNode.FilePath, implNode.StartLine, p.Name())
						result.EdgesAdded++
					}
				}
			}
		}
	}

	// Phase 4: Enrich node metadata from symbol documentation.
	for _, doc := range index.Documents {
		for _, sym := range doc.Symbols {
			nodeID, ok := symMap.GortexID(sym.Symbol)
			if !ok {
				continue
			}
			node := g.GetNode(nodeID)
			if node == nil {
				continue
			}

			if len(sym.Documentation) > 0 {
				// Parse type info from hover documentation.
				typeInfo := extractTypeFromDocs(sym.Documentation)
				if typeInfo != "" {
					semantic.EnrichNodeMeta(node, "semantic_type", typeInfo, p.Name())
					result.NodesEnriched++
				}
			}
		}
	}

	return result
}

// findContainingNode finds the innermost Gortex node that contains the given line.
func findContainingNode(g *graph.Graph, filePath string, line int) *graph.Node {
	nodes := g.GetFileNodes(filePath)
	var best *graph.Node
	bestSize := int(^uint(0) >> 1)
	for _, n := range nodes {
		if n.Kind == graph.KindFile || n.Kind == graph.KindImport {
			continue
		}
		if n.StartLine <= line && line <= n.EndLine {
			size := n.EndLine - n.StartLine
			if size < bestSize {
				best = n
				bestSize = size
			}
		}
	}
	return best
}

// inferEdgeKind determines the edge kind from the node types.
func inferEdgeKind(from, to *graph.Node) graph.EdgeKind {
	if to == nil {
		return ""
	}
	switch to.Kind {
	case graph.KindFunction, graph.KindMethod:
		return graph.EdgeCalls
	case graph.KindType, graph.KindInterface:
		if from.Kind == graph.KindFunction || from.Kind == graph.KindMethod {
			return graph.EdgeReferences
		}
		return graph.EdgeReferences
	default:
		return graph.EdgeReferences
	}
}

// extractSymbolName extracts the short symbol name from a SCIP symbol URI.
// SCIP symbols look like: "scip-go gomod github.com/foo/bar v1.0.0 pkg/Foo.Bar()."
func extractSymbolName(symbol string) string {
	parts := strings.Fields(symbol)
	if len(parts) == 0 {
		return ""
	}
	last := parts[len(parts)-1]
	// Remove trailing punctuation.
	last = strings.TrimRight(last, "().")
	// Extract the last component after '/'.
	if idx := strings.LastIndex(last, "/"); idx >= 0 {
		last = last[idx+1:]
	}
	// Extract after '.' or '#'.
	if idx := strings.LastIndex(last, "."); idx >= 0 {
		last = last[idx+1:]
	}
	if idx := strings.LastIndex(last, "#"); idx >= 0 {
		last = last[idx+1:]
	}
	return last
}

// extractTypeFromDocs extracts type information from SCIP documentation strings.
func extractTypeFromDocs(docs []string) string {
	for _, doc := range docs {
		// SCIP documentation often contains the type signature as the first line.
		lines := strings.SplitN(doc, "\n", 2)
		if len(lines) > 0 {
			line := strings.TrimSpace(lines[0])
			// Look for Go-style type signatures.
			if strings.HasPrefix(line, "func ") ||
				strings.HasPrefix(line, "type ") ||
				strings.HasPrefix(line, "var ") ||
				strings.HasPrefix(line, "const ") {
				return line
			}
			// Look for type annotations like "string", "int", "*Foo".
			if !strings.Contains(line, " ") && len(line) > 0 && len(line) < 100 {
				return line
			}
		}
	}
	return ""
}

// SCIPIndex represents a parsed SCIP index.
type SCIPIndex struct {
	Documents       []SCIPDocument      `json:"documents"`
	ExternalSymbols []SCIPSymbolInfo    `json:"external_symbols"`
}

// SCIPDocument represents a single file in the SCIP index.
type SCIPDocument struct {
	RelativePath string           `json:"relative_path"`
	Occurrences  []SCIPOccurrence `json:"occurrences"`
	Symbols      []SCIPSymbolInfo `json:"symbols"`
}

// SCIPOccurrence represents a symbol occurrence in a document.
type SCIPOccurrence struct {
	Range       []int32 `json:"range"`
	Symbol      string  `json:"symbol"`
	SymbolRoles int32   `json:"symbol_roles"`
}

// IsDefinition returns true if this occurrence is a definition.
func (o *SCIPOccurrence) IsDefinition() bool {
	return o.SymbolRoles&1 != 0 // SymbolRole_Definition = 1
}

// StartLine returns the 1-indexed start line of the occurrence.
func (o *SCIPOccurrence) StartLine() int {
	if len(o.Range) >= 1 {
		return int(o.Range[0]) + 1 // SCIP uses 0-indexed lines
	}
	return 0
}

// SCIPSymbolInfo holds information about a symbol.
type SCIPSymbolInfo struct {
	Symbol        string             `json:"symbol"`
	Documentation []string           `json:"documentation"`
	Relationships []SCIPRelationship `json:"relationships"`
}

// SCIPRelationship describes a relationship between symbols.
type SCIPRelationship struct {
	Symbol           string `json:"symbol"`
	IsImplementation bool   `json:"is_implementation"`
	IsReference      bool   `json:"is_reference"`
	IsTypeDefinition bool   `json:"is_type_definition"`
}

// ParseSCIPFile reads and parses a SCIP index file.
// SCIP uses Protocol Buffers, but we support JSON-encoded SCIP for simplicity.
// For production use with protobuf, this would use the scip protobuf schema.
func ParseSCIPFile(path string) (*SCIPIndex, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	// Try JSON first (for testing and compatibility).
	var index SCIPIndex
	if err := json.Unmarshal(data, &index); err == nil && len(index.Documents) > 0 {
		return &index, nil
	}

	// Try protobuf decoding.
	idx, err := decodeSCIPProtobuf(data)
	if err != nil {
		return nil, fmt.Errorf("failed to parse SCIP file (tried JSON and protobuf): %w", err)
	}
	return idx, nil
}

// decodeSCIPProtobuf decodes a SCIP protobuf file into our internal types.
// This is a minimal protobuf decoder for the SCIP schema without requiring
// the full protobuf dependency.
func decodeSCIPProtobuf(data []byte) (*SCIPIndex, error) {
	// Minimal protobuf wire format decoder for SCIP Index message.
	// SCIP Index has: field 1 = Metadata, field 2 = Document[], field 3 = ExternalSymbol[]
	//
	// Document has: field 4 = relative_path, field 2 = Occurrence[], field 3 = SymbolInformation[]
	// Occurrence has: field 1 = range (repeated int32), field 2 = symbol (string), field 3 = symbol_roles (int32)
	// SymbolInformation has: field 1 = symbol (string), field 3 = documentation (repeated string), field 4 = Relationship[]
	// Relationship has: field 1 = symbol (string), field 2 = is_implementation (bool)

	index := &SCIPIndex{}

	reader := &protoReader{data: data}
	for reader.hasMore() {
		fieldNum, wireType, err := reader.readTag()
		if err != nil {
			return nil, err
		}

		switch fieldNum {
		case 1: // Metadata — skip
			if err := reader.skipField(wireType); err != nil {
				return nil, err
			}
		case 2: // Document
			docData, err := reader.readBytes(wireType)
			if err != nil {
				return nil, err
			}
			doc, err := decodeSCIPDocument(docData)
			if err != nil {
				return nil, err
			}
			index.Documents = append(index.Documents, *doc)
		case 3: // ExternalSymbol
			symData, err := reader.readBytes(wireType)
			if err != nil {
				return nil, err
			}
			sym, err := decodeSCIPSymbolInfo(symData)
			if err != nil {
				return nil, err
			}
			index.ExternalSymbols = append(index.ExternalSymbols, *sym)
		default:
			if err := reader.skipField(wireType); err != nil {
				return nil, err
			}
		}
	}

	return index, nil
}

func decodeSCIPDocument(data []byte) (*SCIPDocument, error) {
	doc := &SCIPDocument{}
	reader := &protoReader{data: data}

	for reader.hasMore() {
		fieldNum, wireType, err := reader.readTag()
		if err != nil {
			return nil, err
		}

		switch fieldNum {
		case 4: // relative_path (string)
			s, err := reader.readString(wireType)
			if err != nil {
				return nil, err
			}
			doc.RelativePath = s
		case 2: // occurrence (message)
			occData, err := reader.readBytes(wireType)
			if err != nil {
				return nil, err
			}
			occ, err := decodeSCIPOccurrence(occData)
			if err != nil {
				return nil, err
			}
			doc.Occurrences = append(doc.Occurrences, *occ)
		case 3: // symbol (SymbolInformation message)
			symData, err := reader.readBytes(wireType)
			if err != nil {
				return nil, err
			}
			sym, err := decodeSCIPSymbolInfo(symData)
			if err != nil {
				return nil, err
			}
			doc.Symbols = append(doc.Symbols, *sym)
		default:
			if err := reader.skipField(wireType); err != nil {
				return nil, err
			}
		}
	}

	return doc, nil
}

func decodeSCIPOccurrence(data []byte) (*SCIPOccurrence, error) {
	occ := &SCIPOccurrence{}
	reader := &protoReader{data: data}

	for reader.hasMore() {
		fieldNum, wireType, err := reader.readTag()
		if err != nil {
			return nil, err
		}

		switch fieldNum {
		case 1: // range (repeated int32, packed)
			if wireType == 2 { // length-delimited (packed)
				rangeData, err := reader.readBytes(wireType)
				if err != nil {
					return nil, err
				}
				rr := &protoReader{data: rangeData}
				for rr.hasMore() {
					v, err := rr.readVarint()
					if err != nil {
						return nil, err
					}
					occ.Range = append(occ.Range, int32(v))
				}
			} else { // varint (non-packed, repeated)
				v, err := reader.readVarint()
				if err != nil {
					return nil, err
				}
				occ.Range = append(occ.Range, int32(v))
			}
		case 2: // symbol (string)
			s, err := reader.readString(wireType)
			if err != nil {
				return nil, err
			}
			occ.Symbol = s
		case 3: // symbol_roles (int32)
			v, err := reader.readVarint()
			if err != nil {
				return nil, err
			}
			occ.SymbolRoles = int32(v)
		default:
			if err := reader.skipField(wireType); err != nil {
				return nil, err
			}
		}
	}

	return occ, nil
}

func decodeSCIPSymbolInfo(data []byte) (*SCIPSymbolInfo, error) {
	sym := &SCIPSymbolInfo{}
	reader := &protoReader{data: data}

	for reader.hasMore() {
		fieldNum, wireType, err := reader.readTag()
		if err != nil {
			return nil, err
		}

		switch fieldNum {
		case 1: // symbol (string)
			s, err := reader.readString(wireType)
			if err != nil {
				return nil, err
			}
			sym.Symbol = s
		case 3: // documentation (repeated string)
			s, err := reader.readString(wireType)
			if err != nil {
				return nil, err
			}
			sym.Documentation = append(sym.Documentation, s)
		case 4: // relationship (message)
			relData, err := reader.readBytes(wireType)
			if err != nil {
				return nil, err
			}
			rel, err := decodeSCIPRelationship(relData)
			if err != nil {
				return nil, err
			}
			sym.Relationships = append(sym.Relationships, *rel)
		default:
			if err := reader.skipField(wireType); err != nil {
				return nil, err
			}
		}
	}

	return sym, nil
}

func decodeSCIPRelationship(data []byte) (*SCIPRelationship, error) {
	rel := &SCIPRelationship{}
	reader := &protoReader{data: data}

	for reader.hasMore() {
		fieldNum, wireType, err := reader.readTag()
		if err != nil {
			return nil, err
		}

		switch fieldNum {
		case 1: // symbol (string)
			s, err := reader.readString(wireType)
			if err != nil {
				return nil, err
			}
			rel.Symbol = s
		case 2: // is_implementation (bool)
			v, err := reader.readVarint()
			if err != nil {
				return nil, err
			}
			rel.IsImplementation = v != 0
		case 3: // is_reference (bool)
			v, err := reader.readVarint()
			if err != nil {
				return nil, err
			}
			rel.IsReference = v != 0
		case 4: // is_type_definition (bool)
			v, err := reader.readVarint()
			if err != nil {
				return nil, err
			}
			rel.IsTypeDefinition = v != 0
		default:
			if err := reader.skipField(wireType); err != nil {
				return nil, err
			}
		}
	}

	return rel, nil
}
