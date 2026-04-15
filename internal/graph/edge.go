package graph

type EdgeKind string

const (
	EdgeImports      EdgeKind = "imports"
	EdgeDefines      EdgeKind = "defines"
	EdgeCalls        EdgeKind = "calls"
	EdgeInstantiates EdgeKind = "instantiates"
	EdgeImplements   EdgeKind = "implements"
	EdgeExtends      EdgeKind = "extends"
	EdgeReferences   EdgeKind = "references"
	EdgeMemberOf     EdgeKind = "member_of"
	EdgeProvides     EdgeKind = "provides"
	EdgeConsumes     EdgeKind = "consumes"
	// EdgeMatches links a consumer contract node to the provider contract
	// node it resolves to (e.g. consumer http:GET:/v1/tucks → provider
	// http:GET:/v1/tucks, across repos). Traversals bridge service
	// boundaries by hopping Consumer → EdgeConsumes⁻¹ → consumer-contract
	// → EdgeMatches → provider-contract → EdgeProvides⁻¹ → handler.
	EdgeMatches EdgeKind = "matches"
)

type Edge struct {
	From     string   `json:"from"`
	To       string   `json:"to"`
	Kind     EdgeKind `json:"kind"`
	FilePath string   `json:"file_path"`
	Line     int      `json:"line"`
	// Confidence is the numeric score (0..1). Kept on the in-memory
	// struct for internal filtering (min_tier, etc.) but excluded from
	// JSON — agents act on ConfidenceLabel, and the float adds ~15
	// chars to every edge in large graph responses.
	Confidence      float64 `json:"-"`
	ConfidenceLabel string  `json:"confidence_label,omitempty"`
	Origin          string  `json:"origin,omitempty"`
	CrossRepo       bool    `json:"cross_repo,omitempty"`
	// Meta is intentionally excluded from JSON. It holds internal
	// instrumentation (semantic_source, provider hints, etc.) that agents
	// don't consume but that adds measurable bytes to every edge in
	// responses returning hundreds of call-graph edges. Internal callers
	// can still read/write the field; external MCP consumers don't see it.
	Meta map[string]any `json:"-"`
}

// Edge.Origin values — call-graph confidence tiers, highest → lowest. Use
// MeetsMinTier / OriginRank to compare.
//
//   - lsp_resolved: Compiler-grade. LSP, go/types, or SCIP confirms that this
//     edge's target is the precise symbol being referenced. Safe to rely on
//     for refactors.
//   - lsp_dispatch: Interface → implementation dispatch resolved by a
//     semantic provider. One step less direct than a literal target match.
//   - ast_resolved: Tree-sitter / AST extraction found a unique target in
//     the same compilation unit. No type system involved, but structurally
//     unambiguous.
//   - ast_inferred: Heuristic resolution using type info we extracted from
//     the AST. Not compiler-verified.
//   - text_matched: Name-only match. The weakest tier — could be a false
//     positive.
const (
	OriginLSPResolved = "lsp_resolved"
	OriginLSPDispatch = "lsp_dispatch"
	OriginASTResolved = "ast_resolved"
	OriginASTInferred = "ast_inferred"
	OriginTextMatched = "text_matched"
)

// OriginRank returns a numeric rank for origin comparison. Higher = more
// confident. Unknown or empty origin returns 0 so it sorts below all known
// tiers; filters treat it as "untagged" and fall back to legacy inference.
func OriginRank(origin string) int {
	switch origin {
	case OriginLSPResolved:
		return 5
	case OriginLSPDispatch:
		return 4
	case OriginASTResolved:
		return 3
	case OriginASTInferred:
		return 2
	case OriginTextMatched:
		return 1
	}
	return 0
}

// MeetsMinTier returns true when origin is at least as confident as minTier.
// Empty minTier always passes (no filter). Empty origin fails any non-empty
// filter — callers wanting legacy fallback should first backfill via
// DefaultOriginFor.
func MeetsMinTier(origin, minTier string) bool {
	if minTier == "" {
		return true
	}
	return OriginRank(origin) >= OriginRank(minTier)
}

// DefaultOriginFor derives an origin tier for edges that don't have Origin
// set yet (edges from providers not updated to set Origin directly, or from
// indexes produced before this field existed). Uses edge kind, confidence
// score, and semantic_source meta as fallback signals. Never returns empty.
func DefaultOriginFor(kind EdgeKind, confidence float64, semanticSource string) string {
	if semanticSource != "" {
		if kind == EdgeImplements {
			return OriginLSPDispatch
		}
		return OriginLSPResolved
	}
	// Structural AST edges are unambiguous by construction.
	switch kind {
	case EdgeDefines, EdgeImports, EdgeExtends, EdgeMemberOf,
		EdgeImplements, EdgeProvides, EdgeConsumes, EdgeMatches:
		return OriginASTResolved
	}
	// Resolution-derived edges fall back to confidence score.
	switch {
	case confidence >= 0.9:
		return OriginASTResolved
	case confidence >= 0.5:
		return OriginASTInferred
	}
	return OriginTextMatched
}

// ConfidenceLabelFor returns EXTRACTED, INFERRED, or AMBIGUOUS for an edge
// based on its kind and confidence value.
//
// Kept for back-compat; new code should prefer Origin tiers (OriginRank /
// MeetsMinTier) which distinguish LSP-grade from AST-grade evidence.
func ConfidenceLabelFor(kind EdgeKind, confidence float64) string {
	// Structural edges from AST are always extracted.
	switch kind {
	case EdgeDefines, EdgeImports, EdgeExtends, EdgeMemberOf, EdgeImplements,
		EdgeProvides, EdgeConsumes, EdgeMatches:
		return "EXTRACTED"
	}
	// Resolution-derived edges: classify by confidence score.
	switch {
	case confidence >= 0.9:
		return "EXTRACTED"
	case confidence >= 0.5:
		return "INFERRED"
	case confidence > 0:
		return "AMBIGUOUS"
	default:
		// confidence == 0 means resolved without type info.
		return "INFERRED"
	}
}
