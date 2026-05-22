package mcp

import (
	"strings"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// manifestEntries pulls the context_manifest entry list out of a
// smart_context JSON result.
func manifestEntries(t *testing.T, m map[string]any) (map[string]any, []map[string]any) {
	t.Helper()
	mani, ok := m["context_manifest"].(map[string]any)
	require.True(t, ok, "graded fidelity must add a context_manifest, got keys: %v", keysOf(m))
	raw, ok := mani["entries"].([]any)
	require.True(t, ok, "manifest must carry an entries list")
	entries := make([]map[string]any, 0, len(raw))
	for _, r := range raw {
		e, ok := r.(map[string]any)
		require.True(t, ok, "each manifest entry must be an object")
		entries = append(entries, e)
	}
	return mani, entries
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestSmartContext_GradedFidelity(t *testing.T) {
	srv, _ := setupCompressTestServer(t)

	r := callTool(t, srv, "smart_context", map[string]any{
		"task":     "validate token and parse claims",
		"fidelity": "graded",
	})
	m := extractTextResult(t, r)
	mani, entries := manifestEntries(t, m)
	require.NotEmpty(t, entries, "manifest entries must not be empty")

	budget, _ := mani["token_budget"].(float64)
	used, _ := mani["tokens_used"].(float64)
	assert.Equal(t, float64(defaultManifestBudget), budget, "default budget must apply")
	assert.LessOrEqual(t, used, budget, "tokens_used must stay within budget")

	tiers := map[string]int{}
	for _, e := range entries {
		tier, _ := e["tier"].(string)
		require.Contains(t, []string{"focus", "ring", "outline"}, tier,
			"every entry must carry a known tier")
		tiers[tier]++
		// A focus entry must carry full source; ring entries are
		// flagged compressed.
		if tier == "ring" {
			if _, hasSrc := e["source"].(string); hasSrc {
				assert.Equal(t, true, e["compressed"],
					"ring-tier source must be flagged compressed")
			}
		}
	}
	assert.Positive(t, tiers["focus"], "at least one focus entry expected")
}

func TestSmartContext_FlatModeHasNoManifest(t *testing.T) {
	srv, _ := setupCompressTestServer(t)
	r := callTool(t, srv, "smart_context", map[string]any{
		"task": "validate token and parse claims",
	})
	m := extractTextResult(t, r)
	_, has := m["context_manifest"]
	assert.False(t, has, "flat mode must not emit a context_manifest")
	// The legacy relevant_symbols field is still present.
	_, hasSyms := m["relevant_symbols"]
	assert.True(t, hasSyms, "flat mode keeps relevant_symbols")
}

func TestSmartContext_GradedBudgetRespected(t *testing.T) {
	srv, _ := setupCompressTestServer(t)
	for _, budget := range []int{40, 200, 8000} {
		r := callTool(t, srv, "smart_context", map[string]any{
			"task":         "validate token and parse claims",
			"fidelity":     "graded",
			"token_budget": float64(budget),
		})
		m := extractTextResult(t, r)
		mani, _ := manifestEntries(t, m)
		used, _ := mani["tokens_used"].(float64)
		assert.LessOrEqualf(t, used, float64(budget),
			"tokens_used %v must stay within token_budget %d", used, budget)
		assert.Equal(t, float64(budget), mani["token_budget"], "echoed budget must match the request")
	}
}

func TestSmartContext_GradedGCX(t *testing.T) {
	srv, _ := setupCompressTestServer(t)
	r := callTool(t, srv, "smart_context", map[string]any{
		"task":     "validate token and parse claims",
		"fidelity": "graded",
		"format":   "gcx",
	})
	require.False(t, r.IsError, "unexpected error: %+v", r.Content)
	require.NotEmpty(t, r.Content)
	tc, ok := r.Content[0].(mcplib.TextContent)
	require.True(t, ok, "expected TextContent, got %T", r.Content[0])
	assert.True(t, strings.Contains(tc.Text, "smart_context.manifest"),
		"GCX output must carry the manifest section, got:\n%s", tc.Text)
}

func TestSmartContext_EstimateFlat(t *testing.T) {
	srv, _ := setupCompressTestServer(t)
	r := callTool(t, srv, "smart_context", map[string]any{
		"task":     "validate token and parse claims",
		"estimate": true,
	})
	m := extractTextResult(t, r)
	// Estimate mode short-circuits: no payload, just the projection.
	_, hasSyms := m["relevant_symbols"]
	assert.False(t, hasSyms, "estimate mode must not return the symbol payload")
	est, ok := m["estimate"].(map[string]any)
	require.True(t, ok, "estimate mode must return an estimate object")
	assert.Equal(t, "flat", est["fidelity"])
	proj, _ := est["projected_tokens"].(float64)
	assert.Positive(t, proj, "projected_tokens must be a positive estimate")
}

func TestSmartContext_EstimateGraded(t *testing.T) {
	srv, _ := setupCompressTestServer(t)
	r := callTool(t, srv, "smart_context", map[string]any{
		"task":     "validate token and parse claims",
		"fidelity": "graded",
		"estimate": true,
	})
	m := extractTextResult(t, r)
	_, hasMani := m["context_manifest"]
	assert.False(t, hasMani, "estimate mode must not return the manifest payload")
	est, ok := m["estimate"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "graded", est["fidelity"])
	assert.Equal(t, float64(defaultManifestBudget), est["token_budget"])
	proj, _ := est["projected_tokens"].(float64)
	budget, _ := est["token_budget"].(float64)
	assert.LessOrEqual(t, proj, budget, "projected tokens must fit the budget")
	for _, k := range []string{"focus", "ring", "outline"} {
		_, ok := est[k].(float64)
		assert.Truef(t, ok, "graded estimate must report a %s count", k)
	}
}

func TestSmartContext_EstimateMatchesManifest(t *testing.T) {
	srv, _ := setupCompressTestServer(t)
	args := map[string]any{
		"task":         "validate token and parse claims",
		"fidelity":     "graded",
		"token_budget": float64(500),
	}
	full := extractTextResult(t, callTool(t, srv, "smart_context", args))
	mani := full["context_manifest"].(map[string]any)
	realUsed, _ := mani["tokens_used"].(float64)

	estArgs := map[string]any{"estimate": true}
	for k, v := range args {
		estArgs[k] = v
	}
	est := extractTextResult(t, callTool(t, srv, "smart_context", estArgs))["estimate"].(map[string]any)
	estProj, _ := est["projected_tokens"].(float64)

	assert.Equal(t, realUsed, estProj,
		"estimate projected_tokens must match the real manifest tokens_used")
}

func TestSmartContext_EstimateGCX(t *testing.T) {
	srv, _ := setupCompressTestServer(t)
	r := callTool(t, srv, "smart_context", map[string]any{
		"task":     "validate token and parse claims",
		"estimate": true,
		"format":   "gcx",
	})
	require.False(t, r.IsError, "unexpected error: %+v", r.Content)
	require.NotEmpty(t, r.Content)
	tc, ok := r.Content[0].(mcplib.TextContent)
	require.True(t, ok, "expected TextContent, got %T", r.Content[0])
	assert.Contains(t, tc.Text, "smart_context.estimate",
		"GCX estimate output must carry the estimate section")
}
