package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/forge"
	"github.com/zzet/gortex/internal/graph"
)

// forgeList / forgeFiles are the package-level seam over the forge free
// functions. They default to the real network-backed forge calls; a test
// swaps them for closures returning canned data so the PR tools exercise
// the graph-join and ranking logic with no network. Keep this the only
// indirection — every PR tool routes its self-served fetch through these.
var (
	forgeList  = forge.ListPRs
	forgeFiles = forge.PRFiles
)

// prCacheTTL is how long a fetched forge.PR is reused before a refetch.
// A short TTL means a triage re-run within the window does not refetch
// the same PR, while a stale list is never served for long.
const prCacheTTL = 60 * time.Second

// prCacheEntry is one cached forge.PR keyed by (repo, number), stamped
// with the time it was fetched so the TTL can be evaluated on read.
type prCacheEntry struct {
	pr        forge.PR
	fetchedAt time.Time
}

// prCache is a small TTL cache of forge.PR values per (repo, number). It
// is shared across PR-tool calls on a server so a triage fan-out plus a
// follow-up get_pr_impact on the same PR reuse one fetch within the TTL.
type prCache struct {
	mu  sync.Mutex
	ttl time.Duration
	m   map[string]prCacheEntry
}

// newPRCache builds an empty PR cache with the given TTL.
func newPRCache(ttl time.Duration) *prCache {
	return &prCache{ttl: ttl, m: make(map[string]prCacheEntry)}
}

// prCacheKey is the cache key for a PR — the repo prefix plus the number.
func prCacheKey(repo string, number int) string {
	return repo + "\x1f" + fmt.Sprintf("%d", number)
}

