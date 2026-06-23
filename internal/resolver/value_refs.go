package resolver

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// SynthValueRef tags a resolved value-reference read edge.
const SynthValueRef = "value-ref"

const (
	// valueRefCandidateVia marks an extractor-emitted placeholder read of a
	// distinctive identifier; valueRefVia marks the bound read this pass lands.
	valueRefCandidateVia = "value_ref_candidate"
	valueRefVia          = "value_ref"
)

// ResolveValueRefs binds each captured distinctive-name value reference to the
// file-scope constant / variable it reads and re-targets the placeholder into a
// tiered EdgeReads from the reader to that constant.
//
// This closes a change-impact gap: a config constant's readers were invisible
// to blast-radius analysis — fillImpactLive follows every incoming edge except
// Defines/MemberOf, so without a read edge "change this constant → who breaks"
// missed every reader that referenced it outside a captured call/arg position.
// Beat: the read rides a provenance tier (min_tier-filterable), where a flat
// reference is not.
//
// Precision gates: only distinctive names bind (>=3 chars with an uppercase
// letter or underscore — the config-constant shape); a candidate whose name is
// shadowed by a same-file parameter, field, or inner-scope local declarator is
// dropped; a reader in a generated file is skipped; self-reads are ignored.
// Unresolved candidates are
// left as inert placeholders. Idempotent: re-targeting to the same constant is
// a no-op and graph.EvictFile drops the edges on reindex.
func ResolveValueRefs(g graph.Store) int {
	if g == nil {
		return 0
	}
	constByFile := map[string]map[string]string{}
	shadowByFile := map[string]map[string]bool{}
	for _, n := range nodesByKindsOrAll(g, graph.KindConstant, graph.KindVariable, graph.KindParam, graph.KindField, graph.KindLocal) {
		if n == nil || n.FilePath == "" {
			continue
		}
		switch n.Kind {
		case graph.KindConstant, graph.KindVariable:
			if !isDistinctiveValueName(n.Name) {
				continue
			}
			m := constByFile[n.FilePath]
			if m == nil {
				m = map[string]string{}
				constByFile[n.FilePath] = m
			}
			if _, ok := m[n.Name]; !ok {
				m[n.Name] = n.ID
			}
		case graph.KindParam, graph.KindField, graph.KindLocal:
			// Declarator census: any same-file parameter, field, or
			// inner-scope local (`let X` / `:= X`) materialised by the
			// per-grammar local-binding pass declares this name in a scope
			// that shadows the file-scope constant. A candidate read of the
			// name might resolve to that declarator, not the constant, so the
			// file-level binding is dropped rather than mis-bound — the same
			// coarse, conservative gate the param/field pruning already used.
			s := shadowByFile[n.FilePath]
			if s == nil {
				s = map[string]bool{}
				shadowByFile[n.FilePath] = s
			}
			s[n.Name] = true
		}
	}
	if len(constByFile) == 0 {
		return 0
	}

	resolved := 0
	var reindex []graph.EdgeReindex
	for e := range g.EdgesByKind(graph.EdgeReads) {
		if e == nil || e.Meta == nil {
			continue
		}
		if via, _ := e.Meta["via"].(string); via != valueRefCandidateVia {
			continue
		}
		name, _ := e.Meta["name"].(string)
		consts := constByFile[e.FilePath]
		if name == "" || consts == nil {
			continue
		}
		constID, ok := consts[name]
		if !ok || constID == e.From {
			continue
		}
		if sh := shadowByFile[e.FilePath]; sh != nil && sh[name] {
			continue
		}
		if reader := g.GetNode(e.From); reader != nil && isGeneratedReader(reader) {
			continue
		}
		if e.To == constID {
			resolved++
			continue
		}
		oldTo := e.To
		e.To = constID
		e.Origin = graph.OriginASTResolved
		e.Confidence = 0.7
		e.ConfidenceLabel = graph.ConfidenceLabelFor(graph.EdgeReads, 0.7)
		e.Meta["via"] = valueRefVia
		StampSynthesized(e, SynthValueRef)
		reindex = append(reindex, graph.EdgeReindex{Edge: e, OldTo: oldTo})
		resolved++
	}
	if len(reindex) > 0 {
		g.ReindexEdges(reindex)
	}
	return resolved
}

// isDistinctiveValueName reports whether name has the config-constant shape:
// at least 3 characters and at least one uppercase letter or underscore. This
// keeps the value-ref binding to names unlikely to collide with an ordinary
// local (which is conventionally lowerCamelCase).
func isDistinctiveValueName(name string) bool {
	if len(name) < 3 {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c == '_' || (c >= 'A' && c <= 'Z') {
			return true
		}
	}
	return false
}

// isGeneratedReader reports whether a node lives in a generated file, which is
// excluded from value-ref binding (its reads are machine-emitted noise).
func isGeneratedReader(n *graph.Node) bool {
	if n.Meta != nil {
		if gen, _ := n.Meta["generated"].(bool); gen {
			return true
		}
	}
	p := n.FilePath
	return strings.Contains(p, ".pb.go") ||
		strings.Contains(p, ".g.dart") ||
		strings.Contains(p, "_generated.") ||
		strings.Contains(p, ".generated.") ||
		strings.HasSuffix(p, ".gen.go")
}
