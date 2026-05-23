package mcp

import (
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
)

// TestToolsSearch_TruncationAdvisesExactNameRetry covers the under-return
// signal: when a broad query matches more deferred tools than max_results,
// the result reports omitted_count and the text advises a select: retry so
// the agent does not silently lose the tool it was looking for.
func TestToolsSearch_TruncationAdvisesExactNameRetry(t *testing.T) {
	t.Setenv("GORTEX_LAZY_TOOLS", "1")
	srv, _ := setupTestServer(t)

	// "memories" matches the whole memory tool family — a deliberately
	// broad query. Capped at 1 result, the rest must be reported.
	result := callToolsSearch(t, srv, map[string]any{
		"query":       "memories",
		"max_results": 1,
		"promote":     false,
	})
	require.False(t, result.IsError)

	body := decodeStructured(t, result)
	require.LessOrEqual(t, len(body.Tools), 1, "max_results must cap the returned schemas")
	require.Positive(t, body.OmittedCount, "a broad query past the cap must report omitted_count")

	text := result.Content[0].(mcplib.TextContent).Text
	require.Contains(t, text, "select:", "truncated result text must advise a select:<exact-name> retry")
	require.Contains(t, text, "more tool(s) matched", "truncated result text must name the under-return")
}

// TestToolsSearch_NoMatchAdvisesExactNameRetry covers the zero-result
// path: an agent that mistyped a keyword is pointed at select:.
func TestToolsSearch_NoMatchAdvisesExactNameRetry(t *testing.T) {
	t.Setenv("GORTEX_LAZY_TOOLS", "1")
	srv, _ := setupTestServer(t)
	result := callToolsSearch(t, srv, map[string]any{"query": "xyzzyplugh"})
	require.False(t, result.IsError)
	text := result.Content[0].(mcplib.TextContent).Text
	require.Contains(t, text, "select:", "a no-match result must still point at the select: escape hatch")
}

// TestToolsSearch_WholeQueryNameMatchRanksFirst pins the ranking audit:
// a query that is a deferred tool's name verbatim surfaces that tool as
// the top hit, even when other tools share one of the query tokens.
func TestToolsSearch_WholeQueryNameMatchRanksFirst(t *testing.T) {
	t.Setenv("GORTEX_LAZY_TOOLS", "1")
	srv, _ := setupTestServer(t)
	require.Contains(t, srv.lazy.DeferredNames(), "taint_paths")

	result := callToolsSearch(t, srv, map[string]any{
		"query":       "taint_paths",
		"max_results": 5,
		"promote":     false,
	})
	require.False(t, result.IsError)

	body := decodeStructured(t, result)
	require.NotEmpty(t, body.Tools)
	require.Equal(t, "taint_paths", body.Tools[0].Name,
		"an exact tool-name query must rank that tool first")
}
