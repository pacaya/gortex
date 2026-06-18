package mcp

import (
	"fmt"
	"os"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
)

// toolBudgetDisableEnv turns off the exploration-call budget hint when set
// to a falsey value (0 / off / false / no).
const toolBudgetDisableEnv = "GORTEX_TOOL_BUDGET"

// budgetAnnotatedTools is the set of read / navigation tools whose
// descriptions carry the exploration-call budget. These are the tools an
// agent can loop on indefinitely while exploring; edit, analysis, and
// one-shot tools are deliberately excluded.
var budgetAnnotatedTools = map[string]bool{
	"search_symbols":      true,
	"smart_context":       true,
	"get_symbol":          true,
	"get_symbol_source":   true,
	"get_editing_context": true,
	"get_file_summary":    true,
	"get_repo_outline":    true,
	"find_usages":         true,
	"get_callers":         true,
	"get_call_chain":      true,
	"get_dependencies":    true,
	"get_dependents":      true,
	"read_file":           true,
}

// budgetForNodeCount maps repo size (graph node count) to a soft ceiling
// on exploration calls. Buckets are deliberately coarse: the number is a
// self-throttle hint for the agent, not an enforced limit.
func budgetForNodeCount(nodes int) int {
	switch {
	case nodes < 2_000:
		return 8
	case nodes < 10_000:
		return 14
	case nodes < 40_000:
		return 22
	case nodes < 120_000:
		return 30
	default:
		return 40
	}
}

// smartContextSeedCount maps repo size (graph node count) to the default
// number of symbols smart_context embeds when the caller does not name a
// max_symbols. A tiny repo rarely has more than a handful of relevant
// symbols, so a small seed keeps the pack lean; a large monorepo
// benefits from a wider net before the agent has to follow up. Buckets
// mirror budgetForNodeCount so the two scale together.
func smartContextSeedCount(nodes int) int {
	switch {
	case nodes < 2_000:
		return 4
	case nodes < 40_000:
		return 6
	case nodes < 120_000:
		return 8
	default:
		return 10
	}
}

// InPackBudget caps each smart_context in-pack enrichment section so a large
// repo doesn't bloat the pack and a small one stays lean.
type InPackBudget struct {
	MaxCallPaths  int // anchored call-path rows kept
	FlowDepth     int // forward flow-spine walk depth
	MaxBoundaries int // dynamic-boundary rows kept
}

// inPackBudgetForNodeCount maps repo size (graph node count) to the in-pack
// enrichment caps. A tiny repo has few genuinely relevant neighbours, so tight
// caps keep the pack focused; a large monorepo has more worth showing, so the
// caps widen. The five buckets mirror budgetForNodeCount so the budgets scale
// together.
func inPackBudgetForNodeCount(nodes int) InPackBudget {
	switch {
	case nodes < 2_000:
		return InPackBudget{MaxCallPaths: 3, FlowDepth: 4, MaxBoundaries: 4}
	case nodes < 10_000:
		return InPackBudget{MaxCallPaths: 4, FlowDepth: 6, MaxBoundaries: 6}
	case nodes < 40_000:
		return InPackBudget{MaxCallPaths: 5, FlowDepth: 8, MaxBoundaries: 8}
	case nodes < 120_000:
		return InPackBudget{MaxCallPaths: 6, FlowDepth: 10, MaxBoundaries: 10}
	default:
		return InPackBudget{MaxCallPaths: 8, FlowDepth: 12, MaxBoundaries: 12}
	}
}

// inPackBudget returns the in-pack enrichment caps for the server's current
// graph size.
func (s *Server) inPackBudget() InPackBudget {
	nodes := 0
	if s.graph != nil {
		nodes = s.graph.NodeCount()
	}
	return inPackBudgetForNodeCount(nodes)
}

// manifestBudgetForNodeCount maps repo size (graph node count) to the
// default graded-manifest token budget when the caller does not name a
// token_budget. A small repo packs fewer relevant symbols, so a tighter
// budget avoids padding the pack with marginal context; a large monorepo
// has more genuinely relevant neighbours worth embedding. The mid bucket
// holds the historical defaultManifestBudget so existing mid-size repos
// see no change.
func manifestBudgetForNodeCount(nodes int) int {
	switch {
	case nodes < 2_000:
		return 4_000
	case nodes < 120_000:
		return defaultManifestBudget
	default:
		return 16_000
	}
}

// toolBudgetSuffix returns the sentence appended to exploration tools'
// descriptions — a project-size-scaled cap on exploration calls so the
// model self-throttles. Computed once from the graph node count; empty
// when disabled via GORTEX_TOOL_BUDGET.
func (s *Server) toolBudgetSuffix() string {
	s.toolBudgetOnce.Do(func() {
		switch strings.ToLower(strings.TrimSpace(os.Getenv(toolBudgetDisableEnv))) {
		case "0", "off", "false", "no":
			return
		}
		nodes := 0
		if s.graph != nil {
			nodes = s.graph.NodeCount()
		}
		budget := budgetForNodeCount(nodes)
		if nodes > 0 {
			s.toolBudgetCached = fmt.Sprintf(
				" Exploration budget: this repo indexes ~%d nodes — aim to make at most %d exploration calls before acting on what you have found.",
				nodes, budget)
		} else {
			s.toolBudgetCached = fmt.Sprintf(
				" Exploration budget: aim to make at most %d exploration calls before acting on what you have found.",
				budget)
		}
	})
	return s.toolBudgetCached
}

// annotateToolBudget appends the exploration-call budget to an exploration
// tool's description so the cap rides in the schema the model reads every
// turn. No-op for non-exploration tools and when the hint is disabled.
func (s *Server) annotateToolBudget(tool *mcp.Tool) {
	if tool == nil || !budgetAnnotatedTools[tool.Name] {
		return
	}
	suffix := s.toolBudgetSuffix()
	if suffix == "" {
		return
	}
	tool.Description = strings.TrimRight(tool.Description, " ") + suffix
}
