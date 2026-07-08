package store_sqlite

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// These methods were added to graph.Store after the sqlite backend was
// first removed; they are restored here so *Store satisfies the current
// interface. All reuse the chunked IN-list / raw-SQL helpers in store.go
// (queryNodesSQL / queryEdgesSQL / lookupChunkSize / minInt). SQLite's
// planner drives every one through the existing secondary indexes.

// lookupNodeCols is the canonical node column list (and scan order) for
// every node-shaped SELECT in the package. It must stay in sync with
// scanNode. The struct columns (start_column/end_column) sit with the line
// range; the promoted meta columns (signature/visibility/doc/external/
// return_type/is_async/is_static/is_abstract/is_exported/updated_at/
// data_class/semantic_type/semantic_source) precede meta.
const lookupNodeCols = `id, kind, name, qual_name, file_path, start_line, end_line, start_column, end_column, language, repo_prefix, workspace_id, project_id, signature, visibility, doc, external, return_type, is_async, is_static, is_abstract, is_exported, updated_at, data_class, semantic_type, semantic_source, meta`

// lookupNodeColsLight is lookupNodeCols without the trailing meta column —
// the projection GetRepoNodesLight uses so a repo-scoped scan never
// transfers or decodes a single blob. Derived, not hand-duplicated, so it
// can never drift out of sync with lookupNodeCols / scanNode.
var lookupNodeColsLight = strings.TrimSuffix(lookupNodeCols, ", meta")

const lookupEdgeCols = `from_id, to_id, kind, file_path, line, confidence, confidence_label, origin, tier, cross_repo, meta, resolve_terminal, resolve_terminal_reason`

// Compile-time assertion: *Store satisfies graph.NodeNameClassCounter.
var _ graph.NodeNameClassCounter = (*Store)(nil)

// CountNodesByNameClass implements graph.NodeNameClassCounter: for each
// distinct name, it tallies how many nodes.name matches are Real (is_stub =
// 0 and kind IN definitionKinds) vs Stub (is_stub = 1), server-side via
// nodes_by_name — one aggregate query per chunk instead of one
// FindNodesByName round trip per name. A name absent from the returned map
// has no matching node at all (Real == Stub == 0 either way).
func (s *Store) CountNodesByNameClass(names []string, definitionKinds []graph.NodeKind) map[string]graph.NodeNameClassCount {
	_, kindArgs := aggDedupeNodeKinds(definitionKinds)
	if len(kindArgs) == 0 {
		return nil
	}
	uniq := dedupeNonEmpty(names)
	if len(uniq) == 0 {
		return nil
	}
	out := make(map[string]graph.NodeNameClassCount, len(uniq))
	kindPlaceholders := inPlaceholders(len(kindArgs))
	for i := 0; i < len(uniq); i += lookupChunkSize {
		end := minInt(i+lookupChunkSize, len(uniq))
		chunk := uniq[i:end]
		q := `SELECT name,
		             SUM(CASE WHEN is_stub = 0 AND kind IN (` + kindPlaceholders + `) THEN 1 ELSE 0 END),
		             SUM(CASE WHEN is_stub = 1 THEN 1 ELSE 0 END)
		        FROM nodes
		       WHERE name IN (` + inPlaceholders(len(chunk)) + `)
		       GROUP BY name`
		args := make([]any, 0, len(kindArgs)+len(chunk))
		args = append(args, kindArgs...)
		args = append(args, toAnyArgs(chunk)...)
		rows, err := s.db.Query(q, args...)
		if err != nil {
			panicOnFatal(err)
			return out
		}
		for rows.Next() {
			var name string
			var c graph.NodeNameClassCount
			if err := rows.Scan(&name, &c.Real, &c.Stub); err != nil {
				_ = rows.Close()
				panicOnFatal(err)
				return out
			}
			out[name] = c
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			panicOnFatal(err)
			return out
		}
		_ = rows.Close()
	}
	return out
}

