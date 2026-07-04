package mcp

import (
	"encoding/json"
	"strings"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
)

func TestParseAssistMode(t *testing.T) {
	cases := []struct {
		in   string
		want assistMode
	}{
		{"", assistAuto},
		{"auto", assistAuto},
		{"AUTO", assistAuto},
		{"  auto ", assistAuto},
		{"on", assistOn},
		{"ON", assistOn},
		{"yes", assistOn},
		{"true", assistOn},
		{"force", assistOn},
		{"off", assistOff},
		{"OFF", assistOff},
		{"no", assistOff},
		{"false", assistOff},
		{"skip", assistOff},
		{"deep", assistDeep},
		{"DEEP", assistDeep},
		{"verify", assistDeep},
		{"body", assistDeep},
		{"garbage", assistAuto},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			req := mcplib.CallToolRequest{}
			req.Params.Arguments = map[string]any{"assist": tc.in}
			got := parseAssistMode(req)
			assert.Equal(t, tc.want, got, "input=%q", tc.in)
		})
	}
}

func TestLooksNaturalLanguage(t *testing.T) {
	cases := []struct {
		name string
		q    string
		want bool
	}{
		{"empty", "", false},
		{"blanks", "   ", false},
		{"single token", "handler", false},
		{"two tokens", "handle user", false},

		{"qualified identifier", "pkg/foo bar baz", false},
		{"camelCase token", "handleSomething for fun", false},
		{"PascalCase token", "MyHandler tests pass", false},
		{"dotted identifier", "foo.Bar baz qux", false},
		{"snake_case identifier", "do_thing in cluster", false},
		{"scoped identifier", "ns::Type does stuff", false},

		{"NL with stop word", "where do we hash passwords", true},
		{"NL plain 4 tokens", "validate token auth flow", true},
		{"NL plain 3 tokens no stop word", "validate token auth", false},
		{"NL with the", "the user login flow", true},

		{"mixed identifier short-circuits stop word", "the handleAsk token", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := looksNaturalLanguage(tc.q)
			assert.Equal(t, tc.want, got, "q=%q", tc.q)
		})
	}
}

func TestShouldEngageAssist(t *testing.T) {
	// `on` always engages, regardless of shape.
	assert.True(t, shouldEngageAssist(assistOn, "Foo"))
	assert.True(t, shouldEngageAssist(assistOn, ""))

	// `off` never engages.
	assert.False(t, shouldEngageAssist(assistOff, "where do we hash"))
	assert.False(t, shouldEngageAssist(assistOff, ""))

	// `auto` defers to the heuristic.
	assert.False(t, shouldEngageAssist(assistAuto, "handleAsk"))
	assert.True(t, shouldEngageAssist(assistAuto, "where do we hash"))

	// `deep` always engages — its whole purpose is opt-in verification
	// for cases the caller knows are NL queries.
	assert.True(t, shouldEngageAssist(assistDeep, "Foo"))
	assert.True(t, shouldEngageAssist(assistDeep, "where do we hash"))
}

// TestDeferColdLoadForAssist covers the enrichment-aware first-load
// gate: an implicit assist request defers its local-model cold load
// only when the model is not yet loaded AND enrichment is in flight.
func TestDeferColdLoadForAssist(t *testing.T) {
	cases := []struct {
		name         string
		engage       bool
		modelLoaded  bool
		enrichBusy   bool
		wantEngage   bool
		wantDeferred bool
	}{
		{"not engaging stays off", false, false, true, false, false},
		{"loaded model ignores busy", true, true, true, true, false},
		{"not busy proceeds", true, false, false, true, false},
		{"unloaded + busy defers", true, false, true, false, true},
		{"loaded + not busy proceeds", true, true, false, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			engage, deferred := deferColdLoadForAssist(tc.engage, tc.modelLoaded, tc.enrichBusy)
			assert.Equal(t, tc.wantEngage, engage, "engage")
			assert.Equal(t, tc.wantDeferred, deferred, "deferred")
			// Deferral and engagement are mutually exclusive.
			assert.False(t, engage && deferred, "engage and deferred must never both be true")
		})
	}
}

