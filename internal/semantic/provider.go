package semantic

import (
	"github.com/zzet/gortex/internal/graph"
)

// Provider enriches graph edges and nodes with semantic information
// from a language-aware analysis backend (SCIP, go/types, LSP, etc.).
type Provider interface {
	// Name returns a human-readable identifier (e.g., "scip-go", "go-types", "gopls").
	Name() string

	// Languages returns the language codes this provider handles (e.g., ["go"]).
	Languages() []string

	// Available reports whether this provider can run. Checks for
	// external tool availability (e.g., scip-go on PATH, go command present).
	Available() bool

	// Enrich performs a full enrichment pass over the graph for the given repo root.
	// It upgrades edge confidence, adds missing edges, and fills Node.Meta fields.
	// Called after tree-sitter indexing + resolver pass completes.
	Enrich(g *graph.Graph, repoRoot string) (*EnrichResult, error)

	// EnrichFile performs a targeted enrichment for a single file and its
	// immediate dependents. Used in watch mode for incremental updates.
	// Returns nil result if incremental enrichment is not supported.
	EnrichFile(g *graph.Graph, repoRoot string, filePath string) (*EnrichResult, error)

	// Close releases any resources held by the provider (daemon processes,
	// temp files, connections).
	Close() error
}

// EnrichResult contains statistics from an enrichment pass.
type EnrichResult struct {
	Provider        string  `json:"provider"`
	Language        string  `json:"language"`
	EdgesConfirmed  int     `json:"edges_confirmed"`
	EdgesRefuted    int     `json:"edges_refuted"`
	EdgesAdded      int     `json:"edges_added"`
	NodesEnriched   int     `json:"nodes_enriched"`
	SymbolsCovered  int     `json:"symbols_covered"`
	SymbolsTotal    int     `json:"symbols_total"`
	CoveragePercent float64 `json:"coverage_percent"`
	DurationMs      int64   `json:"duration_ms"`
}

// ProviderStatus represents the current state of a semantic provider.
type ProviderStatus struct {
	Name            string  `json:"name"`
	Language        string  `json:"language"`
	Status          string  `json:"status"` // "ready", "unavailable", "error"
	CoveragePercent float64 `json:"coverage_percent,omitempty"`
	LastResult      *EnrichResult `json:"last_result,omitempty"`
}
