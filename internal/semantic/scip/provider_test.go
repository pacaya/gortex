package scip

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
)

func TestSCIPProtobufRoundTrip(t *testing.T) {
	index := &SCIPIndex{
		Documents: []SCIPDocument{
			{
				RelativePath: "main.go",
				Occurrences: []SCIPOccurrence{
					{
						Range:       []int32{9, 5, 9, 9}, // line 10 (0-indexed=9), col 5-9
						Symbol:      "scip-go gomod example.com v1.0.0 main.Foo().",
						SymbolRoles: 1, // Definition
					},
					{
						Range:       []int32{14, 2, 14, 5},
						Symbol:      "scip-go gomod example.com v1.0.0 main.Bar().",
						SymbolRoles: 0, // Reference
					},
				},
				Symbols: []SCIPSymbolInfo{
					{
						Symbol:        "scip-go gomod example.com v1.0.0 main.Foo().",
						Documentation: []string{"func Foo() error"},
					},
				},
			},
		},
	}

	// Encode to protobuf.
	data := encodeSCIPForTesting(index)
	require.NotEmpty(t, data)

	// Decode back.
	decoded, err := decodeSCIPProtobuf(data)
	require.NoError(t, err)
	require.Len(t, decoded.Documents, 1)

	doc := decoded.Documents[0]
	assert.Equal(t, "main.go", doc.RelativePath)
	require.Len(t, doc.Occurrences, 2)

	// Check definition.
	assert.Equal(t, int32(9), doc.Occurrences[0].Range[0])
	assert.True(t, doc.Occurrences[0].IsDefinition())
	assert.Equal(t, 10, doc.Occurrences[0].StartLine()) // 0-indexed → 1-indexed

	// Check reference.
	assert.False(t, doc.Occurrences[1].IsDefinition())

	// Check symbol info.
	require.Len(t, doc.Symbols, 1)
	assert.Contains(t, doc.Symbols[0].Documentation[0], "func Foo()")
}

func TestSCIPJSONParsing(t *testing.T) {
	tmpDir := t.TempDir()
	jsonFile := filepath.Join(tmpDir, "index.scip")

	jsonData := `{
		"documents": [
			{
				"relative_path": "pkg/handler.go",
				"occurrences": [
					{"range": [4, 5, 4, 11], "symbol": "pkg.Handle()", "symbol_roles": 1},
					{"range": [10, 2, 10, 8], "symbol": "pkg.Handle()", "symbol_roles": 0}
				],
				"symbols": [
					{
						"symbol": "pkg.Handle()",
						"documentation": ["func Handle(w http.ResponseWriter, r *http.Request)"],
						"relationships": [
							{"symbol": "http.Handler", "is_implementation": true}
						]
					}
				]
			}
		]
	}`

	require.NoError(t, os.WriteFile(jsonFile, []byte(jsonData), 0644))

	index, err := ParseSCIPFile(jsonFile)
	require.NoError(t, err)
	require.Len(t, index.Documents, 1)

	doc := index.Documents[0]
	assert.Equal(t, "pkg/handler.go", doc.RelativePath)
	require.Len(t, doc.Occurrences, 2)
	require.Len(t, doc.Symbols, 1)
	require.Len(t, doc.Symbols[0].Relationships, 1)
	assert.True(t, doc.Symbols[0].Relationships[0].IsImplementation)
}

func TestEnrichFromIndex(t *testing.T) {
	logger := zap.NewNop()
	p := NewProvider("scip-go", nil, []string{"go"}, 120, logger)

	g := graph.New()
	g.AddNode(&graph.Node{
		ID: "main.go::Foo", Kind: graph.KindFunction, Name: "Foo",
		FilePath: "main.go", StartLine: 10, EndLine: 20, Language: "go",
	})
	g.AddNode(&graph.Node{
		ID: "main.go::Bar", Kind: graph.KindFunction, Name: "Bar",
		FilePath: "main.go", StartLine: 22, EndLine: 30, Language: "go",
	})

	// Add an INFERRED edge.
	g.AddEdge(&graph.Edge{
		From: "main.go::Bar", To: "main.go::Foo", Kind: graph.EdgeCalls,
		Confidence: 0.7, ConfidenceLabel: "INFERRED",
	})

	// Create a SCIP index that confirms the call.
	index := &SCIPIndex{
		Documents: []SCIPDocument{
			{
				RelativePath: "main.go",
				Occurrences: []SCIPOccurrence{
					{
						Range:       []int32{9, 5, 9, 8}, // line 10 (Foo definition)
						Symbol:      "main.Foo()",
						SymbolRoles: 1, // Definition
					},
					{
						Range:       []int32{21, 5, 21, 8}, // line 22 (Bar definition)
						Symbol:      "main.Bar()",
						SymbolRoles: 1, // Definition
					},
					{
						Range:       []int32{24, 2, 24, 5}, // line 25 (call to Foo from Bar)
						Symbol:      "main.Foo()",
						SymbolRoles: 0, // Reference
					},
				},
				Symbols: []SCIPSymbolInfo{
					{
						Symbol:        "main.Foo()",
						Documentation: []string{"func Foo() error"},
					},
				},
			},
		},
	}

	result := p.enrichFromIndex(g, index, "/tmp/repo")

	assert.Greater(t, result.SymbolsCovered, 0)
	assert.Greater(t, result.EdgesConfirmed, 0)

	// Verify the edge was confirmed.
	edges := g.GetOutEdges("main.go::Bar")
	require.NotEmpty(t, edges)
	for _, e := range edges {
		if e.To == "main.go::Foo" && e.Kind == graph.EdgeCalls {
			assert.Equal(t, 1.0, e.Confidence)
			assert.Equal(t, "EXTRACTED", e.ConfidenceLabel)
		}
	}
}

func TestExtractSymbolName(t *testing.T) {
	tests := []struct {
		symbol string
		want   string
	}{
		{"scip-go gomod github.com/foo/bar v1.0.0 pkg.Foo().", "Foo"},
		{"scip-go gomod github.com/foo/bar v1.0.0 internal/graph/Graph.AddNode().", "AddNode"},
		{"scip-go gomod github.com/foo/bar v1.0.0 internal/graph/Node#Kind.", "Kind"},
		{"", ""},
	}

	for _, tt := range tests {
		got := extractSymbolName(tt.symbol)
		assert.Equal(t, tt.want, got, "for symbol %q", tt.symbol)
	}
}
