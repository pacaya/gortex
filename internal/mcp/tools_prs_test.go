package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/forge"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
)

// prToolsTestServer builds a server over a synthetic graph: a security-
// sensitive hub function (internal/auth/login.go) with many inbound
// callers and no covering test, so the file→symbol join and PR-risk score
// have real signal. Returns the server and the changed file path.
func prToolsTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	g := graph.New()
	file := "internal/auth/login.go"
	hubID := file + "::ValidateToken"
	g.AddNode(&graph.Node{ID: hubID, Kind: graph.KindFunction, Name: "ValidateToken", FilePath: file, StartLine: 1, EndLine: 10})
	for i := 0; i < 12; i++ {
		cid := "pkg/c.go::caller" + strconv.Itoa(i)
		g.AddNode(&graph.Node{ID: cid, Kind: graph.KindFunction, Name: "caller" + strconv.Itoa(i), FilePath: "pkg/c.go"})
		g.AddEdge(&graph.Edge{From: cid, To: hubID, Kind: graph.EdgeCalls})
	}
	srv := NewServer(query.NewEngine(g), g, nil, nil, zap.NewNop(), nil)
	return srv, file
}

// withSeams swaps the forge func-var seam for the duration of the test so
// no network call is made; the originals are restored on cleanup.
func withSeams(t *testing.T, list func(context.Context, string, forge.ListOpts) ([]forge.PR, error), files func(context.Context, string, int) ([]string, error)) {
	t.Helper()
	origList, origFiles := forgeList, forgeFiles
	if list != nil {
		forgeList = list
	}
	if files != nil {
		forgeFiles = files
	}
	t.Cleanup(func() { forgeList, forgeFiles = origList, origFiles })
}

func callPRTool(t *testing.T, srv *Server, name string, h func(context.Context, mcplib.CallToolRequest) (*mcplib.CallToolResult, error), args map[string]any) *mcplib.CallToolResult {
	t.Helper()
	req := mcplib.CallToolRequest{}
	req.Params.Name = name
	req.Params.Arguments = args
	res, err := h(t.Context(), req)
	require.NoError(t, err)
	return res
}

func unmarshalPRResult(t *testing.T, res *mcplib.CallToolResult, v any) {
	t.Helper()
	require.NoError(t, json.Unmarshal([]byte(res.Content[0].(mcplib.TextContent).Text), v))
}

// --- get_pr_impact: supplied files, no forge call --------------------------

