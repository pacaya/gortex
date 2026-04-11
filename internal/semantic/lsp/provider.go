package lsp

import (
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/semantic"
)

// Provider uses an LSP server for on-demand semantic queries.
type Provider struct {
	command     string
	args        []string
	languages   []string
	daemon      bool
	maxParallel int
	logger      *zap.Logger

	client *Client
}

// NewProvider creates an LSP provider.
func NewProvider(command string, args []string, languages []string, daemon bool, maxParallel int, logger *zap.Logger) *Provider {
	if maxParallel <= 0 {
		maxParallel = 10
	}
	return &Provider{
		command:     command,
		args:        args,
		languages:   languages,
		daemon:      daemon,
		maxParallel: maxParallel,
		logger:      logger,
	}
}

func (p *Provider) Name() string       { return "lsp-" + p.command }
func (p *Provider) Languages() []string { return p.languages }

func (p *Provider) Available() bool {
	_, err := exec.LookPath(p.command)
	return err == nil
}

func (p *Provider) Close() error {
	if p.client != nil {
		return p.client.Shutdown()
	}
	return nil
}

func (p *Provider) Enrich(g *graph.Graph, repoRoot string) (*semantic.EnrichResult, error) {
	start := time.Now()

	absRoot, err := filepath.Abs(repoRoot)
	if err != nil {
		return nil, err
	}

	// Start or reuse client.
	if err := p.ensureClient(absRoot); err != nil {
		return nil, fmt.Errorf("start LSP server: %w", err)
	}

	result := &semantic.EnrichResult{
		Provider: p.Name(),
		Language: p.languages[0],
	}

	// Collect nodes that need enrichment (AMBIGUOUS or INFERRED edges).
	type enrichTarget struct {
		node *graph.Node
		edge *graph.Edge
	}

	var targets []enrichTarget
	for _, e := range g.AllEdges() {
		if e.Confidence >= 1.0 {
			continue
		}
		fromNode := g.GetNode(e.From)
		if fromNode == nil {
			continue
		}
		langMatch := false
		for _, lang := range p.languages {
			if fromNode.Language == lang {
				langMatch = true
				break
			}
		}
		if langMatch {
			targets = append(targets, enrichTarget{node: fromNode, edge: e})
		}
	}

	// Count total symbols.
	for _, n := range g.AllNodes() {
		if n.Kind == graph.KindFile || n.Kind == graph.KindImport {
			continue
		}
		for _, lang := range p.languages {
			if n.Language == lang {
				result.SymbolsTotal++
				break
			}
		}
	}

	// Open documents for files that have targets.
	openedFiles := make(map[string]bool)
	for _, t := range targets {
		if !openedFiles[t.node.FilePath] {
			if err := p.openDocument(absRoot, t.node.FilePath); err != nil {
				p.logger.Debug("LSP: failed to open document",
					zap.String("file", t.node.FilePath),
					zap.Error(err),
				)
				continue
			}
			openedFiles[t.node.FilePath] = true
		}
	}

	// Query hover info for nodes to enrich metadata.
	enrichedNodes := make(map[string]bool)
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
		if !langMatch {
			continue
		}

		if !openedFiles[n.FilePath] {
			if err := p.openDocument(absRoot, n.FilePath); err != nil {
				continue
			}
			openedFiles[n.FilePath] = true
		}

		hoverResult, err := p.hover(absRoot, n.FilePath, n.StartLine-1, 0)
		if err != nil || hoverResult == nil {
			continue
		}

		typeInfo := extractTypeFromHover(hoverResult.Contents.Value)
		if typeInfo != "" {
			semantic.EnrichNodeMeta(n, "semantic_type", typeInfo, p.Name())
			if !enrichedNodes[n.ID] {
				result.NodesEnriched++
				result.SymbolsCovered++
				enrichedNodes[n.ID] = true
			}
		}
	}

	// Query implementations for interface nodes.
	for _, n := range g.AllNodes() {
		if n.Kind != graph.KindInterface {
			continue
		}
		langMatch := false
		for _, lang := range p.languages {
			if n.Language == lang {
				langMatch = true
				break
			}
		}
		if !langMatch {
			continue
		}

		impls, err := p.findImplementations(absRoot, n.FilePath, n.StartLine-1, 0)
		if err != nil || len(impls) == 0 {
			continue
		}

		for _, loc := range impls {
			implPath := uriToPath(loc.URI, absRoot)
			if implPath == "" {
				continue
			}
			implNode := semantic.MatchNodeByFileLine(g, implPath, loc.Range.Start.Line+1)
			if implNode == nil {
				continue
			}

			existing := semantic.FindMatchingEdge(g, implNode.ID, n.ID, graph.EdgeImplements)
			if existing != nil {
				if existing.Confidence < 1.0 {
					semantic.ConfirmEdge(existing, p.Name())
					result.EdgesConfirmed++
				}
			} else {
				semantic.AddSemanticEdge(g, implNode.ID, n.ID, graph.EdgeImplements,
					implNode.FilePath, implNode.StartLine, p.Name())
				result.EdgesAdded++
			}
		}
	}

	// Query references for AMBIGUOUS edges to confirm/refute.
	for _, t := range targets {
		toNode := g.GetNode(t.edge.To)
		if toNode == nil {
			continue
		}

		refs, err := p.findReferences(absRoot, toNode.FilePath, toNode.StartLine-1, 0)
		if err != nil || len(refs) == 0 {
			continue
		}

		// Check if any reference matches the caller's location.
		confirmed := false
		for _, ref := range refs {
			refPath := uriToPath(ref.URI, absRoot)
			if refPath == t.node.FilePath &&
				ref.Range.Start.Line+1 >= t.node.StartLine &&
				ref.Range.Start.Line+1 <= t.node.EndLine {
				confirmed = true
				break
			}
		}

		if confirmed {
			semantic.ConfirmEdge(t.edge, p.Name())
			result.EdgesConfirmed++
		}
	}

	if result.SymbolsTotal > 0 {
		result.CoveragePercent = float64(result.SymbolsCovered) / float64(result.SymbolsTotal) * 100
	}

	result.DurationMs = time.Since(start).Milliseconds()
	return result, nil
}

