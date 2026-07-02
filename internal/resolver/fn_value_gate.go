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
		// Resolution scope depends on the captured form. A special form's
		// receiver hint (`<self>` / a concrete type) binds the member against
		// that type's methods (compiler-precise); a qualified-path candidate
		// marked `fn_value_ungated` may bind cross-module at a lower tier; a
		// plain candidate binds same-file.
		recvHint, _ := e.Meta["fn_ref_recv_hint"].(string)
		ungated, _ := e.Meta["fn_value_ungated"].(bool)
		skipGate, _ := e.Meta["skip_gate"].(bool)
		target := ""
		conf := 0.6
		origin := graph.OriginASTInferred
		switch {
		case skipGate:
			// Curated-HOF string callable: bypass same-file scope and bind by a
			// repo-wide unique-or-drop rule (a `Class::method` string scopes to
			// the type).
			if recvHint != "" {
				target = resolveMemberByType(g, recvHint, name)
			}
			if target == "" {
				target = resolveUniqueFnValue(g, name)
			}
			conf = 0.5
		case recvHint == "<self>":
			if target = resolveFnValueSelfMember(g, e.From, name); target != "" {
				conf, origin = 0.85, graph.OriginASTResolved
			} else {
				target = resolveFnValueName(g, e.FilePath, name)
			}
		case recvHint != "":
			if target = resolveMemberByType(g, recvHint, name); target != "" {
				conf, origin = 0.85, graph.OriginASTResolved
			} else if ungated {
				target = resolveFnValueCrossModule(g, name)
				conf = 0.45
			}
		default:
			target = resolveFnValueName(g, e.FilePath, name)
			if target == "" && ungated {
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
		meta := map[string]any{
			"via":             fnValueRegistrationVia,
			metaFnValueName:   name,
			MetaSynthesizedBy: SynthFnValueCallback,
			MetaProvenance:    ProvenanceHeuristic,
		}
		if form, _ := e.Meta["fn_ref_form"].(string); form != "" {
			meta["fn_ref_form"] = form
		}
		landed = append(landed, &graph.Edge{
			From:            e.From,
			To:              target,
			Kind:            graph.EdgeReferences,
			FilePath:        e.FilePath,
			Line:            e.Line,
			Confidence:      conf,
			ConfidenceLabel: graph.ConfidenceLabelFor(graph.EdgeReferences, conf),
			Origin:          origin,
			Meta:            meta,
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

// resolveUniqueFnValue returns the ID of the sole function/method named name in
// the repo, or "" when none or more than one exists (unique-or-drop). The
// shared repo-wide resolution rule for qualified-path and gate-skipping
// (curated-HOF string) function values.
func resolveUniqueFnValue(g graph.Store, name string) string {
	match := ""
	for _, n := range g.FindNodesByName(name) {
		if n == nil {
			continue
		}
		if n.Kind != graph.KindFunction && n.Kind != graph.KindMethod {
			continue
		}
		if match != "" && match != n.ID {
			return "" // ambiguous — drop
		}
		match = n.ID
	}
	return match
}

// resolveFnValueCrossModule binds a function value to a uniquely-named
// function/method anywhere in the repo, skipping any candidate with file-local
// linkage (a C/C++ `static` function, stamped scope_static): such a definition
// is invisible outside its translation unit, so a cross-module reference can
// never target it, and a same-named static in an unrelated file must not make
// the name look ambiguous. The same-file path is preferred by the caller; this
// is the cross-module fallback.
func resolveFnValueCrossModule(g graph.Store, name string) string {
	match := ""
	for _, n := range g.FindNodesByName(name) {
		if n == nil {
			continue
		}
		if n.Kind != graph.KindFunction && n.Kind != graph.KindMethod {
			continue
		}
		if isFileLocalLinkage(n) {
			continue
		}
		if match != "" && match != n.ID {
			return "" // ambiguous — drop
		}
		match = n.ID
	}
	return match
}

// isFileLocalLinkage reports whether a node was stamped with translation-unit
// (C/C++ static) linkage, so it cannot be the target of a cross-module value
// reference.
func isFileLocalLinkage(n *graph.Node) bool {
	if n.Meta == nil {
		return false
	}
	v, _ := n.Meta["scope_static"].(bool)
	return v
}

// resolveMemberByType binds member to a uniquely-named method of typeName
// (matched via Meta["receiver"]), or "" when none or more than one matches.
// Shared scope rule for `Foo::bar`-style references and self-member resolution.
func resolveMemberByType(g graph.Store, typeName, member string) string {
	if typeName == "" || member == "" {
		return ""
	}
	match := ""
	for _, n := range g.FindNodesByName(member) {
		if n == nil || n.Kind != graph.KindMethod {
			continue
		}
		if recv, _ := n.Meta["receiver"].(string); recv != typeName {
			continue
		}
		if match != "" && match != n.ID {
			return "" // ambiguous within the type — drop
		}
		match = n.ID
	}
	return match
}

// resolveFnValueSelfMember binds a `this.m` / `self.m` member reference against
// the methods of the registration site's enclosing type, so it can never bind
// a coincidentally-named top-level function.
func resolveFnValueSelfMember(g graph.Store, fromID, member string) string {
	from := g.GetNode(fromID)
	if from == nil || from.Meta == nil {
		return ""
	}
	recv, _ := from.Meta["receiver"].(string)
	if recv == "" {
		return ""
	}
	return resolveMemberByType(g, recv, member)
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
