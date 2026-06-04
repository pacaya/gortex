package resolver

import "github.com/zzet/gortex/internal/graph"

// sqlCallsiteVia marks the placeholder call edge a JS/TS/Python SQL
// function call site emits (mirrors languages.sqlCallsiteVia).
const sqlCallsiteVia = "sql.callsite"

// sqlCallPrefix is the placeholder namespace for an unresolved SQL
// function call (`unresolved::sqlfn::<name>`).
const sqlCallPrefix = unresolvedPrefix + "sqlfn::"

// ResolveSQLCallsites is the framework-dispatch synthesizer that links a
// cross-language SQL function call site (Supabase/PostgREST `.rpc('fn')`,
// SQLAlchemy `func.fn()`) to the SQL `CREATE FUNCTION` node it invokes.
// The language extractors emit a placeholder EdgeCalls to
// `unresolved::sqlfn::<name>` carrying Meta["via"]="sql.callsite"; this
// pass lands it on the unique SQL KindFunction node of that name.
//
// Full recompute and idempotent: the target is recomputed from the edge's
// sql_function meta, so a reindex of either side re-lands or re-orphans
// the edge. graph.ReindexEdges keeps the adjacency buckets consistent.
// An ambiguous name (two SQL functions) is left on the placeholder.
//
// Returns the number of call edges landed on a SQL function.
func ResolveSQLCallsites(g graph.Store) int {
	if g == nil {
		return 0
	}
	sqlFns := map[string][]*graph.Node{}
	for _, n := range nodesByKindsOrAll(g, graph.KindFunction) {
		if n != nil && n.Language == "sql" && n.Name != "" {
			sqlFns[n.Name] = append(sqlFns[n.Name], n)
		}
	}
	if len(sqlFns) == 0 {
		return 0
	}

	resolved := 0
	var reindex []graph.EdgeReindex
	for e := range g.EdgesByKind(graph.EdgeCalls) {
		if e == nil || e.Meta == nil {
			continue
		}
		if v, _ := e.Meta["via"].(string); v != sqlCallsiteVia {
			continue
		}
		fn, _ := e.Meta["sql_function"].(string)
		if fn == "" {
			continue
		}
		var target *graph.Node
		if cands := sqlFns[fn]; len(cands) == 1 {
			target = cands[0]
		}
		want := sqlCallPrefix + fn
		if target != nil {
			want = target.ID
		}
		if e.To == want {
			if target != nil {
				resolved++
			}
			continue
		}
		oldTo := e.To
		e.To = want
		if target != nil {
			e.Origin = graph.OriginASTInferred
			e.Confidence = 0.6
			e.ConfidenceLabel = graph.ConfidenceLabelFor(graph.EdgeCalls, 0.6)
			StampSynthesized(e, SynthSQLCallsite)
			resolved++
		} else {
			// Re-orphaned (the SQL function disappeared / became ambiguous).
			e.Origin = graph.OriginASTInferred
			e.Confidence = 0
			e.ConfidenceLabel = ""
			UnstampSynthesized(e)
		}
		reindex = append(reindex, graph.EdgeReindex{Edge: e, OldTo: oldTo})
	}
	if len(reindex) > 0 {
		g.ReindexEdges(reindex)
	}
	return resolved
}