func (p *Provider) EnrichFile(g *graph.Graph, repoRoot, filePath string) (*semantic.EnrichResult, error) {
	// LSP supports incremental updates, but for simplicity we skip it.
	// The full Enrich pass handles this.
	return nil, nil
}

// ensureClient starts the LSP server if not already running.
func (p *Provider) ensureClient(workspaceRoot string) error {
	if p.client != nil {
		return nil
	}

	client, err := NewClient(p.command, p.args, workspaceRoot, p.logger)
	if err != nil {
		return err
	}

	// Send initialize request.
	initParams := InitializeParams{
		ProcessID: os.Getpid(),
		RootURI:   pathToURI(workspaceRoot),
		Capabilities: ClientCapabilities{
			TextDocument: TextDocumentClientCapabilities{
				Implementation: &ImplementationCapability{DynamicRegistration: true},
				References:     &ReferencesCapability{DynamicRegistration: true},
				Definition:     &DefinitionCapability{DynamicRegistration: true},
				Hover:          &HoverCapability{ContentFormat: []string{"plaintext"}},
			},
		},
	}

	var initResult InitializeResult
	if err := client.Call("initialize", initParams, &initResult); err != nil {
		_ = client.Shutdown()
		return fmt.Errorf("initialize: %w", err)
	}

	// Send initialized notification.
	if err := client.Notify("initialized", struct{}{}); err != nil {
		_ = client.Shutdown()
		return fmt.Errorf("initialized: %w", err)
	}

	p.client = client
	return nil
}

