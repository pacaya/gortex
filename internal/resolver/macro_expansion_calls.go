package resolver

import (
	"sort"

	"github.com/zzet/gortex/internal/graph"
)

// macroExpansionVia marks a call edge synthesized at a function-like
// macro's expansion (use) site.
//
// The C/C++ extractor recovers the calls hidden inside a function-like
// macro's replacement list and attributes them to the macro's `#define`
// line — that is where the call *text* lives. But it is not where the
// call *happens*: a `CALL_M(o);` in real code expands to `(o)->run()` at
// the invocation line, inside the invoking function. This pass adds, at
// the use site, the caller -> callee edge the expansion implies, so a
// forward call walk (get_call_chain) shows `caller -> run` where the
// macro is invoked, not only at the definition.
const macroExpansionVia = "macro_expansion"

// macroFunctionKindMeta is the Meta["macro_kind"] value the extractor
// stamps on a function-like (parameterised) macro node — the only macro
// shape whose use site is a call_expression and whose body can hide
// calls. Object-like macros are skipped.
const macroFunctionKindMeta = "function"

// ResolveMacroExpansionCalls mints, for every site that invokes a
// function-like macro, a direct caller -> callee call edge for each call
// the macro's body hides — attributed to the USE-SITE line and file
// rather than the macro's `#define` line.
//
// Recoverable seam: a function-like macro invocation `CALL_M(o)` parses
// (the preprocessor is not run) as an ordinary call_expression naming
// CALL_M, so the extractor emits `caller --(use line)--> unresolved::CALL_M`.
// The macro node itself is KindMacro, which the call resolver never binds
// (it accepts only function / method targets), so the use-site edge stays
// an `unresolved::<macro>` placeholder carrying the use-site line — the
// durable signal this pass keys on. The macro node already carries the
// recovered body callees as its out-edges (`macro --(define line)-->
// callee`, emitted by the extractor).
//
// For each use-site edge whose name uniquely binds — within the caller's
// repo — to one function-like macro that recovered callees, the pass
// emits `caller --(use line)--> callee` for each recovered callee. The
// edge rides at OriginASTInferred (a heuristic materialisation, never an
// upgrade over a compiler-verified fact) and carries the macro-expansion
// provenance. It is idempotent: the edge key includes the line,
// graph.AddEdge dedupes by key, and an existing edge at that key authored
// by anything other than this pass is left untouched, so a real (e.g.
// LSP-resolved) edge is never overwritten or downgraded.
//
// Conservative by construction: a macro name bound by more than one
// function-like macro in the same repo is ambiguous and skipped; the
// macro's def-site recovered-callee edges and the use-site placeholder
// both remain, so this pass only ever adds the use-site attribution and
// never removes the existing edges.
//
// Returns the number of use-site call edges the pass owns after this run.
func ResolveMacroExpansionCalls(g graph.Store) int {
	if g == nil {
		return 0
	}

	// Index function-like macros that recovered body callees, keyed by
	// (repo, name). A name bound by more than one such macro in a repo is
	// ambiguous: mark it so every use site of that name is skipped.
	type macroEntry struct {
		node      *graph.Node
		callees   []string
		ambiguous bool
	}
	byKey := map[string]*macroEntry{}
	macroKey := func(repo, name string) string { return repo + "\x00" + name }

	for _, n := range nodesByKindsOrAll(g, graph.KindMacro) {
		if n == nil || n.Meta == nil || n.Name == "" {
			continue
		}
		if k, _ := n.Meta["macro_kind"].(string); k != macroFunctionKindMeta {
			continue
		}
		callees := macroBodyCallees(g, n.ID)
		if len(callees) == 0 {
			continue
		}
		key := macroKey(n.RepoPrefix, n.Name)
		if e, ok := byKey[key]; ok {
			e.ambiguous = true
			continue
		}
		byKey[key] = &macroEntry{node: n, callees: callees}
	}
	if len(byKey) == 0 {
		return 0
	}

	// Collect use-site edges first so the AddEdge writes below cannot
	// mutate a live EdgesByKind iteration. A use site is either an
	// unresolved `unresolved::<macro>` placeholder (the common case — the
	// call resolver leaves macro-named calls unresolved) or a call edge
	// landed directly on the macro node (defensive: no resolver does this
	// today, but a future one might).
	type useSite struct {
		caller string
		file   string
		line   int
		entry  *macroEntry
	}
	var sites []useSite
	for e := range g.EdgesByKind(graph.EdgeCalls) {
		if e == nil || e.From == "" || e.To == "" {
			continue
		}
		var entry *macroEntry
		switch {
		case graph.IsUnresolvedTarget(e.To):
			name := graph.UnresolvedName(e.To)
			if name == "" {
				continue
			}
			caller := g.GetNode(e.From)
			if caller == nil {
				continue
			}
			ent, ok := byKey[macroKey(caller.RepoPrefix, name)]
			if !ok || ent.ambiguous {
				continue
			}
			entry = ent
		default:
			target := g.GetNode(e.To)
			if target == nil || target.Kind != graph.KindMacro {
				continue
			}
			ent, ok := byKey[macroKey(target.RepoPrefix, target.Name)]
			if !ok || ent.ambiguous || ent.node.ID != target.ID {
				continue
			}
			entry = ent
		}
		sites = append(sites, useSite{caller: e.From, file: e.FilePath, line: e.Line, entry: entry})
	}
	if len(sites) == 0 {
		return 0
	}

	// Deterministic order so the owned-edge count and the batched writes
	// are stable across runs.
	sort.Slice(sites, func(i, j int) bool {
		if sites[i].caller != sites[j].caller {
			return sites[i].caller < sites[j].caller
		}
		if sites[i].file != sites[j].file {
			return sites[i].file < sites[j].file
		}
		if sites[i].line != sites[j].line {
			return sites[i].line < sites[j].line
		}
		return sites[i].entry.node.ID < sites[j].entry.node.ID
	})

	owned := 0
	for _, s := range sites {
		for _, callee := range s.entry.callees {
			if callee == s.caller {
				continue
			}
			if existing := existingCallEdge(g, s.caller, callee, s.file, s.line); existing != nil {
				if v, _ := existing.Meta["via"].(string); v != macroExpansionVia {
					// A real edge already occupies this exact identity
					// slot — never overwrite or downgrade it.
					continue
				}
			}
			g.AddEdge(&graph.Edge{
				From:            s.caller,
				To:              callee,
				Kind:            graph.EdgeCalls,
				FilePath:        s.file,
				Line:            s.line,
				Confidence:      ConfidenceHeuristic,
				ConfidenceLabel: graph.ConfidenceLabelFor(graph.EdgeCalls, ConfidenceHeuristic),
				Origin:          graph.OriginASTInferred,
				Meta: map[string]any{
					"via":             macroExpansionVia,
					"macro":           s.entry.node.Name,
					MetaSynthesizedBy: SynthMacroExpansion,
					MetaProvenance:    ProvenanceHeuristic,
				},
			})
			owned++
		}
	}
	return owned
}

// macroBodyCallees returns the distinct call targets a macro node's body
// recovered — the `To` of each EdgeCalls out-edge the extractor emitted
// from the macro node — in stable order.
func macroBodyCallees(g graph.Store, macroID string) []string {
	seen := map[string]bool{}
	var out []string
	for _, e := range g.GetOutEdges(macroID) {
		if e == nil || e.Kind != graph.EdgeCalls || e.To == "" {
			continue
		}
		if seen[e.To] {
			continue
		}
		seen[e.To] = true
		out = append(out, e.To)
	}
	sort.Strings(out)
	return out
}

// existingCallEdge returns the call edge already at the exact
// (from, to, file, line) identity slot, or nil. Used both to keep the
// pass idempotent and to refuse to overwrite a real edge.
func existingCallEdge(g graph.Store, from, to, file string, line int) *graph.Edge {
	for _, e := range g.GetOutEdges(from) {
		if e != nil && e.Kind == graph.EdgeCalls && e.To == to && e.FilePath == file && e.Line == line {
			return e
		}
	}
	return nil
}
