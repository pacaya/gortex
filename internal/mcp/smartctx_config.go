package mcp

import (
	"context"

	"github.com/zzet/gortex/internal/callpath"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/eval/quality"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
)

// smartContextSections resolves which in-pack enrichment sections a
// smart_context call should attach. Per-call include_* params override the
// project's smart_context config; every section is off by default.
func (s *Server) smartContextSections(args map[string]any, relPath string) config.SmartContextSections {
	cfg := config.SmartContextConfig{}
	if s.configManager != nil {
		cfg = s.configManager.GetRepoConfig(repoPrefixForPath(s, relPath)).MCP.SmartContext
	}
	return cfg.Resolve(
		boolPtrArg(args, "include_call_paths"),
		boolPtrArg(args, "include_flows"),
		boolPtrArg(args, "include_confidence"),
	)
}

// boolPtrArg returns a *bool: the parsed value when the caller passed the key,
// nil when absent — so an unset flag inherits config rather than forcing false.
func boolPtrArg(args map[string]any, key string) *bool {
	if v, set := boolArg(args, key); set {
		return &v
	}
	return nil
}

// attachInPackSections records the opt-in in-pack enrichment sections on the
// assembled pack under result["in_pack"]. Only sections with content are
// written, so the default pack stays untouched; later passes attach the flow
// spine and confidence verdict to the same block.
func (s *Server) attachInPackSections(result map[string]any, sections config.SmartContextSections, symbols []*graph.Node) {
	block := map[string]any{}
	if sections.CallPaths {
		if cp := s.inPackCallPaths(symbols); len(cp) > 0 {
			block["call_paths"] = cp
		}
	}
	if sections.Flows {
		if fl := s.inPackFlows(symbols); fl != nil {
			block["flows"] = fl
		}
	}
	if len(block) > 0 {
		result["in_pack"] = block
	}
}

// inPackFlows builds the flow section: a forward flow spine from the focus
// symbol (the first pack symbol) and the dynamic-dispatch boundaries that spine
// hits — call sites whose target the static graph cannot resolve, where the
// flow would continue at runtime. Returns nil when there is no multi-node spine
// and no boundary to announce.
func (s *Server) inPackFlows(symbols []*graph.Node) map[string]any {
	if s.graph == nil || len(symbols) == 0 || symbols[0] == nil {
		return nil
	}
	budget := s.inPackBudget()
	spine, boundaries := s.flowSpine(symbols[0].ID, budget.FlowDepth)
	if len(boundaries) > budget.MaxBoundaries {
		boundaries = boundaries[:budget.MaxBoundaries]
	}
	out := map[string]any{}
	if len(spine) >= 2 {
		out["spine"] = spine
	}
	if len(boundaries) > 0 {
		out["boundaries"] = boundaries
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// addInPackSection records one section on result["in_pack"], creating the block
// if it does not exist yet.
func addInPackSection(result map[string]any, key string, val any) {
	inPack, _ := result["in_pack"].(map[string]any)
	if inPack == nil {
		inPack = map[string]any{}
	}
	inPack[key] = val
	result["in_pack"] = inPack
}

// inPackConfidence builds the retrieval-confidence verdict for a task: it runs a
// ranked search for the task and summarises the candidate-score distribution
// (how sharply the top result beats the rest) into a high/medium/low verdict.
// Returns nil when the search yields nothing.
func (s *Server) inPackConfidence(ctx context.Context, task string) map[string]any {
	eng := s.engineFor(ctx)
	if eng == nil || task == "" {
		return nil
	}
	cands := eng.SearchSymbolsRanked(task, 10, query.QueryOptions{}, nil)
	if len(cands) == 0 {
		return nil
	}
	scores := make([]float64, 0, len(cands))
	for _, c := range cands {
		if c != nil {
			scores = append(scores, c.Score)
		}
	}
	return buildConfidenceVerdict(quality.ConfidenceFromScores(task, scores))
}

// buildConfidenceVerdict reduces a confidence record to the in-pack verdict map.
// Returns nil for an empty record.
func buildConfidenceVerdict(rec quality.ConfidenceRecord) map[string]any {
	if rec.K == 0 {
		return nil
	}
	return map[string]any{
		"verdict":         confidenceVerdict(rec),
		"top1":            rec.Top1,
		"top2":            rec.Top2,
		"ratio_top1_top2": rec.Ratio12,
		"k":               rec.K,
		"std_dev":         rec.StdDev,
	}
}

// confidenceVerdict classifies a confidence record: a single candidate is
// "single", a sharp top-1 (≥2× the runner-up) is "high", a modest lead
// "medium", and a flat distribution "low" (the ranker is unsure).
func confidenceVerdict(rec quality.ConfidenceRecord) string {
	switch {
	case rec.K <= 1:
		return "single"
	case rec.Ratio12 >= 2.0:
		return "high"
	case rec.Ratio12 >= 1.25:
		return "medium"
	default:
		return "low"
	}
}

// flowSpine greedily walks forward from the focus over resolved CALLS/REFERENCES
// edges (smallest target id first, for determinism), returning the chain of
// node ids it traverses and the dynamic-dispatch boundaries — out-edges to
// unresolved targets — encountered along the way.
func (s *Server) flowSpine(focus string, maxDepth int) (spine []string, boundaries []map[string]any) {
	visited := map[string]bool{focus: true}
	bseen := map[string]bool{}
	spine = []string{focus}
	cur := focus
	for depth := 0; depth < maxDepth; depth++ {
		next := ""
		for _, e := range s.graph.GetOutEdges(cur) {
			if e == nil || (e.Kind != graph.EdgeCalls && e.Kind != graph.EdgeReferences) {
				continue
			}
			if graph.IsUnresolvedTarget(e.To) {
				key := e.From + "\x00" + e.To
				if !bseen[key] {
					bseen[key] = true
					boundaries = append(boundaries, map[string]any{
						"from":   e.From,
						"target": graph.UnresolvedName(e.To),
						"reason": "dynamic_dispatch",
					})
				}
				continue
			}
			if visited[e.To] {
				continue
			}
			if next == "" || e.To < next {
				next = e.To
			}
		}
		if next == "" {
			break
		}
		visited[next] = true
		spine = append(spine, next)
		cur = next
	}
	return spine, boundaries
}

// inPackCallPaths builds the anchored call-paths section: the focus symbol (the
// first pack symbol) is the anchor and the rest are roots, so each row shows how
// another pack symbol reaches the focus over the call graph. Returns nil when
// fewer than two symbols are in the pack or none reach the focus.
func (s *Server) inPackCallPaths(symbols []*graph.Node) []map[string]any {
	if s.graph == nil || len(symbols) < 2 {
		return nil
	}
	anchor := symbols[0].ID
	roots := make([]string, 0, len(symbols)-1)
	for _, n := range symbols[1:] {
		if n != nil && n.ID != "" {
			roots = append(roots, n.ID)
		}
	}
	anchored := callpath.New(s.graph).PathsToAnchor(roots, anchor, callpath.Options{MaxDepth: 8})
	if len(anchored) == 0 {
		return nil
	}
	if limit := s.inPackBudget().MaxCallPaths; len(anchored) > limit {
		anchored = anchored[:limit]
	}
	out := make([]map[string]any, 0, len(anchored))
	for _, ap := range anchored {
		out = append(out, map[string]any{
			"root":       ap.Root,
			"anchor":     anchor,
			"length":     ap.Path.Length,
			"confidence": ap.Path.Confidence,
			"nodes":      ap.Path.Nodes,
		})
	}
	return out
}