// openDocument sends textDocument/didOpen for a file.
func (p *Provider) openDocument(repoRoot, relPath string) error {
	absPath := filepath.Join(repoRoot, relPath)
	content, err := os.ReadFile(absPath)
	if err != nil {
		return err
	}

	langID := "go" // default
	if len(p.languages) > 0 {
		langID = p.languages[0]
	}

	return p.client.Notify("textDocument/didOpen", DidOpenTextDocumentParams{
		TextDocument: TextDocumentItem{
			URI:        pathToURI(absPath),
			LanguageID: langID,
			Version:    1,
			Text:       string(content),
		},
	})
}

// hover queries hover info for a position.
func (p *Provider) hover(repoRoot, relPath string, line, col int) (*HoverResult, error) {
	absPath := filepath.Join(repoRoot, relPath)
	params := HoverParams{
		TextDocumentPositionParams: TextDocumentPositionParams{
			TextDocument: TextDocumentIdentifier{URI: pathToURI(absPath)},
			Position:     Position{Line: line, Character: col},
		},
	}

	var result HoverResult
	if err := p.client.Call("textDocument/hover", params, &result); err != nil {
		return nil, err
	}
	if result.Contents.Value == "" {
		return nil, nil
	}
	return &result, nil
}

// findImplementations queries textDocument/implementation.
func (p *Provider) findImplementations(repoRoot, relPath string, line, col int) ([]Location, error) {
	absPath := filepath.Join(repoRoot, relPath)
	params := ImplementationParams{
		TextDocumentPositionParams: TextDocumentPositionParams{
			TextDocument: TextDocumentIdentifier{URI: pathToURI(absPath)},
			Position:     Position{Line: line, Character: col},
		},
	}

	var locations []Location
	if err := p.client.Call("textDocument/implementation", params, &locations); err != nil {
		return nil, err
	}
	return locations, nil
}

// findReferences queries textDocument/references.
func (p *Provider) findReferences(repoRoot, relPath string, line, col int) ([]Location, error) {
	absPath := filepath.Join(repoRoot, relPath)
	params := ReferenceParams{
		TextDocumentPositionParams: TextDocumentPositionParams{
			TextDocument: TextDocumentIdentifier{URI: pathToURI(absPath)},
			Position:     Position{Line: line, Character: col},
		},
		Context: ReferenceContext{IncludeDeclaration: false},
	}

	var locations []Location
	if err := p.client.Call("textDocument/references", params, &locations); err != nil {
		return nil, err
	}
	return locations, nil
}

// pathToURI converts a file path to a file:// URI.
func pathToURI(path string) string {
	absPath, _ := filepath.Abs(path)
	return "file://" + absPath
}

// uriToPath converts a file:// URI to a repo-relative path.
func uriToPath(uri, repoRoot string) string {
	parsed, err := url.Parse(uri)
	if err != nil {
		return ""
	}
	absPath := parsed.Path
	if !strings.HasPrefix(absPath, repoRoot) {
		return ""
	}
	rel, err := filepath.Rel(repoRoot, absPath)
	if err != nil {
		return ""
	}
	return filepath.ToSlash(rel)
}

// extractTypeFromHover extracts type information from hover text.
func extractTypeFromHover(hover string) string {
	// Remove markdown code fences.
	hover = strings.TrimPrefix(hover, "```go\n")
	hover = strings.TrimPrefix(hover, "```\n")
	hover = strings.TrimSuffix(hover, "\n```")
	hover = strings.TrimSpace(hover)

	lines := strings.SplitN(hover, "\n", 2)
	if len(lines) > 0 {
		line := strings.TrimSpace(lines[0])
		if strings.HasPrefix(line, "func ") ||
			strings.HasPrefix(line, "type ") ||
			strings.HasPrefix(line, "var ") ||
			strings.HasPrefix(line, "const ") ||
			strings.HasPrefix(line, "field ") ||
			strings.HasPrefix(line, "package ") {
			return line
		}
		// Short type like "string", "*Foo", "[]byte".
		if !strings.Contains(line, " ") && len(line) > 0 && len(line) < 100 {
			return line
		}
	}
	return ""
}