// FindNodesByNameContaining returns nodes whose Name contains substr,
// case-insensitively (SQLite's LIKE is ASCII case-insensitive). An empty
// substring matches nothing (parity with the in-memory store); a limit > 0
// caps the result set. The leading-wildcard LIKE is a deliberate full scan —
// no index accelerates an unanchored substring — matching the in-memory
// strings.Contains fallback. % and _ in substr are escaped so they match
// literally.
func (s *Store) FindNodesByNameContaining(substr string, limit int) []*graph.Node {
	if substr == "" {
		return nil
	}
	pattern := "%" + escapeLikePattern(substr) + "%"
	q := `SELECT ` + lookupNodeCols + ` FROM nodes WHERE name LIKE ? ESCAPE '\' ORDER BY id`
	if limit > 0 {
		return s.queryNodesSQL(q+` LIMIT ?`, pattern, limit)
	}
	return s.queryNodesSQL(q, pattern)
}

// GetNodesByQualNames returns a map qualName→*Node (first match per
// qual_name) for the batch — the qual-name twin of FindNodesByNames, used to
// pre-warm import resolution. Driven by the unique nodes_by_qual index.
func (s *Store) GetNodesByQualNames(qualNames []string) map[string]*graph.Node {
	uniq := dedupeNonEmpty(qualNames)
	if len(uniq) == 0 {
		return nil
	}
	out := make(map[string]*graph.Node, len(uniq))
	for i := 0; i < len(uniq); i += lookupChunkSize {
		end := minInt(i+lookupChunkSize, len(uniq))
		chunk := uniq[i:end]
		q := `SELECT ` + lookupNodeCols + ` FROM nodes WHERE qual_name IN (` + inPlaceholders(len(chunk)) + `)`
		for _, n := range s.queryNodesSQL(q, toAnyArgs(chunk)...) {
			if n == nil {
				continue
			}
			if _, ok := out[n.QualName]; !ok {
				out[n.QualName] = n
			}
		}
	}
	return out
}

// GetOutEdgesByNodeIDs batches per-node out-edge fan-out into one query per
// chunk. Missing IDs are simply absent from the returned map.
func (s *Store) GetOutEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	return s.edgesByNodeIDs(ids, "from_id", func(e *graph.Edge) string { return e.From })
}

// GetInEdgesByNodeIDs is the incoming-edge twin of GetOutEdgesByNodeIDs.
func (s *Store) GetInEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	return s.edgesByNodeIDs(ids, "to_id", func(e *graph.Edge) string { return e.To })
}

// edgesByNodeIDs runs the chunked IN-list edge fetch keyed on the given
// column (from_id or to_id), grouping results by the supplied key extractor.
func (s *Store) edgesByNodeIDs(ids []string, col string, key func(*graph.Edge) string) map[string][]*graph.Edge {
	uniq := dedupeNonEmpty(ids)
	if len(uniq) == 0 {
		return nil
	}
	out := make(map[string][]*graph.Edge, len(uniq))
	for i := 0; i < len(uniq); i += lookupChunkSize {
		end := minInt(i+lookupChunkSize, len(uniq))
		chunk := uniq[i:end]
		q := `SELECT ` + lookupEdgeCols + ` FROM edges WHERE ` + col + ` IN (` + inPlaceholders(len(chunk)) + `)`
		for _, e := range s.queryEdgesSQL(q, toAnyArgs(chunk)...) {
			if e == nil {
				continue
			}
			k := key(e)
			out[k] = append(out[k], e)
		}
	}
	return out
}

// dedupeNonEmpty drops empties and duplicates, preserving first-seen order.
func dedupeNonEmpty(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, v := range in {
		if v == "" {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}

// inPlaceholders returns "?,?,?" for n bound parameters.
func inPlaceholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.Repeat(",?", n)[1:]
}

// toAnyArgs widens a string slice for variadic Query/Exec args.
func toAnyArgs(ss []string) []any {
	args := make([]any, len(ss))
	for i, v := range ss {
		args[i] = v
	}
	return args
}

// escapeLikePattern escapes the LIKE metacharacters so the substring matches
// literally under `... LIKE ? ESCAPE '\'`.
func escapeLikePattern(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}
