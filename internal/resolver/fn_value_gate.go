package resolver

import "github.com/zzet/gortex/internal/graph"

// Function-as-value callback gate.
//
// A large class of real call relationships is wired by passing a function as a
// *value* — registering a handler (`router.Get("/x", handler)`), a callback
// (`list.forEach(process)`), an observer (`signal.connect(onChange)`) — rather
// than calling it directly. The per-language extractors capture each such
// value-position identifier as a placeholder reference edge
// (To = "unresolved::fnvalue::<name>", Meta via="callback_candidate",
// fn_value_name=<name>); see EmitFnValueCandidates in the languages package.
//
// Capture alone floods: every bare identifier in a value position is a
// candidate, and most are locals, parameters, or builtins, not functions. This
// gate is the other half of the pair — it binds each candidate to a real
// function/method in the SAME FILE and drops the rest, so an unbound identifier
// never becomes an edge.
//
// Beat: the landed edge rides a provenance TIER (OriginASTInferred — a
// scope-bound name resolution, strictly above text_matched) so callback edges
// are min_tier-filterable like every other Gortex edge, instead of carrying a
// single flat heuristic flag. The per-language value-position capture lands on
// top of this skeleton.
const (
	// SynthFnValueCallback is the provenance tag for a bound callback edge.
	SynthFnValueCallback = "fn-value-callback"

	// fnValueCandidateVia marks an extractor-emitted placeholder awaiting the
	// gate; fnValueRegistrationVia marks the bound edge the gate lands.
	fnValueCandidateVia    = "callback_candidate"
	fnValueRegistrationVia = "callback_registration"

	// metaFnValueName carries the captured bare identifier on both the
	// placeholder and the bound edge.
	metaFnValueName = "fn_value_name"
)

// ResolveFnValueCallbacks binds each captured function-as-value placeholder to a
// same-file function/method and lands a tiered callback-registration reference
// edge, dropping any candidate that does not resolve to a real function. It is a
// full-recompute, idempotent synthesizer: graph.AddEdge dedupes and
// graph.EvictFile drops the edges on reindex. Returns the number of edges
// landed.
func ResolveFnValueCallbacks(g graph.Store) int {
	if g == nil {
		return 0
	}
	var landed []*graph.Edge
	for _, e := range g.AllEdges() {
		if e == nil || e.Meta == nil {
			continue
		}
		if via, _ := e.Meta["via"].(string); via != fnValueCandidateVia {
			continue
		}
		name, _ := e.Meta[metaFnValueName].(string)
		if name == "" || isFnValueNonTarget(name) {
			continue
		}
		// Same-file scope is the high-confidence default. A qualified-path
		// candidate the capture marked `fn_value_ungated` (e.g. a Rust `m::f`
		// path) may bind cross-module — to a uniquely-named function in the
		// repo — at a lower confidence so min_tier filtering segregates it.
		target := resolveFnValueName(g, e.FilePath, name)
		conf := 0.6
		if target == "" {
			if ungated, _ := e.Meta["fn_value_ungated"].(bool); ungated {
				target = resolveFnValueCrossModule(g, name)
				conf = 0.45
			}
		}
		if target == "" || target == e.From {
			// Unbound (a local / param / undefined name) or a self-reference
			// (a function's own declaration token): reject rather than
			// fabricate an edge.
			continue
		}
		landed = append(landed, &graph.Edge{
			From:            e.From,
			To:              target,
			Kind:            graph.EdgeReferences,
			FilePath:        e.FilePath,
			Line:            e.Line,
			Confidence:      conf,
			ConfidenceLabel: graph.ConfidenceLabelFor(graph.EdgeReferences, conf),
			Origin:          graph.OriginASTInferred,
			Meta: map[string]any{
				"via":             fnValueRegistrationVia,
				metaFnValueName:   name,
				MetaSynthesizedBy: SynthFnValueCallback,
				MetaProvenance:    ProvenanceHeuristic,
			},
		})
	}
	for _, e := range landed {
		g.AddEdge(e)
	}
	return len(landed)
}

// resolveFnValueName returns the ID of a same-file function or method named
// name, or "" when none exists. Same-file scope is the conservative default;
// per-language capture extends the gate with imported-symbol and C-family
// file-scope rules on top of this skeleton.
func resolveFnValueName(g graph.Store, filePath, name string) string {
	if filePath == "" || name == "" {
		return ""
	}
	for _, n := range g.GetFileNodes(filePath) {
		if n == nil {
			continue
		}
		if n.Name != name {
			continue
		}
		if n.Kind == graph.KindFunction || n.Kind == graph.KindMethod {
			return n.ID
		}
	}
	return ""
}

// resolveFnValueCrossModule binds a qualified-path function value to a
// uniquely-named function/method anywhere in the repo, refusing on ambiguity
// (more than one definition of the name). The same-file path is preferred by
// the caller; this is the cross-module fallback for an explicit path value.
func resolveFnValueCrossModule(g graph.Store, name string) string {
	match := ""
	for _, n := range g.FindNodesByName(name) {
		if n == nil {
			continue
		}
		if n.Kind != graph.KindFunction && n.Kind != graph.KindMethod {
			continue
		}
		if match != "" && match != n.ID {
			return "" // ambiguous across modules — drop
		}
		match = n.ID
	}
	return match
}

// isFnValueNonTarget reports whether name is a literal/keyword/builtin that
// can never be a captured function value, so the gate skips it before the
// same-file lookup. The set is deliberately small and language-agnostic; the
// per-language capture passes refine it with isGoBuiltinOrKeyword-style checks.
func isFnValueNonTarget(name string) bool {
	switch name {
	case "true", "false", "nil", "null", "none", "None", "undefined",
		"this", "self", "super", "new", "delete", "typeof", "void":
		return true
	}
	return false
}
