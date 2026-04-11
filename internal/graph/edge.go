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
)

type Edge struct {
	From            string         `json:"from"`
	To              string         `json:"to"`
	Kind            EdgeKind       `json:"kind"`
	FilePath        string         `json:"file_path"`
	Line            int            `json:"line"`
	Confidence      float64        `json:"confidence,omitempty"`
	ConfidenceLabel string         `json:"confidence_label,omitempty"`
	CrossRepo       bool           `json:"cross_repo,omitempty"`
	Meta            map[string]any `json:"meta,omitempty"`
}

// ConfidenceLabelFor returns EXTRACTED, INFERRED, or AMBIGUOUS for an edge
// based on its kind and confidence value.
func ConfidenceLabelFor(kind EdgeKind, confidence float64) string {
	// Structural edges from AST are always extracted.
	switch kind {
	case EdgeDefines, EdgeImports, EdgeExtends, EdgeMemberOf, EdgeImplements,
		EdgeProvides, EdgeConsumes:
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
