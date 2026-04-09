package eval

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/bridge"
	"github.com/zzet/gortex/internal/graph"
	"go.uber.org/zap"
)

// TestEvalServerLifecycle is an integration test that exercises the full
// eval-server HTTP lifecycle: start → health check → tool call → stats → shutdown.
func TestEvalServerLifecycle(t *testing.T) {
	// --- Setup: build a handler with a graph and a registered tool ---
	g := graph.New()
	g.AddNode(&graph.Node{
		ID:       "main.go::Main",
		Kind:     graph.KindFunction,
		Name:     "Main",
		FilePath: "main.go",
		Language: "go",
	})
	g.AddNode(&graph.Node{
		ID:       "main.go::Helper",
		Kind:     graph.KindFunction,
		Name:     "Helper",
		FilePath: "main.go",
		Language: "go",
	})
	g.AddEdge(&graph.Edge{
		From: "main.go::Main",
		To:   "main.go::Helper",
		Kind: graph.EdgeCalls,
	})

	srv := mcpserver.NewMCPServer("gortex-integration", "0.1.0-test",
		mcpserver.WithToolCapabilities(false),
		mcpserver.WithRecovery(),
	)
	srv.AddTool(
		mcp.NewTool("echo",
			mcp.WithDescription("Echo tool for integration testing"),
			mcp.WithString("message", mcp.Description("Message to echo")),
		),
		func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			args := req.GetArguments()
			msg, _ := args["message"].(string)
			if msg == "" {
				msg = "empty"
			}
			return mcp.NewToolResultText("echo: " + msg), nil
		},
	)

	logger := zap.NewNop()
	handler := NewHandler(srv, g, "0.1.0-test", logger)

	// --- Start: use httptest.NewServer for a real HTTP server ---
	ts := httptest.NewServer(handler)
	defer ts.Close()

	client := ts.Client()

	// --- Step 1: Health check ---
	t.Run("health_check", func(t *testing.T) {
		resp, err := client.Get(ts.URL + "/health")
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()

		assert.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Equal(t, "application/json", resp.Header.Get("Content-Type"))

		var health bridge.HealthResponse
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&health))

		assert.Equal(t, "ok", health.Status)
		assert.True(t, health.Indexed, "graph has nodes so indexed should be true")
		assert.Equal(t, 2, health.Nodes)
		assert.Equal(t, 1, health.Edges)
		assert.Equal(t, "0.1.0-test", health.Version)
		assert.Greater(t, health.UptimeSeconds, float64(0))
	})

	// --- Step 2: Tool call (echo) ---
	t.Run("tool_call_echo", func(t *testing.T) {
		body := `{"arguments":{"message":"integration test"}}`
		resp, err := client.Post(
			ts.URL+"/tool/echo",
			"application/json",
			strings.NewReader(body),
		)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()

		assert.Equal(t, http.StatusOK, resp.StatusCode)

		var toolResp bridge.ToolResponse
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&toolResp))

		assert.False(t, toolResp.IsError)
		require.Len(t, toolResp.Content, 1)
		assert.Equal(t, "text", toolResp.Content[0].Type)
		assert.Contains(t, toolResp.Content[0].Text, "integration test")
	})

	// --- Step 3: Stats endpoint ---
	t.Run("stats", func(t *testing.T) {
		resp, err := client.Get(ts.URL + "/stats")
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()

		assert.Equal(t, http.StatusOK, resp.StatusCode)

		var stats bridge.StatsResponse
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&stats))

		assert.Equal(t, 2, stats.TotalNodes)
		assert.Equal(t, 1, stats.TotalEdges)
		assert.NotNil(t, stats.ByKind)
		assert.NotNil(t, stats.ByLanguage)
	})

	// --- Step 4: Unknown tool returns 404 ---
	t.Run("unknown_tool_404", func(t *testing.T) {
		body := `{"arguments":{}}`
		resp, err := client.Post(
			ts.URL+"/tool/nonexistent",
			"application/json",
			strings.NewReader(body),
		)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()

		assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	})

	// --- Shutdown is implicit: ts.Close() in defer ---
}

// TestIndexCacheRoundTrip is an integration test that exercises the full
// cache lifecycle: create files → store → load → verify integrity → evict.
func TestIndexCacheRoundTrip(t *testing.T) {
	cacheDir := t.TempDir()
	version := "0.2.0-test"

	cache, err := NewCache(cacheDir, version)
	require.NoError(t, err)

	// --- Step 1: Create a realistic index directory with multiple files ---
	indexDir := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(indexDir, "graph.bin"),
		[]byte("serialized-graph-data-with-nodes-and-edges"),
		0o644,
	))
	// Create a nested directory simulating search.bleve/
	bleveDir := filepath.Join(indexDir, "search.bleve")
	require.NoError(t, os.MkdirAll(bleveDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(bleveDir, "store"),
		[]byte("bleve-store-data"),
		0o644,
	))
	require.NoError(t, os.WriteFile(
		filepath.Join(bleveDir, "index_meta.json"),
		[]byte(`{"version":1}`),
		0o644,
	))

	repo := "django/django"
	commit := "a1b2c3d4e5f6"

	// --- Step 2: Verify cache is empty initially ---
	assert.False(t, cache.Check(repo, commit), "cache should be empty initially")

	// --- Step 3: Store the index ---
	require.NoError(t, cache.Store(repo, commit, indexDir))

	// --- Step 4: Verify cache entry exists ---
	assert.True(t, cache.Check(repo, commit), "cache should have entry after store")

	// --- Step 5: Load and verify path ---
	loadedPath, err := cache.Load(repo, commit)
	require.NoError(t, err)
	expectedPath := filepath.Join(cacheDir, CacheKey(repo, commit))
	assert.Equal(t, expectedPath, loadedPath)

	// --- Step 6: Verify all files are intact ---
	graphData, err := os.ReadFile(filepath.Join(loadedPath, "graph.bin"))
	require.NoError(t, err)
	assert.Equal(t, "serialized-graph-data-with-nodes-and-edges", string(graphData))

	bleveStore, err := os.ReadFile(filepath.Join(loadedPath, "search.bleve", "store"))
	require.NoError(t, err)
	assert.Equal(t, "bleve-store-data", string(bleveStore))

	bleveMeta, err := os.ReadFile(filepath.Join(loadedPath, "search.bleve", "index_meta.json"))
	require.NoError(t, err)
	assert.Equal(t, `{"version":1}`, string(bleveMeta))

	// --- Step 7: Validate version compatibility ---
	assert.True(t, cache.Validate(repo, commit), "version should match")

	// --- Step 8: Version mismatch detection ---
	cacheV2, err := NewCache(cacheDir, "0.3.0-different")
	require.NoError(t, err)
	assert.False(t, cacheV2.Validate(repo, commit), "different version should fail validation")

	// --- Step 9: Re-store with updated content ---
	indexDir2 := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(indexDir2, "graph.bin"),
		[]byte("updated-graph-data"),
		0o644,
	))
	require.NoError(t, cache.Store(repo, commit, indexDir2))

	loadedPath2, err := cache.Load(repo, commit)
	require.NoError(t, err)
	updatedData, err := os.ReadFile(filepath.Join(loadedPath2, "graph.bin"))
	require.NoError(t, err)
	assert.Equal(t, "updated-graph-data", string(updatedData))

	// --- Step 10: Evict and verify removal ---
	require.NoError(t, cache.Evict(repo, commit))
	assert.False(t, cache.Check(repo, commit), "cache should be empty after eviction")

	_, err = cache.Load(repo, commit)
	assert.Error(t, err, "load after eviction should fail")
}
