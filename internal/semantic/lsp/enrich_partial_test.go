package lsp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/semantic"
)

// providerWithFakeServerParallel mirrors providerWithFakeServer but pins
// maxParallel so tests can force deterministic sequential processing.
func providerWithFakeServerParallel(t *testing.T, server *fakeLSPServer, languages []string, maxParallel int) (*Provider, func()) {
	t.Helper()
	c, serverIn, serverOut, cleanup := newPipedClient(t)

	go server.run(serverIn, serverOut)

	p := NewProvider("fake-lsp", nil, languages, false, maxParallel, zap.NewNop())
	p.client = c // skip ensureClient — the client is already wired.
	p.caps = ServerCapabilities{
		CallHierarchyProvider: true,
		TypeHierarchyProvider: true,
	}
	return p, cleanup
}

// TestLSP_Provider_PartialLandingOnCanceledContext verifies the staged /
// incremental landing contract: when the enrichment context is cancelled
// mid-pass, everything completed before the cut — hierarchy-promoted
// edges and already-served hover stamps — is in the graph and COUNTED on
// the result, the un-visited remainder is skipped, and the result comes
// back Partial with no error. This is the regression test for the
// deadline path that used to discard a fully-buffered pass wholesale.
func TestLSP_Provider_PartialLandingOnCanceledContext(t *testing.T) {
	t.Setenv("GORTEX_LSP_SWEEP", "full") // exercise the full per-file sweep, not the demand-gated default
	repoRoot := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(repoRoot, "main.go"),
		[]byte("package main\n\nfunc Hello() string { return \"\" }\n\nfunc Caller() { _ = Hello() }\n"),
		0o644,
	))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	server := newFakeLSPServer()

	// Call hierarchy: only the Caller position (0-based line 4) yields an
	// item; its outgoing call resolves to Hello. This runs BEFORE hover
	// within the file, so the promoted edge must survive the cut below.
	server.handle("textDocument/prepareCallHierarchy", func(params json.RawMessage) (any, *jsonRPCError) {
		var req struct {
			Position Position `json:"position"`
		}
		_ = json.Unmarshal(params, &req)
		if req.Position.Line != 4 {
			return []CallHierarchyItem{}, nil
		}
		return []CallHierarchyItem{{
			Name: "Caller", Kind: 12,
			URI:            pathToURI(filepath.Join(repoRoot, "main.go")),
			Range:          Range{Start: Position{Line: 4, Character: 0}, End: Position{Line: 4, Character: 30}},
			SelectionRange: Range{Start: Position{Line: 4, Character: 5}, End: Position{Line: 4, Character: 11}},
		}}, nil
	})
	server.handle("callHierarchy/outgoingCalls", func(params json.RawMessage) (any, *jsonRPCError) {
		return []CallHierarchyOutgoingCall{{
			To: CallHierarchyItem{
				Name: "Hello", Kind: 12,
				URI:            pathToURI(filepath.Join(repoRoot, "main.go")),
				Range:          Range{Start: Position{Line: 2, Character: 0}, End: Position{Line: 2, Character: 30}},
				SelectionRange: Range{Start: Position{Line: 2, Character: 5}, End: Position{Line: 2, Character: 10}},
			},
		}}, nil
	})
	server.handle("callHierarchy/incomingCalls", func(params json.RawMessage) (any, *jsonRPCError) {
		return []CallHierarchyIncomingCall{}, nil
	})

	// Hover: the FIRST hover cancels the enrichment context before
	// replying, so exactly one hover completes; every later node must be
	// skipped by the cancellation checks (maxParallel=1 keeps the
	// fan-out sequential).
	var hoverMu sync.Mutex
	hoverCalls := 0
	server.handle("textDocument/hover", func(params json.RawMessage) (any, *jsonRPCError) {
		hoverMu.Lock()
		hoverCalls++
		if hoverCalls == 1 {
			cancel()
		}
		hoverMu.Unlock()
		return map[string]any{
			"contents": map[string]any{"kind": "plaintext", "value": "func X() string"},
		}, nil
	})

	p, cleanup := providerWithFakeServerParallel(t, server, []string{"go"}, 1)
	defer cleanup()

	g := graph.New()
	g.AddNode(&graph.Node{
		ID: "main.go::Hello", Kind: graph.KindFunction, Name: "Hello",
		FilePath: "main.go", StartLine: 3, EndLine: 3, Language: "go",
	})
	g.AddNode(&graph.Node{
		ID: "main.go::Caller", Kind: graph.KindFunction, Name: "Caller",
		FilePath: "main.go", StartLine: 5, EndLine: 5, Language: "go",
	})
	g.AddEdge(&graph.Edge{
		From: "main.go::Caller", To: "main.go::Hello", Kind: graph.EdgeCalls,
		Confidence: 0.7, ConfidenceLabel: "INFERRED", Origin: graph.OriginTextMatched,
	})

	done := make(chan struct{})
	var result *semantic.EnrichResult
	var err error
	go func() {
		defer close(done)
		result, err = p.EnrichRepoContext(ctx, g, "", repoRoot)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("EnrichRepoContext did not return after cancellation")
	}

	require.NoError(t, err, "a cancelled pass must return its partial result, not an error")
	require.NotNil(t, result)
	assert.True(t, result.Partial, "a cancelled pass must be marked partial")
	assert.NotEmpty(t, result.AbortReason)

	// Hierarchy work ran before hover and must have landed + counted:
	// the ambiguous Caller→Hello call edge is promoted to lsp_resolved.
	var call *graph.Edge
	for _, e := range g.GetOutEdges("main.go::Caller") {
		if e.Kind == graph.EdgeCalls && e.To == "main.go::Hello" {
			call = e
			break
		}
	}
	require.NotNil(t, call)
	assert.Equal(t, graph.OriginLSPResolved, call.Origin,
		"hierarchy promotion completed before the cut must survive it")
	assert.Equal(t, 1, result.EdgesConfirmed, "landed hierarchy work must be counted on the partial result")

	// Exactly one hover was served (the one that triggered the cancel);
	// its stamp must be in the graph and counted. The other node was the
	// abandoned remainder.
	hoverMu.Lock()
	served := hoverCalls
	hoverMu.Unlock()
	assert.Equal(t, 1, served, "cancellation must stop scheduling further hovers")
	assert.Equal(t, 1, result.NodesEnriched, "the completed hover must be counted")

	stamped := 0
	for _, id := range []string{"main.go::Hello", "main.go::Caller"} {
		n := g.GetNode(id)
		if n.Meta != nil && n.Meta["semantic_type"] != nil {
			stamped++
			assert.Equal(t, "lsp-fake-lsp", n.Meta["semantic_source"])
		}
	}
	assert.Equal(t, 1, stamped, "exactly the pre-cut hover stamp must be in the graph")
}