func TestGetPRImpact_SuppliedFilesNoForge(t *testing.T) {
	srv, file := prToolsTestServer(t)

	// Both seams panic if hit — the supplied-files path must not touch them.
	seamHit := false
	withSeams(t,
		func(context.Context, string, forge.ListOpts) ([]forge.PR, error) { seamHit = true; return nil, nil },
		func(context.Context, string, int) ([]string, error) { seamHit = true; return nil, nil },
	)

	filesJSON, _ := json.Marshal([]string{file})
	res := callPRTool(t, srv, "get_pr_impact", srv.handleGetPRImpact, map[string]any{
		"number": float64(7),
		"files":  string(filesJSON),
	})
	require.False(t, res.IsError, "errored: %v", res)
	require.False(t, seamHit, "the forge seam must NOT be called when files are supplied")

	var out struct {
		Number         int     `json:"number"`
		Risk           string  `json:"risk"`
		Score          float64 `json:"score"`
		ChangedFiles   []string `json:"changed_files"`
		ChangedSymbols []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"changed_symbols"`
		Blast map[string]any `json:"blast"`
		ReviewPriorities []struct {
			Axis  string  `json:"axis"`
			Score float64 `json:"score"`
		} `json:"review_priorities"`
	}
	unmarshalPRResult(t, res, &out)

	require.Equal(t, 7, out.Number)
	require.Contains(t, out.ChangedFiles, file)
	// The file→symbol join found the hub.
	require.NotEmpty(t, out.ChangedSymbols)
	foundHub := false
	for _, s := range out.ChangedSymbols {
		if s.Name == "ValidateToken" {
			foundHub = true
		}
	}
	require.True(t, foundHub, "expected ValidateToken in the changed-symbol join")
	// Security path + 12-caller untested hub scores at least HIGH.
	require.Contains(t, []string{"HIGH", "CRITICAL"}, out.Risk)
	// Blast grouping is present (callers grouped by file).
	require.NotNil(t, out.Blast)
	require.Contains(t, out.Blast, "callers_by_file")
	// review_priorities sorted descending.
	for i := 1; i < len(out.ReviewPriorities); i++ {
		require.GreaterOrEqual(t, out.ReviewPriorities[i-1].Score, out.ReviewPriorities[i].Score)
	}
}

func TestGetPRImpact_ReceiptEmitted(t *testing.T) {
	srv, file := prToolsTestServer(t)
	withSeams(t,
		func(context.Context, string, forge.ListOpts) ([]forge.PR, error) { t.Fatal("list seam hit"); return nil, nil },
		func(context.Context, string, int) ([]string, error) { t.Fatal("files seam hit"); return nil, nil },
	)
	filesJSON, _ := json.Marshal([]string{file})
	res := callPRTool(t, srv, "get_pr_impact", srv.handleGetPRImpact, map[string]any{
		"number":  float64(7),
		"files":   string(filesJSON),
		"receipt": true,
	})
	require.False(t, res.IsError, "errored: %v", res)

	var out struct {
		Receipt *struct {
			ReceiptVersion int    `json:"receipt_version"`
			RiskTier       string `json:"risk_tier"`
			NextSafeAction string `json:"next_safe_action"`
		} `json:"receipt"`
	}
	unmarshalPRResult(t, res, &out)
	require.NotNil(t, out.Receipt, "receipt:true must emit a receipt block")
	require.Equal(t, 1, out.Receipt.ReceiptVersion)
	require.NotEmpty(t, out.Receipt.RiskTier)
	require.NotEmpty(t, out.Receipt.NextSafeAction)

	// receipt absent by default.
	res2 := callPRTool(t, srv, "get_pr_impact", srv.handleGetPRImpact, map[string]any{
		"number": float64(7),
		"files":  string(filesJSON),
	})
	var out2 map[string]any
	unmarshalPRResult(t, res2, &out2)
	_, has := out2["receipt"]
	require.False(t, has, "receipt must be absent when not requested")
}

func TestGetPRImpact_RequiresNumber(t *testing.T) {
	srv, file := prToolsTestServer(t)
	filesJSON, _ := json.Marshal([]string{file})
	res := callPRTool(t, srv, "get_pr_impact", srv.handleGetPRImpact, map[string]any{
		"files": string(filesJSON),
	})
	require.True(t, res.IsError, "expected an error when number is missing")
}

// --- list_prs: classification ----------------------------------------------

func TestListPRs_ClassifiesSupplied(t *testing.T) {
	srv, _ := prToolsTestServer(t)
	withSeams(t,
		func(context.Context, string, forge.ListOpts) ([]forge.PR, error) { t.Fatal("list seam hit"); return nil, nil },
		nil,
	)

	prs := []forge.PR{
		{Number: 1, Title: "draft work", Author: "a", BaseRef: "main", IsDraft: true},
		{Number: 2, Title: "ready", Author: "b", BaseRef: "main", ReviewDecision: "APPROVED", CIRollup: "SUCCESS"},
		{Number: 3, Title: "needs work", Author: "c", BaseRef: "main", ReviewDecision: "CHANGES_REQUESTED", CIRollup: "FAILURE"},
	}
	prsJSON, _ := json.Marshal(prs)
	res := callPRTool(t, srv, "list_prs", srv.handleListPRs, map[string]any{"prs": string(prsJSON)})
	require.False(t, res.IsError, "errored: %v", res)

	var out struct {
		Total int `json:"total"`
		PRs   []struct {
			Number   int      `json:"number"`
			CI       string   `json:"ci"`
			State    string   `json:"state"`
			Blockers []string `json:"blockers"`
		} `json:"prs"`
	}
	unmarshalPRResult(t, res, &out)
	require.Equal(t, 3, out.Total)
	byNum := map[int]string{}
	ciByNum := map[int]string{}
	for _, p := range out.PRs {
		byNum[p.Number] = p.State
		ciByNum[p.Number] = p.CI
	}
	require.Equal(t, "DRAFT", byNum[1])
	require.Equal(t, "APPROVED", byNum[2])
	require.Equal(t, "SUCCESS", ciByNum[2])
	require.Equal(t, "CHANGES_REQUESTED", byNum[3])
	require.Equal(t, "FAILURE", ciByNum[3])
}

// --- triage_prs: sorted by score desc --------------------------------------

func TestTriagePRs_SortedDescending(t *testing.T) {
	srv, hubFile := prToolsTestServer(t)
	// PR #1 touches a low-risk file with no symbols; PR #2 touches the
	// security hub. Supplied via the files map so no forge call happens.
	prs := []forge.PR{
		{Number: 1, Title: "low", Author: "a"},
		{Number: 2, Title: "high", Author: "b"},
	}
	prsJSON, _ := json.Marshal(prs)
	filesMap := map[string][]string{
		"1": {"pkg/unrelated.go"},
		"2": {hubFile},
	}
	filesJSON, _ := json.Marshal(filesMap)

	withSeams(t,
		func(context.Context, string, forge.ListOpts) ([]forge.PR, error) { t.Fatal("list seam hit"); return nil, nil },
		func(context.Context, string, int) ([]string, error) { t.Fatal("files seam hit"); return nil, nil },
	)

	res := callPRTool(t, srv, "triage_prs", srv.handleTriagePRs, map[string]any{
		"prs":   string(prsJSON),
		"files": string(filesJSON),
	})
	require.False(t, res.IsError, "errored: %v", res)

	var out struct {
		Total  int `json:"total"`
		Ranked []struct {
			Number int     `json:"number"`
			Score  float64 `json:"score"`
		} `json:"ranked"`
	}
	unmarshalPRResult(t, res, &out)
	require.Equal(t, 2, out.Total)
	require.Len(t, out.Ranked, 2)
	// Sorted by score descending: the security hub PR (#2) ranks first.
	for i := 1; i < len(out.Ranked); i++ {
		require.GreaterOrEqual(t, out.Ranked[i-1].Score, out.Ranked[i].Score, "ranked must be score-descending")
	}
	require.Equal(t, 2, out.Ranked[0].Number, "the higher-risk PR must rank first")
}

// --- forge-unavailable: no token, no supplied data -------------------------

func TestPRTools_ForgeUnavailable(t *testing.T) {
	// Strip any token from the environment so forge.Available == false.
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_ENTERPRISE_TOKEN", "")

	srv, _ := prToolsTestServer(t)
	seamHit := false
	withSeams(t,
		func(context.Context, string, forge.ListOpts) ([]forge.PR, error) { seamHit = true; return nil, nil },
		func(context.Context, string, int) ([]string, error) { seamHit = true; return nil, nil },
	)

	cases := []struct {
		name string
		h    func(context.Context, mcplib.CallToolRequest) (*mcplib.CallToolResult, error)
		args map[string]any
	}{
		{"list_prs", srv.handleListPRs, map[string]any{}},
		{"get_pr_impact", srv.handleGetPRImpact, map[string]any{"number": float64(5)}},
		{"triage_prs", srv.handleTriagePRs, map[string]any{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := callPRTool(t, srv, c.name, c.h, c.args)
			require.False(t, res.IsError, "must degrade, not error")
			var out struct {
				Error string `json:"error"`
				Hint  string `json:"hint"`
			}
			unmarshalPRResult(t, res, &out)
			require.Equal(t, "forge unavailable", out.Error)
			require.Contains(t, out.Hint, "GH_TOKEN")
		})
	}
	require.False(t, seamHit, "an unavailable forge must short-circuit before the seam")
}

// --- rate limited: the seam returns forge.ErrRateLimited -------------------

func TestPRTools_RateLimited(t *testing.T) {
	// A token must resolve so forge.Available == true and the handler
	// proceeds to call the seam, which simulates a rate-limit.
	t.Setenv("GH_TOKEN", "x-fake-token")

	srv, _ := prToolsTestServer(t)
	rlErr := fmt.Errorf("%w (retry after 42s)", forge.ErrRateLimited)
	withSeams(t,
		func(context.Context, string, forge.ListOpts) ([]forge.PR, error) { return nil, rlErr },
		func(context.Context, string, int) ([]string, error) { return nil, rlErr },
	)

	cases := []struct {
		name string
		h    func(context.Context, mcplib.CallToolRequest) (*mcplib.CallToolResult, error)
		args map[string]any
	}{
		{"list_prs", srv.handleListPRs, map[string]any{}},
		{"get_pr_impact", srv.handleGetPRImpact, map[string]any{"number": float64(5)}},
		{"triage_prs", srv.handleTriagePRs, map[string]any{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := callPRTool(t, srv, c.name, c.h, c.args)
			require.False(t, res.IsError, "must degrade, not error")
			var out struct {
				Error      string `json:"error"`
				RetryAfter int    `json:"retry_after_s"`
			}
			unmarshalPRResult(t, res, &out)
			require.Equal(t, "rate limited", out.Error)
			require.Equal(t, 42, out.RetryAfter, "retry_after_s parsed from the wrapped hint")
		})
	}
}

// --- gcx / toon / max_bytes round-trip -------------------------------------

func TestPRTools_GCXTOONBudget(t *testing.T) {
	srv, file := prToolsTestServer(t)
	filesJSON, _ := json.Marshal([]string{file})
	prsJSON, _ := json.Marshal([]forge.PR{{Number: 1, Title: "t", Author: "a", BaseRef: "main"}})

	// list_prs GCX.
	g1 := callPRTool(t, srv, "list_prs", srv.handleListPRs, map[string]any{"prs": string(prsJSON), "format": "gcx"})
	require.False(t, g1.IsError)
	require.Contains(t, g1.Content[0].(mcplib.TextContent).Text, "list_prs.summary")
	require.Contains(t, g1.Content[0].(mcplib.TextContent).Text, "list_prs.prs")

	// get_pr_impact GCX.
	g2 := callPRTool(t, srv, "get_pr_impact", srv.handleGetPRImpact, map[string]any{"number": float64(1), "files": string(filesJSON), "format": "gcx"})
	require.False(t, g2.IsError)
	require.Contains(t, g2.Content[0].(mcplib.TextContent).Text, "get_pr_impact.summary")

	// triage_prs GCX.
	g3 := callPRTool(t, srv, "triage_prs", srv.handleTriagePRs, map[string]any{"prs": string(prsJSON), "files": "{\"1\":[\"" + file + "\"]}", "format": "gcx"})
	require.False(t, g3.IsError)
	require.Contains(t, g3.Content[0].(mcplib.TextContent).Text, "triage_prs.ranked")

	// TOON round-trip keeps a known key.
	tn := callPRTool(t, srv, "list_prs", srv.handleListPRs, map[string]any{"prs": string(prsJSON), "format": "toon"})
	require.False(t, tn.IsError)
	require.Contains(t, tn.Content[0].(mcplib.TextContent).Text, "total")

	// max_bytes budget honoured: the budgeted GCX response is trimmed
	// below the full one (row-tail trimming kicks in on opt-in).
	full := callPRTool(t, srv, "get_pr_impact", srv.handleGetPRImpact, map[string]any{"number": float64(1), "files": string(filesJSON), "format": "gcx"})
	require.False(t, full.IsError)
	b := callPRTool(t, srv, "get_pr_impact", srv.handleGetPRImpact, map[string]any{"number": float64(1), "files": string(filesJSON), "format": "gcx", "max_bytes": float64(160)})
	require.False(t, b.IsError)
	require.Less(t, len(b.Content[0].(mcplib.TextContent).Text), len(full.Content[0].(mcplib.TextContent).Text),
		"max_bytes must trim the response below the unbudgeted size")
}

// --- discoverability via tools_search --------------------------------------

func TestPRTools_DiscoverableViaToolsSearch(t *testing.T) {
	t.Setenv("GORTEX_LAZY_TOOLS", "1")
	srv, _ := setupTestServer(t)
	require.NotNil(t, srv.lazy)

	for _, name := range []string{"list_prs", "get_pr_impact", "triage_prs"} {
		hits := srv.lazy.Query("select:"+name, 1)
		require.Len(t, hits, 1, "%s must be discoverable by exact name", name)
		require.Equal(t, name, hits[0].tool.Name)
	}

	// All three are deferred (none in the hot eager set).
	for _, name := range []string{"list_prs", "get_pr_impact", "triage_prs"} {
		require.False(t, hotEagerTools[name], "%s must be a deferred tool", name)
	}
}

// --- retry-after parser ----------------------------------------------------

func TestRetryAfterSeconds(t *testing.T) {
	require.Equal(t, 30, retryAfterSeconds(fmt.Errorf("%w (retry after 30s)", forge.ErrRateLimited)))
	require.Equal(t, 90, retryAfterSeconds(fmt.Errorf("%w (retry after 1m30s)", forge.ErrRateLimited)))
	require.Equal(t, -1, retryAfterSeconds(fmt.Errorf("%w: secondary limit", forge.ErrRateLimited)))
	require.Equal(t, -1, retryAfterSeconds(nil))
}