// get returns the cached PR for (repo, number) when present and still
// within the TTL.
func (c *prCache) get(repo string, number int) (forge.PR, bool) {
	if c == nil {
		return forge.PR{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[prCacheKey(repo, number)]
	if !ok {
		return forge.PR{}, false
	}
	if c.ttl > 0 && time.Since(e.fetchedAt) > c.ttl {
		delete(c.m, prCacheKey(repo, number))
		return forge.PR{}, false
	}
	return e.pr, true
}

// put records a freshly-fetched PR under (repo, number).
func (c *prCache) put(repo string, number int, pr forge.PR) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[prCacheKey(repo, number)] = prCacheEntry{pr: pr, fetchedAt: time.Now()}
}

// registerPRTools registers the data-only forge MCP surface — list_prs,
// get_pr_impact, triage_prs. All three are deferred (they land in the
// lazy catalog unless GORTEX_LAZY_TOOLS is off), read-only, and forge-
// self-serving: each fetches PR data via the daemon's own forge client
// (GH_TOKEN + the indexed repo identity), with an optional caller-
// supplied-data path to skip the network. None of them edits.
func (s *Server) registerPRTools() {
	s.addTool(
		mcp.NewTool("list_prs",
			mcp.WithDescription("List a repository's pull requests with a one-shot review-state classification. Each PR is reduced to a state label (DRAFT / BASE_MISMATCH / CHANGES_REQUESTED / APPROVED / STALE / READY), a normalized CI rollup (NONE / FAILURE / PENDING / SUCCESS), and its merge blockers. The daemon self-serves the data via its own forge client (needs GH_TOKEN / GITHUB_TOKEN in the daemon environment); pass `prs` to classify an already-fetched set with no network call. Use to triage a review queue before opening any PR."),
			mcp.WithString("repo", mcp.Description("Repository prefix to resolve the working tree (multi-repo mode).")),
			mcp.WithString("state", mcp.Description("PR state filter passed to the forge: open (default), closed, or all.")),
			mcp.WithNumber("limit", mcp.Description("Cap the number of PRs fetched / returned (default 30).")),
			mcp.WithString("prs", mcp.Description("JSON array of already-fetched forge.PR objects to classify instead of calling the forge. Skips the network entirely.")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx (GCX1 compact wire format), or toon")),
			mcp.WithNumber("max_bytes", mcp.Description("Cap the marshaled response at this many bytes. The longest list is trimmed; truncation metadata rides on the response. Omit for no cap.")),
		),
		s.handleListPRs,
	)
	s.addTool(
		mcp.NewTool("get_pr_impact",
			mcp.WithDescription("Graph-joined blast radius and risk score for a pull request. Maps the PR's changed files to the symbols they define (whole-file granularity), scores PR-level risk across five axes (blast-radius flow, caller fan-in, coverage gap, security keywords, community span), and groups the affected surface by community and by caller/test file. The daemon self-serves the changed-file set via its forge client (needs GH_TOKEN / GITHUB_TOKEN); pass `files` to skip the fetch. Set `receipt:true` to additionally emit a small privacy-safe review receipt. Use to gauge how carefully a PR must be reviewed before reading the diff."),
			mcp.WithNumber("number", mcp.Required(), mcp.Description("GitHub PR number.")),
			mcp.WithString("repo", mcp.Description("Repository prefix to resolve the working tree (multi-repo mode).")),
			mcp.WithString("files", mcp.Description("JSON array of already-fetched changed file paths to score instead of calling the forge. Skips the network entirely.")),
			mcp.WithBoolean("receipt", mcp.Description("When true, also emit a small machine-readable review receipt (counts, tier, next safe action, merge-blocker verdict) — no file paths or symbol IDs.")),
			mcp.WithBoolean("scrub", mcp.Description("When emitting a receipt, strip any path-like / symbol-ID-like / email-like value so the receipt is safe to share cross-org. No effect unless receipt is true.")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx (GCX1 compact wire format), or toon")),
			mcp.WithNumber("max_bytes", mcp.Description("Cap the marshaled response at this many bytes. The longest list is trimmed; truncation metadata rides on the response. Omit for no cap.")),
		),
		s.handleGetPRImpact,
	)
	s.addTool(
		mcp.NewTool("triage_prs",
			mcp.WithDescription("Rank a repository's open pull requests by graph-derived review priority. Computes get_pr_impact per PR and orders them by composite risk score (highest first, deterministic). The daemon self-serves the PR list and per-PR files via its forge client (needs GH_TOKEN / GITHUB_TOKEN); pass `prs` and/or `files` to supply already-fetched data and skip the fan-out. Use to decide which PR to review first."),
			mcp.WithString("repo", mcp.Description("Repository prefix to resolve the working tree (multi-repo mode).")),
			mcp.WithNumber("limit", mcp.Description("Cap the number of open PRs triaged / returned (default 20).")),
			mcp.WithString("prs", mcp.Description("JSON array of already-fetched forge.PR objects to triage instead of listing via the forge.")),
			mcp.WithString("files", mcp.Description("JSON object mapping a PR number (as a string key) to its already-fetched changed file paths, so a per-PR file fetch is skipped.")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx (GCX1 compact wire format), or toon")),
			mcp.WithNumber("max_bytes", mcp.Description("Cap the marshaled response at this many bytes. The longest list is trimmed; truncation metadata rides on the response. Omit for no cap.")),
		),
		s.handleTriagePRs,
	)
}

// forgeUnavailablePayload is the typed degradation returned when no token
// is resolvable and the caller supplied no data. It names GH_TOKEN so the
// operator knows exactly what the daemon environment is missing.
func forgeUnavailablePayload() map[string]any {
	return map[string]any{
		"error": "forge unavailable",
		"hint":  "set GH_TOKEN (or GITHUB_TOKEN) in the daemon environment",
	}
}

// rateLimitedPayload maps a forge.ErrRateLimited error onto the typed
// rate-limit degradation, extracting the Retry-After hint the forge layer
// encoded in the wrapped message.
func rateLimitedPayload(err error) map[string]any {
	out := map[string]any{"error": "rate limited"}
	if s := retryAfterSeconds(err); s >= 0 {
		out["retry_after_s"] = s
	}
	return out
}

// retryAfterSeconds extracts the whole-second Retry-After hint the forge
// layer wraps into a rate-limit error's message ("(retry after 30s)").
// Returns -1 when no hint is present.
func retryAfterSeconds(err error) int {
	if err == nil {
		return -1
	}
	msg := err.Error()
	const marker = "retry after "
	idx := strings.Index(msg, marker)
	if idx < 0 {
		return -1
	}
	rest := msg[idx+len(marker):]
	// rest looks like "30s)" or "1m30s)" — parse the leading Go duration.
	end := strings.IndexByte(rest, ')')
	if end >= 0 {
		rest = rest[:end]
	}
	rest = strings.TrimSpace(rest)
	d, perr := time.ParseDuration(rest)
	if perr != nil {
		return -1
	}
	return int(d.Seconds())
}

// isForgeUnavailable reports whether err is a "no token" forge error.
func isForgeUnavailable(err error) bool {
	return errors.Is(err, forge.ErrNotAuthenticated)
}

// handleListPRs lists (or accepts) a repository's PRs and classifies each
// into a review-state label + CI rollup + blockers.
func (s *Server) handleListPRs(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	repo := strings.TrimSpace(req.GetString("repo", ""))
	state := strings.TrimSpace(req.GetString("state", ""))
	if state == "" {
		state = "open"
	}
	limit := req.GetInt("limit", 30)
	if limit < 1 {
		limit = 30
	}

	prs, supplied, err := s.parseSuppliedPRs(req)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	if !supplied {
		if !forge.Available(ctx) {
			return s.respondJSONOrTOON(ctx, req, forgeUnavailablePayload())
		}
		roots := s.collectRepoRoots(repo)
		repoRoot := pickRepoRoot(roots, repo)
		fetched, ferr := forgeList(ctx, repoRoot, forge.ListOpts{State: state, Limit: limit, WithDecision: true, WithCI: true})
		if ferr != nil {
			if errors.Is(ferr, forge.ErrRateLimited) {
				return s.respondJSONOrTOON(ctx, req, rateLimitedPayload(ferr))
			}
			if isForgeUnavailable(ferr) {
				return s.respondJSONOrTOON(ctx, req, forgeUnavailablePayload())
			}
			return mcp.NewToolResultError(fmt.Sprintf("listing PRs failed: %v", ferr)), nil
		}
		prs = fetched
		for _, pr := range prs {
			s.prCache.put(repo, pr.Number, pr)
		}
	}

	if limit > 0 && len(prs) > limit {
		prs = prs[:limit]
	}

	payload := listPRsPayload(prs)

	if s.isGCX(ctx, req) {
		return s.gcxResponseWithBudget(req)(encodeListPRs(payload))
	}
	if s.isTOON(ctx, req) {
		return returnTOON(payload)
	}
	return s.respondJSONOrTOON(ctx, req, payload)
}

// listPRsPayload projects the classified PRs onto the list_prs wire shape.
func listPRsPayload(prs []forge.PR) map[string]any {
	rows := make([]map[string]any, 0, len(prs))
	for _, pr := range prs {
		st := forge.ClassifyStatus(pr, pr.BaseRef)
		blockers := st.Blockers
		if blockers == nil {
			blockers = []string{}
		}
		rows = append(rows, map[string]any{
			"number":   pr.Number,
			"title":    pr.Title,
			"author":   pr.Author,
			"age_days": st.AgeDays,
			"ci":       forge.RollupCI(pr),
			"review":   pr.ReviewDecision,
			"state":    st.State,
			"blockers": blockers,
		})
	}
	return map[string]any{
		"prs":   rows,
		"total": len(rows),
	}
}

// handleGetPRImpact maps a PR's changed files to symbols, scores PR-level
// risk, and groups the affected surface by community and caller/test file.
func (s *Server) handleGetPRImpact(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if s.graph == nil {
		return mcp.NewToolResultError("no graph available — index a repo first"), nil
	}

	number := req.GetInt("number", 0)
	if number <= 0 {
		return mcp.NewToolResultError("number is required"), nil
	}
	repo := strings.TrimSpace(req.GetString("repo", ""))
	receipt := req.GetBool("receipt", false)
	scrub := req.GetBool("scrub", false)

	files, supplied, err := s.parseSuppliedFiles(req)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	if !supplied {
		fetched, degraded, ferr := s.fetchPRFiles(ctx, repo, number)
		if degraded != nil {
			return s.respondJSONOrTOON(ctx, req, degraded)
		}
		if ferr != nil {
			return mcp.NewToolResultError(ferr.Error()), nil
		}
		files = fetched
	}

	payload := s.prImpactForNumber(ctx, number, files, receipt, scrub)

	if s.isGCX(ctx, req) {
		return s.gcxResponseWithBudget(req)(encodePRImpact(payload))
	}
	if s.isTOON(ctx, req) {
		return returnTOON(payload)
	}
	return s.respondJSONOrTOON(ctx, req, payload)
}

// prImpactForNumber builds the get_pr_impact payload for one PR from its
// changed file set: file→symbol join, PR-risk score, and community / blast
// grouping. receipt adds a privacy-safe review receipt projected from the
// same risk result. It performs no network I/O — the files are supplied.
func (s *Server) prImpactForNumber(ctx context.Context, number int, files []string, receipt, scrub bool) map[string]any {
	changedFiles, changedSymbolNodes := s.changedSymbolsForFiles(files)
	symbolIDs := make([]string, 0, len(changedSymbolNodes))
	for _, n := range changedSymbolNodes {
		symbolIDs = append(symbolIDs, n.ID)
	}

	communities := s.getCommunities()
	var nodeToComm map[string]string
	if communities != nil {
		nodeToComm = communities.NodeToComm
	}

	result := analysis.ScorePRRisk(s.graph, analysis.PRRiskInput{
		SymbolIDs:    symbolIDs,
		ChangedFiles: changedFiles,
		NodeToComm:   nodeToComm,
		Communities:  communities,
		Processes:    s.getProcesses(),
	})

	priorities := make([]map[string]any, 0, len(result.Factors))
	for _, f := range result.Factors {
		priorities = append(priorities, map[string]any{
			"axis":   f.Axis,
			"score":  f.Score,
			"reason": f.Reason,
		})
	}

	// Community grouping: the distinct communities the changed symbols span.
	commSet := map[string]bool{}
	for _, id := range symbolIDs {
		if cid, ok := nodeToComm[id]; ok && cid != "" {
			commSet[cid] = true
		}
	}
	commList := make([]string, 0, len(commSet))
	for c := range commSet {
		commList = append(commList, c)
	}
	sort.Strings(commList)

	changedFilesOut := append([]string(nil), changedFiles...)
	sort.Strings(changedFilesOut)

	changedSymbolsOut := make([]map[string]any, 0, len(changedSymbolNodes))
	for _, n := range changedSymbolNodes {
		changedSymbolsOut = append(changedSymbolsOut, map[string]any{
			"id":   n.ID,
			"name": n.Name,
			"kind": string(n.Kind),
			"file": n.FilePath,
		})
	}

	payload := map[string]any{
		"number":            number,
		"risk":              string(result.Risk),
		"score":             result.Score,
		"review_priorities": priorities,
		"changed_files":     changedFilesOut,
		"changed_symbols":   changedSymbolsOut,
		"communities":       commList,
		"blast":             s.buildBlastRadius(ctx, changedSymbolNodes),
	}

	if receipt {
		rec := analysis.BuildReviewReceipt(result, "", false, scrub)
		payload["receipt"] = rec
	}
	return payload
}

// changedSymbolsForFiles maps a set of changed file paths to the code
// symbols those files define, via GetFileNodes (whole-file granularity —
// the coarse mapping a not-checked-out PR allows). Returns the deduped
// non-empty file list and the deduped symbol nodes (file nodes excluded),
// both deterministically ordered.
func (s *Server) changedSymbolsForFiles(files []string) ([]string, []*graph.Node) {
	fileSeen := map[string]bool{}
	var changedFiles []string
	nodeSeen := map[string]bool{}
	var nodes []*graph.Node
	for _, f := range files {
		f = strings.TrimSpace(f)
		if f == "" || fileSeen[f] {
			continue
		}
		fileSeen[f] = true
		changedFiles = append(changedFiles, f)
		for _, n := range s.graph.GetFileNodes(f) {
			if n == nil || n.Kind == graph.KindFile {
				continue
			}
			if nodeSeen[n.ID] {
				continue
			}
			nodeSeen[n.ID] = true
			nodes = append(nodes, n)
		}
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })
	return changedFiles, nodes
}

// handleTriagePRs ranks a repository's open PRs by get_pr_impact score
// (highest first, deterministic). The PR list and per-PR files come from
// supplied maps or a self-served forge fan-out.
func (s *Server) handleTriagePRs(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if s.graph == nil {
		return mcp.NewToolResultError("no graph available — index a repo first"), nil
	}

	repo := strings.TrimSpace(req.GetString("repo", ""))
	limit := req.GetInt("limit", 20)
	if limit < 1 {
		limit = 20
	}

	prs, prsSupplied, err := s.parseSuppliedPRs(req)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	filesByNumber, err := s.parseSuppliedFilesByNumber(req)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	if !prsSupplied {
		if !forge.Available(ctx) && len(filesByNumber) == 0 {
			return s.respondJSONOrTOON(ctx, req, forgeUnavailablePayload())
		}
		roots := s.collectRepoRoots(repo)
		repoRoot := pickRepoRoot(roots, repo)
		fetched, ferr := forgeList(ctx, repoRoot, forge.ListOpts{State: "open", Limit: limit, WithCI: true})
		if ferr != nil {
			if errors.Is(ferr, forge.ErrRateLimited) {
				return s.respondJSONOrTOON(ctx, req, rateLimitedPayload(ferr))
			}
			if isForgeUnavailable(ferr) {
				return s.respondJSONOrTOON(ctx, req, forgeUnavailablePayload())
			}
			return mcp.NewToolResultError(fmt.Sprintf("listing PRs failed: %v", ferr)), nil
		}
		prs = fetched
		for _, pr := range prs {
			s.prCache.put(repo, pr.Number, pr)
		}
	}

	if limit > 0 && len(prs) > limit {
		prs = prs[:limit]
	}

	ranked := make([]map[string]any, 0, len(prs))
	for _, pr := range prs {
		files, ok := filesByNumber[pr.Number]
		if !ok {
			// Use the PR's own hydrated files if present; otherwise self-serve.
			if len(pr.Files) > 0 {
				files = pr.Files
			} else {
				fetched, degraded, ferr := s.fetchPRFiles(ctx, repo, pr.Number)
				if degraded != nil {
					return s.respondJSONOrTOON(ctx, req, degraded)
				}
				if ferr != nil {
					return mcp.NewToolResultError(ferr.Error()), nil
				}
				files = fetched
			}
		}
		impact := s.prImpactForNumber(ctx, pr.Number, files, false, false)
		ranked = append(ranked, map[string]any{
			"number": pr.Number,
			"title":  pr.Title,
			"author": pr.Author,
			"risk":   impact["risk"],
			"score":  impact["score"],
		})
	}

	// Deterministic: score descending, PR number ascending on a tie.
	sort.SliceStable(ranked, func(i, j int) bool {
		si, _ := ranked[i]["score"].(float64)
		sj, _ := ranked[j]["score"].(float64)
		if si != sj {
			return si > sj
		}
		ni, _ := ranked[i]["number"].(int)
		nj, _ := ranked[j]["number"].(int)
		return ni < nj
	})

	payload := map[string]any{
		"ranked": ranked,
		"total":  len(ranked),
	}

	if s.isGCX(ctx, req) {
		return s.gcxResponseWithBudget(req)(encodeTriagePRs(payload))
	}
	if s.isTOON(ctx, req) {
		return returnTOON(payload)
	}
	return s.respondJSONOrTOON(ctx, req, payload)
}

// fetchPRFiles resolves the changed-file set for one PR via the forge,
// honoring the no-token and rate-limit degradations. The returned map is
// non-nil only when a degradation payload should be returned to the
// caller; the error is non-nil only for an unexpected failure.
func (s *Server) fetchPRFiles(ctx context.Context, repo string, number int) (files []string, degraded map[string]any, err error) {
	if !forge.Available(ctx) {
		return nil, forgeUnavailablePayload(), nil
	}
	roots := s.collectRepoRoots(repo)
	repoRoot := pickRepoRoot(roots, repo)
	fetched, ferr := forgeFiles(ctx, repoRoot, number)
	if ferr != nil {
		if errors.Is(ferr, forge.ErrRateLimited) {
			return nil, rateLimitedPayload(ferr), nil
		}
		if isForgeUnavailable(ferr) {
			return nil, forgeUnavailablePayload(), nil
		}
		return nil, nil, fmt.Errorf("fetching files for PR #%d failed: %v", number, ferr)
	}
	return fetched, nil, nil
}

// parseSuppliedPRs reads the optional caller-supplied `prs` JSON array of
// forge.PR objects. supplied reports whether the caller provided the arg
// at all (an empty-but-present array still counts, so the tool skips the
// network as the caller asked). A malformed value is an error.
func (s *Server) parseSuppliedPRs(req mcp.CallToolRequest) (prs []forge.PR, supplied bool, err error) {
	raw := strings.TrimSpace(req.GetString("prs", ""))
	if raw == "" {
		return nil, false, nil
	}
	if uerr := json.Unmarshal([]byte(raw), &prs); uerr != nil {
		return nil, false, fmt.Errorf("invalid prs JSON: %v", uerr)
	}
	return prs, true, nil
}

// parseSuppliedFiles reads the optional caller-supplied `files` JSON array
// of changed file paths. supplied reports whether the caller provided the
// arg, so the tool can skip the forge fetch even for an empty list.
func (s *Server) parseSuppliedFiles(req mcp.CallToolRequest) (files []string, supplied bool, err error) {
	raw := strings.TrimSpace(req.GetString("files", ""))
	if raw == "" {
		return nil, false, nil
	}
	if uerr := json.Unmarshal([]byte(raw), &files); uerr != nil {
		return nil, false, fmt.Errorf("invalid files JSON: %v", uerr)
	}
	return files, true, nil
}

// parseSuppliedFilesByNumber reads the optional caller-supplied `files`
// JSON object mapping a PR number (string key) to its changed file paths,
// used by triage_prs to skip a per-PR file fetch. An absent arg yields an
// empty map; a malformed value is an error.
func (s *Server) parseSuppliedFilesByNumber(req mcp.CallToolRequest) (map[int][]string, error) {
	raw := strings.TrimSpace(req.GetString("files", ""))
	if raw == "" {
		return map[int][]string{}, nil
	}
	var stringKeyed map[string][]string
	if uerr := json.Unmarshal([]byte(raw), &stringKeyed); uerr != nil {
		return nil, fmt.Errorf("invalid files JSON (want an object mapping PR number to file paths): %v", uerr)
	}
	out := make(map[int][]string, len(stringKeyed))
	for k, v := range stringKeyed {
		n, cerr := strconv.Atoi(strings.TrimSpace(k))
		if cerr != nil {
			return nil, fmt.Errorf("invalid PR number key %q in files map: %v", k, cerr)
		}
		out[n] = v
	}
	return out, nil
}