// TestLSP_Provider_SkipsNodesWithoutPosition verifies that nodes with
// StartLine < 1 (synthetic module/package nodes) never reach the wire:
// a StartLine-0 node used to produce position.line == -1, which gopls
// rejects per request ("cannot unmarshal number -1 into ...
// position.line of type uint32") — pure wasted round trips.
func TestLSP_Provider_SkipsNodesWithoutPosition(t *testing.T) {
	t.Setenv("GORTEX_LSP_SWEEP", "full") // exercise the full per-file sweep, not the demand-gated default
	repoRoot := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(repoRoot, "main.go"),
		[]byte("package main\n\nfunc F() string { return \"hi\" }\n"),
		0o644,
	))

	server := newFakeLSPServer()
	var mu sync.Mutex
	var positionLines []int
	recordPosition := func(params json.RawMessage) {
		var req struct {
			Position Position `json:"position"`
		}
		_ = json.Unmarshal(params, &req)
		mu.Lock()
		positionLines = append(positionLines, req.Position.Line)
		mu.Unlock()
	}
	server.handle("textDocument/hover", func(params json.RawMessage) (any, *jsonRPCError) {
		recordPosition(params)
		return map[string]any{
			"contents": map[string]any{"kind": "plaintext", "value": "func F() string"},
		}, nil
	})
	server.handle("textDocument/prepareCallHierarchy", func(params json.RawMessage) (any, *jsonRPCError) {
		recordPosition(params)
		return []CallHierarchyItem{}, nil
	})
	server.handle("textDocument/prepareTypeHierarchy", func(params json.RawMessage) (any, *jsonRPCError) {
		recordPosition(params)
		return []TypeHierarchyItem{}, nil
	})
	server.handle("textDocument/implementation", func(params json.RawMessage) (any, *jsonRPCError) {
		recordPosition(params)
		return []Location{}, nil
	})
	server.handle("textDocument/references", func(params json.RawMessage) (any, *jsonRPCError) {
		recordPosition(params)
		return []Location{}, nil
	})

	p, cleanup := providerWithFakeServer(t, server, []string{"go"})
	defer cleanup()

	g := graph.New()
	// Valid node — enriched as usual.
	g.AddNode(&graph.Node{
		ID: "main.go::F", Kind: graph.KindFunction, Name: "F",
		FilePath: "main.go", StartLine: 3, EndLine: 3, Language: "go",
	})
	// Synthetic nodes without a source position: a function (hover +
	// call hierarchy path), an interface (implementations path), and an
	// ambiguous-edge target (references path). None may hit the wire.
	g.AddNode(&graph.Node{
		ID: "main.go::synthetic_fn", Kind: graph.KindFunction, Name: "synthetic_fn",
		FilePath: "main.go", StartLine: 0, EndLine: 0, Language: "go",
	})
	g.AddNode(&graph.Node{
		ID: "main.go::SyntheticIface", Kind: graph.KindInterface, Name: "SyntheticIface",
		FilePath: "main.go", StartLine: 0, EndLine: 0, Language: "go",
	})
	g.AddEdge(&graph.Edge{
		From: "main.go::F", To: "main.go::synthetic_fn", Kind: graph.EdgeCalls,
		Confidence: 0.7, ConfidenceLabel: "INFERRED", Origin: graph.OriginTextMatched,
	})

	done := make(chan error, 1)
	go func() {
		_, err := p.Enrich(g, repoRoot)
		done <- err
	}()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("Enrich timed out")
	}

	mu.Lock()
	defer mu.Unlock()
	require.NotEmpty(t, positionLines, "the valid node should still be queried")
	for _, line := range positionLines {
		assert.GreaterOrEqual(t, line, 0,
			"no request may carry a negative position.line — StartLine<1 nodes must be skipped")
	}

	// The valid node was enriched; the synthetic one was not.
	assert.NotNil(t, g.GetNode("main.go::F").Meta["semantic_type"])
	synth := g.GetNode("main.go::synthetic_fn")
	if synth.Meta != nil {
		assert.Nil(t, synth.Meta["semantic_type"])
	}
}

// TestLSPLine is the unit table for the StartLine → LSP-line conversion.
func TestLSPLine(t *testing.T) {
	cases := []struct {
		name      string
		startLine int
		wantLine  int
		wantOK    bool
	}{
		{"negative", -3, 0, false},
		{"zero (synthetic node)", 0, 0, false},
		{"first line", 1, 0, true},
		{"typical", 42, 41, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			line, ok := lspLine(&graph.Node{StartLine: tc.startLine})
			assert.Equal(t, tc.wantOK, ok)
			assert.Equal(t, tc.wantLine, line)
		})
	}
}