func TestTruncateBody(t *testing.T) {
	cases := []struct {
		name     string
		src      string
		maxLines int
		maxChars int
		want     string
	}{
		{"empty", "", 8, 600, ""},
		{
			"under both caps",
			"a()\nb()\nc()",
			8, 600,
			"a()\nb()\nc()\n",
		},
		{
			"blank lines skipped from line count",
			"a()\n\nb()\n\nc()\n",
			3, 600,
			"a()\n\nb()\n\nc()\n…\n",
		},
		{
			"line cap fires",
			"l1\nl2\nl3\nl4\nl5",
			3, 600,
			"l1\nl2\nl3\n…\n",
		},
		{
			"char cap fires after line cap",
			strings.Repeat("X", 700),
			8, 100,
			strings.Repeat("X", 100) + "…\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := truncateBody(tc.src, tc.maxLines, tc.maxChars)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestHasMixedCase(t *testing.T) {
	assert.False(t, hasMixedCase("lower"))
	assert.False(t, hasMixedCase("UPPER"))
	assert.False(t, hasMixedCase(""))
	assert.False(t, hasMixedCase("123"))
	assert.True(t, hasMixedCase("camelCase"))
	assert.True(t, hasMixedCase("PascalCase"))
}

func TestHasStopWord(t *testing.T) {
	assert.True(t, hasStopWord([]string{"hello", "where", "world"}))
	assert.True(t, hasStopWord([]string{"WHERE", "is", "x"}))
	assert.False(t, hasStopWord([]string{"validate", "token", "auth"}))
	assert.False(t, hasStopWord(nil))
}

// TestFetchAndMergeBM25_DedupesAcrossTerms verifies that when the
// same node matches multiple terms, it appears only once and keeps
// its primary-term position.
func TestFetchAndMergeBM25_DedupesAcrossTerms(t *testing.T) {
	srv, _ := setupTestServer(t)
	scope := query.QueryOptions{}

	// Primary term that hits "helper".
	primary := srv.engine.SearchSymbolsScoped("helper", 20, scope)
	require.NotEmpty(t, primary)

	// Merging with the same term as an "expansion" must produce the
	// same list, not duplicates.
	merged, primaryCount := fetchAndMergeBM25(srv.engine, "helper", []string{"helper"}, 20, scope)
	assert.Equal(t, len(primary), primaryCount)
	assert.Equal(t, idsOf(primary), idsOf(merged))
}

// TestFetchAndMergeBM25_CombinedQueryUnionIsSuperset is the load-bearing
// guard for the "combine expansion terms into one BM25 query"
// optimisation. The merged result MUST contain at least every node
// that a per-term fan-out would have returned — otherwise switching
// from N BM25 calls to (primary + combined) drops candidates the
// rerank pipeline used to see. Exact-name rescue (the per-fragment
// FindNodesByNames step) is what makes this hold for tokenisation
// edge cases like PascalCase concatenated names that BM25 misses.
func TestFetchAndMergeBM25_CombinedQueryUnionIsSuperset(t *testing.T) {
	srv, _ := setupTestServer(t)
	scope := query.QueryOptions{}

	// Per-term fan-out (the OLD behaviour). For each fragment, run
	// the engine search separately and collect every distinct node ID
	// it surfaces — this is the worst-case "no candidate may be
	// dropped by collapsing into one query" set.
	terms := []string{"helper", "main"}
	unionExpected := map[string]bool{}
	for _, t := range terms {
		for _, n := range srv.engine.SearchSymbolsScoped(t, 20, scope) {
			unionExpected[n.ID] = true
		}
	}
	require.NotEmpty(t, unionExpected, "per-term fan-out produced nothing — test corpus drifted")

	// New behaviour: primary + combined-OR + per-fragment exact-name
	// rescue, all driven by fetchAndMergeBM25.
	merged, _ := fetchAndMergeBM25(srv.engine, terms[0], terms[1:], 20, scope)
	mergedSet := map[string]bool{}
	for _, n := range merged {
		mergedSet[n.ID] = true
	}

	for id := range unionExpected {
		require.True(t, mergedSet[id], "merged result missing per-term hit %q", id)
	}
}

// TestFetchAndMergeBM25_ExactNameRescuePreserved is the regression
// guard for the soup-mode + PascalCase fragment case that per-term
// fan-out used to handle implicitly. When BM25 tokenisation misses
// a fragment ("BillingInvoice" tokenises to one term `billinginvoice`
// which the camelCase-split index doesn't carry), the per-fragment
// FindNodesByNames rescue MUST still surface its exact-name node.
// This mirrors the failure mode TestSearchSymbols_PathScoping caught
// when soup-split fragments first went through the combined query
// path.
func TestFetchAndMergeBM25_ExactNameRescuePreserved(t *testing.T) {
	srv, _ := setupTestServer(t)

	// The test corpus carries no PascalCase-concatenated names by
	// default, so add three synthetic ones — these never reach BM25
	// (we don't re-index it for the test) but they are what the
	// rescue step has to surface.
	for path, name := range map[string]string{
		"svc/billing/Invoice.go": "BillingInvoice",
		"svc/auth/Login.go":      "AuthLogin",
		"libs/money/Amount.go":   "MoneyAmount",
	} {
		id := path + "::" + name
		srv.graph.AddNode(&graph.Node{
			ID: id, Kind: graph.KindFunction, Name: name,
			FilePath: path, StartLine: 1, EndLine: 5, Language: "go",
		})
	}

	terms := []string{"BillingInvoice", "AuthLogin", "MoneyAmount"}
	merged, _ := fetchAndMergeBM25(srv.engine, terms[0], terms[1:], 20, query.QueryOptions{})

	mergedNames := map[string]bool{}
	for _, n := range merged {
		mergedNames[n.Name] = true
	}
	for _, want := range terms {
		require.True(t, mergedNames[want], "exact-name rescue dropped %q from merged result", want)
	}
}

// TestFetchAndMergeBM25_AppendsNewMatches verifies that expansion
// terms bring in additional candidates the primary term missed.
func TestFetchAndMergeBM25_AppendsNewMatches(t *testing.T) {
	srv, _ := setupTestServer(t)
	scope := query.QueryOptions{}

	primary := srv.engine.SearchSymbolsScoped("helper", 20, scope)
	merged, primaryCount := fetchAndMergeBM25(srv.engine, "helper", []string{"main"}, 20, scope)
	assert.Equal(t, len(primary), primaryCount)

	primaryIDs := idsOf(primary)
	mergedIDs := idsOf(merged)

	// Every primary ID appears in the merged set, in primary order
	// at the head.
	require.GreaterOrEqual(t, len(mergedIDs), len(primaryIDs))
	for i, id := range primaryIDs {
		assert.Equal(t, id, mergedIDs[i], "primary order broken at index %d", i)
	}
	// The merge brought in at least one "main"-matched node.
	assert.Greater(t, len(mergedIDs), len(primaryIDs))
}

// TestSearchSymbols_AssistArgPassThrough verifies the new assist arg
// parses and doesn't break the no-LLM path. Without a service the
// gate always reads as "no engage" regardless of mode, so results
// match the no-assist baseline exactly.
func TestSearchSymbols_AssistArgPassThrough(t *testing.T) {
	srv, _ := setupTestServer(t)

	for _, mode := range []string{"", "auto", "on", "off"} {
		t.Run("assist="+mode, func(t *testing.T) {
			args := map[string]any{"query": "helper"}
			if mode != "" {
				args["assist"] = mode
			}
			result := callTool(t, srv, "search_symbols", args)
			require.False(t, result.IsError, "search failed for mode=%q", mode)
			text := result.Content[0].(mcplib.TextContent).Text
			var resp map[string]any
			require.NoError(t, json.Unmarshal([]byte(text), &resp))
			results := resp["results"].([]any)
			require.NotEmpty(t, results, "no results for mode=%q", mode)
		})
	}
}

func TestPrioritizeCallables(t *testing.T) {
	// Mixed input: BM25-ranked, with callable kinds interleaved among
	// param/field/type nodes. Expected output: callables in their
	// original order, then everything else in its original order.
	nodes := []*graph.Node{
		{ID: "p1", Kind: graph.KindParam},
		{ID: "f1", Kind: graph.KindFunction},
		{ID: "fld1", Kind: graph.KindField},
		{ID: "m1", Kind: graph.KindMethod},
		{ID: "t1", Kind: graph.KindType},
		{ID: "f2", Kind: graph.KindFunction},
	}
	got := prioritizeCallables(nodes)
	want := []string{"f1", "m1", "f2", "p1", "fld1", "t1"}
	gotIDs := idsOf(got)
	if len(gotIDs) != len(want) {
		t.Fatalf("length mismatch: got=%v want=%v", gotIDs, want)
	}
	for i := range want {
		if gotIDs[i] != want[i] {
			t.Errorf("position %d: got=%q want=%q", i, gotIDs[i], want[i])
		}
	}
}

func TestPrioritizeCallables_AllCallable(t *testing.T) {
	nodes := []*graph.Node{
		{ID: "a", Kind: graph.KindFunction},
		{ID: "b", Kind: graph.KindMethod},
	}
	got := prioritizeCallables(nodes)
	if got[0].ID != "a" || got[1].ID != "b" {
		t.Fatalf("order changed when no reordering needed: %v", idsOf(got))
	}
}

func TestPrioritizeCallables_NoCallable(t *testing.T) {
	nodes := []*graph.Node{
		{ID: "a", Kind: graph.KindParam},
		{ID: "b", Kind: graph.KindField},
	}
	got := prioritizeCallables(nodes)
	if got[0].ID != "a" || got[1].ID != "b" {
		t.Fatalf("order changed when no callables present: %v", idsOf(got))
	}
}

func idsOf(nodes []*graph.Node) []string {
	out := make([]string, len(nodes))
	for i, n := range nodes {
		out[i] = n.ID
	}
	return out
}
