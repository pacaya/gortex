package store_sqlite

import (
	"iter"
	"sort"

	"github.com/zzet/gortex/internal/graph"
)

// This file implements the trivial SQL aggregator / scanner optional
// capability interfaces from graph.Store. Each method pushes its
// GROUP BY / WHERE / COUNT into SQLite so the planner drives it through
// the schema's secondary indexes, returning only the aggregate rows
// instead of materialising the whole node / edge table Go-side.
//
// Conventions shared across these methods:
//   - Empty / nil input returns nil (parity with the in-memory store).
//   - Input id / kind slices are deduped before they reach the IN-list.
//   - Large IN-lists are chunked by lookupChunkSize.
//   - agg-prefixed helpers are local to this file.

var (
	_ graph.InEdgeCounter            = (*Store)(nil)
	_ graph.NodeIDsByKinds           = (*Store)(nil)
	_ graph.EdgeKindCounter          = (*Store)(nil)
	_ graph.NodeDegreeByKinds        = (*Store)(nil)
	_ graph.NodesInFilesByKindFinder = (*Store)(nil)
	_ graph.FileImportAggregator     = (*Store)(nil)
	_ graph.InDegreeForNodes         = (*Store)(nil)
	_ graph.CrossRepoEdgeAggregator  = (*Store)(nil)
	_ graph.FileImporters            = (*Store)(nil)
	_ graph.FileSymbolNamesByPaths   = (*Store)(nil)
	_ graph.EdgesByKindsScanner      = (*Store)(nil)
	_ graph.NodesByKindsScanner      = (*Store)(nil)
	_ graph.EdgeAdjacencyForKinds    = (*Store)(nil)
	_ graph.NodeDegreeAggregator     = (*Store)(nil)
	_ graph.NodeFanAggregator        = (*Store)(nil)
)

// aggDedupeEdgeKinds drops empties and duplicates from an edge-kind
// slice, preserving first-seen order; returns the kinds widened to the
// []any an IN-list binds.
func aggDedupeEdgeKinds(kinds []graph.EdgeKind) (uniq []graph.EdgeKind, args []any) {
	seen := make(map[graph.EdgeKind]struct{}, len(kinds))
	for _, k := range kinds {
		if k == "" {
			continue
		}
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		uniq = append(uniq, k)
		args = append(args, string(k))
	}
	return uniq, args
}

// aggDedupeNodeKinds is the node-kind twin of aggDedupeEdgeKinds.
func aggDedupeNodeKinds(kinds []graph.NodeKind) (uniq []graph.NodeKind, args []any) {
	seen := make(map[graph.NodeKind]struct{}, len(kinds))
	for _, k := range kinds {
		if k == "" {
			continue
		}
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		uniq = append(uniq, k)
		args = append(args, string(k))
	}
	return uniq, args
}

// InEdgeCountsByKind returns per-target incoming-edge counts for the
// supplied edge kinds, grouped server-side via edges_by_to.
func (s *Store) InEdgeCountsByKind(kinds []graph.EdgeKind) map[string]int {
	_, args := aggDedupeEdgeKinds(kinds)
	if len(args) == 0 {
		return nil
	}
	q := `SELECT to_id, COUNT(*) FROM edges WHERE kind IN (` + inPlaceholders(len(args)) + `) GROUP BY to_id`
	rows, err := s.db.Query(q, args...)
	panicOnFatal(err)
	defer rows.Close()
	out := make(map[string]int)
	for rows.Next() {
		var id string
		var n int
		panicOnFatal(rows.Scan(&id, &n))
		out[id] = n
	}
	panicOnFatal(rows.Err())
	return out
}

// NodeIDsByKinds returns the deduplicated IDs of every node whose kind
// is in the supplied set.
func (s *Store) NodeIDsByKinds(kinds []graph.NodeKind) []string {
	_, args := aggDedupeNodeKinds(kinds)
	if len(args) == 0 {
		return nil
	}
	q := `SELECT id FROM nodes WHERE kind IN (` + inPlaceholders(len(args)) + `) ORDER BY id`
	rows, err := s.db.Query(q, args...)
	panicOnFatal(err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		panicOnFatal(rows.Scan(&id))
		out = append(out, id)
	}
	panicOnFatal(rows.Err())
	return out
}

// EdgeKindCounts returns one entry per distinct edge kind with its
// occurrence count across the whole graph.
func (s *Store) EdgeKindCounts() map[graph.EdgeKind]int {
	rows, err := s.db.Query(`SELECT kind, COUNT(*) FROM edges GROUP BY kind`)
	panicOnFatal(err)
	defer rows.Close()
	out := make(map[graph.EdgeKind]int)
	for rows.Next() {
		var kind string
		var n int
		panicOnFatal(rows.Scan(&kind, &n))
		out[graph.EdgeKind(kind)] = n
	}
	panicOnFatal(rows.Err())
	return out
}

// NodeDegreeByKinds returns total in/out degree for every node whose
// kind is in the set (optionally under pathPrefix); UsageInCount is
// always 0 for this capability.
func (s *Store) NodeDegreeByKinds(kinds []graph.NodeKind, pathPrefix string) []graph.NodeDegreeRow {
	_, kindArgs := aggDedupeNodeKinds(kinds)
	if len(kindArgs) == 0 {
		return nil
	}
	args := append([]any(nil), kindArgs...)
	q := `SELECT n.id,
		(SELECT COUNT(*) FROM edges e WHERE e.to_id = n.id) AS in_count,
		(SELECT COUNT(*) FROM edges e WHERE e.from_id = n.id) AS out_count
	FROM nodes n
	WHERE n.kind IN (` + inPlaceholders(len(kindArgs)) + `)`
	if pathPrefix != "" {
		q += ` AND n.file_path LIKE ? ESCAPE '\'`
		args = append(args, escapeLikePattern(pathPrefix)+"%")
	}
	q += ` ORDER BY n.id`
	rows, err := s.db.Query(q, args...)
	panicOnFatal(err)
	defer rows.Close()
	var out []graph.NodeDegreeRow
	for rows.Next() {
		var r graph.NodeDegreeRow
		panicOnFatal(rows.Scan(&r.NodeID, &r.InCount, &r.OutCount))
		out = append(out, r)
	}
	panicOnFatal(rows.Err())
	return out
}

// NodesInFilesByKind returns every node living in one of the supplied
// files whose kind is in the supplied set.
func (s *Store) NodesInFilesByKind(files []string, kinds []graph.NodeKind) []*graph.Node {
	uniqFiles := dedupeNonEmpty(files)
	_, kindArgs := aggDedupeNodeKinds(kinds)
	if len(uniqFiles) == 0 || len(kindArgs) == 0 {
		return nil
	}
	var out []*graph.Node
	for i := 0; i < len(uniqFiles); i += lookupChunkSize {
		end := minInt(i+lookupChunkSize, len(uniqFiles))
		chunk := uniqFiles[i:end]
		args := append(toAnyArgs(chunk), kindArgs...)
		q := `SELECT ` + lookupNodeCols + ` FROM nodes WHERE file_path IN (` +
			inPlaceholders(len(chunk)) + `) AND kind IN (` + inPlaceholders(len(kindArgs)) + `) ORDER BY id`
		out = append(out, s.queryNodesSQL(q, args...)...)
	}
	return out
}

// FileImportCounts returns per-target-file incoming-import counts. A
// nil scope counts every import edge; a non-nil scope bounds counts to
// edges whose target node ID lies in the slice (empty non-nil => nil).
func (s *Store) FileImportCounts(scope []string) []graph.FileImportCountRow {
	if scope != nil && len(scope) == 0 {
		return nil
	}
	base := `SELECT COALESCE(NULLIF(n.file_path, ''), n.id) AS path, COUNT(*) AS cnt
		FROM edges e JOIN nodes n ON e.to_id = n.id
		WHERE e.kind = ?`
	args := []any{string(graph.EdgeImports)}
	fileToCount := make(map[string]int)
	if scope == nil {
		q := base + ` GROUP BY path`
		aggScanImportCounts(s, q, args, fileToCount)
	} else {
		uniq := dedupeNonEmpty(scope)
		if len(uniq) == 0 {
			return nil
		}
		for i := 0; i < len(uniq); i += lookupChunkSize {
			end := minInt(i+lookupChunkSize, len(uniq))
			chunk := uniq[i:end]
			q := base + ` AND e.to_id IN (` + inPlaceholders(len(chunk)) + `) GROUP BY path`
			aggScanImportCounts(s, q, append(append([]any(nil), args...), toAnyArgs(chunk)...), fileToCount)
		}
	}
	if len(fileToCount) == 0 {
		return nil
	}
	out := make([]graph.FileImportCountRow, 0, len(fileToCount))
	for path, cnt := range fileToCount {
		out = append(out, graph.FileImportCountRow{FilePath: path, Count: cnt})
	}
	return out
}

// aggScanImportCounts runs an import-count query and folds the (path,
// count) rows into the accumulator (chunked scopes can revisit a path).
func aggScanImportCounts(s *Store, q string, args []any, acc map[string]int) {
	rows, err := s.db.Query(q, args...)
	panicOnFatal(err)
	defer rows.Close()
	for rows.Next() {
		var path string
		var cnt int
		panicOnFatal(rows.Scan(&path, &cnt))
		acc[path] += cnt
	}
	panicOnFatal(rows.Err())
}

// InDegreeForNodes returns total incoming-edge counts (any kind) for
// the supplied node id set.
func (s *Store) InDegreeForNodes(ids []string) map[string]int {
	uniq := dedupeNonEmpty(ids)
	if len(uniq) == 0 {
		return nil
	}
	out := make(map[string]int)
	for i := 0; i < len(uniq); i += lookupChunkSize {
		end := minInt(i+lookupChunkSize, len(uniq))
		chunk := uniq[i:end]
		q := `SELECT to_id, COUNT(*) FROM edges WHERE to_id IN (` +
			inPlaceholders(len(chunk)) + `) GROUP BY to_id`
		rows, err := s.db.Query(q, toAnyArgs(chunk)...)
		panicOnFatal(err)
		for rows.Next() {
			var id string
			var n int
			panicOnFatal(rows.Scan(&id, &n))
			out[id] = n
		}
		panicOnFatal(rows.Err())
		rows.Close()
	}
	return out
}

// CrossRepoEdgeCounts returns pre-grouped cross-repo edge counts keyed
// by (base kind, from-repo, to-repo). Cross-repo kinds are those
// graph.BaseKindForCrossRepo recognises; the count is reported under
// the base kind.
func (s *Store) CrossRepoEdgeCounts() []graph.CrossRepoEdgeRow {
	q := `SELECT e.kind, nf.repo_prefix, nt.repo_prefix, COUNT(*)
		FROM edges e
		JOIN nodes nf ON e.from_id = nf.id
		JOIN nodes nt ON e.to_id = nt.id
		WHERE nf.repo_prefix <> nt.repo_prefix
		GROUP BY e.kind, nf.repo_prefix, nt.repo_prefix`
	rows, err := s.db.Query(q)
	panicOnFatal(err)
	defer rows.Close()
	// Aggregate keyed by the edge's OWN kind (cross_repo_*), NOT the base.
	// BaseKindForCrossRepo is used only as the recogniser that decides
	// whether an edge participates — parity with the in-memory store.
	type key struct {
		kind graph.EdgeKind
		from string
		to   string
	}
	acc := make(map[key]int)
	for rows.Next() {
		var kind, from, to string
		var n int
		panicOnFatal(rows.Scan(&kind, &from, &to, &n))
		ek := graph.EdgeKind(kind)
		if _, ok := graph.BaseKindForCrossRepo(ek); !ok {
			continue
		}
		acc[key{kind: ek, from: from, to: to}] += n
	}
	panicOnFatal(rows.Err())
	if len(acc) == 0 {
		return nil
	}
	out := make([]graph.CrossRepoEdgeRow, 0, len(acc))
	for k, n := range acc {
		out = append(out, graph.CrossRepoEdgeRow{Kind: k.kind, FromRepo: k.from, ToRepo: k.to, Count: n})
	}
	return out
}

// FileImporters returns the importing-node rows for every EdgeImports
// edge whose target's FilePath OR ID equals filePath.
func (s *Store) FileImporters(filePath string) []graph.FileImporterRow {
	if filePath == "" {
		return nil
	}
	q := `SELECT nf.file_path, nf.id, nf.name, nf.kind
		FROM edges e
		JOIN nodes nt ON e.to_id = nt.id
		JOIN nodes nf ON e.from_id = nf.id
		WHERE e.kind = ? AND (nt.file_path = ? OR nt.id = ?)
		ORDER BY nf.file_path`
	rows, err := s.db.Query(q, string(graph.EdgeImports), filePath, filePath)
	panicOnFatal(err)
	defer rows.Close()
	var out []graph.FileImporterRow
	for rows.Next() {
		var r graph.FileImporterRow
		var kind string
		panicOnFatal(rows.Scan(&r.FromFile, &r.FromID, &r.FromName, &kind))
		r.FromKind = graph.NodeKind(kind)
		out = append(out, r)
	}
	panicOnFatal(rows.Err())
	return out
}

// FileSymbolNamesByPaths returns the distinct (file, name) pairs for
// nodes in the supplied paths whose kind is in the set, sorted by
// (file, name).
func (s *Store) FileSymbolNamesByPaths(paths []string, kinds []graph.NodeKind) []graph.FileSymbolNameRow {
	uniqPaths := dedupeNonEmpty(paths)
	_, kindArgs := aggDedupeNodeKinds(kinds)
	if len(uniqPaths) == 0 || len(kindArgs) == 0 {
		return nil
	}
	var out []graph.FileSymbolNameRow
	for i := 0; i < len(uniqPaths); i += lookupChunkSize {
		end := minInt(i+lookupChunkSize, len(uniqPaths))
		chunk := uniqPaths[i:end]
		args := append(toAnyArgs(chunk), kindArgs...)
		q := `SELECT DISTINCT file_path, name FROM nodes WHERE file_path IN (` +
			inPlaceholders(len(chunk)) + `) AND kind IN (` + inPlaceholders(len(kindArgs)) + `)`
		rows, err := s.db.Query(q, args...)
		panicOnFatal(err)
		for rows.Next() {
			var r graph.FileSymbolNameRow
			panicOnFatal(rows.Scan(&r.FilePath, &r.Name))
			out = append(out, r)
		}
		panicOnFatal(rows.Err())
		rows.Close()
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].FilePath != out[j].FilePath {
			return out[i].FilePath < out[j].FilePath
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// EdgesByKinds streams every edge whose kind is in the supplied set;
// honours early-stop. Empty kinds yields nothing.
func (s *Store) EdgesByKinds(kinds []graph.EdgeKind) iter.Seq[*graph.Edge] {
	_, args := aggDedupeEdgeKinds(kinds)
	return func(yield func(*graph.Edge) bool) {
		if len(args) == 0 {
			return
		}
		q := `SELECT ` + lookupEdgeCols + ` FROM edges WHERE kind IN (` +
			inPlaceholders(len(args)) + `) ORDER BY id`
		for _, e := range s.queryEdgesSQL(q, args...) {
			if e == nil {
				continue
			}
			if !yield(e) {
				return
			}
		}
	}
}

// NodesByKinds returns every node whose kind is in the supplied set.
func (s *Store) NodesByKinds(kinds []graph.NodeKind) []*graph.Node {
	_, args := aggDedupeNodeKinds(kinds)
	if len(args) == 0 {
		return nil
	}
	q := `SELECT ` + lookupNodeCols + ` FROM nodes WHERE kind IN (` +
		inPlaceholders(len(args)) + `) ORDER BY id`
	return s.queryNodesSQL(q, args...)
}

// EdgeAdjacencyForKinds streams (from, to) id pairs for edges whose
// kind is in edgeKinds and whose endpoints both have a kind in
// nodeKinds; honours early-stop. Empty kinds yields nothing.
func (s *Store) EdgeAdjacencyForKinds(edgeKinds []graph.EdgeKind, nodeKinds []graph.NodeKind) iter.Seq[[2]string] {
	_, eArgs := aggDedupeEdgeKinds(edgeKinds)
	_, nArgs := aggDedupeNodeKinds(nodeKinds)
	return func(yield func([2]string) bool) {
		if len(eArgs) == 0 || len(nArgs) == 0 {
			return
		}
		args := append([]any(nil), eArgs...)
		args = append(args, nArgs...)
		args = append(args, nArgs...)
		q := `SELECT e.from_id, e.to_id
			FROM edges e
			JOIN nodes nf ON e.from_id = nf.id
			JOIN nodes nt ON e.to_id = nt.id
			WHERE e.kind IN (` + inPlaceholders(len(eArgs)) + `)
			AND nf.kind IN (` + inPlaceholders(len(nArgs)) + `)
			AND nt.kind IN (` + inPlaceholders(len(nArgs)) + `)`
		rows, err := s.db.Query(q, args...)
		panicOnFatal(err)
		defer rows.Close()
		for rows.Next() {
			var from, to string
			panicOnFatal(rows.Scan(&from, &to))
			if !yield([2]string{from, to}) {
				return
			}
		}
		panicOnFatal(rows.Err())
	}
}

// NodeDegreeCounts returns per-node in/out/usage-in edge counts for the
// supplied id set. Unknown ids produce no row; duplicates collapse.
func (s *Store) NodeDegreeCounts(ids []string, usageKinds []graph.EdgeKind) []graph.NodeDegreeRow {
	uniq := dedupeNonEmpty(ids)
	if len(uniq) == 0 {
		return nil
	}
	_, usageArgs := aggDedupeEdgeKinds(usageKinds)
	out := make([]graph.NodeDegreeRow, 0, len(uniq))
	for i := 0; i < len(uniq); i += lookupChunkSize {
		end := minInt(i+lookupChunkSize, len(uniq))
		chunk := uniq[i:end]
		// Usage-in subquery: a literal 0 when no usage kinds are given.
		usageExpr := `0`
		var usageInline []any
		if len(usageArgs) > 0 {
			usageExpr = `(SELECT COUNT(*) FROM edges e WHERE e.to_id = n.id AND e.kind IN (` +
				inPlaceholders(len(usageArgs)) + `))`
			usageInline = usageArgs
		}
		q := `SELECT n.id,
			(SELECT COUNT(*) FROM edges e WHERE e.to_id = n.id) AS in_count,
			(SELECT COUNT(*) FROM edges e WHERE e.from_id = n.id) AS out_count,
			` + usageExpr + ` AS usage_in
		FROM nodes n
		WHERE n.id IN (` + inPlaceholders(len(chunk)) + `)`
		// Bind order matches placeholder order: usage subquery first
		// (it appears earlier in the SELECT list), then the id IN-list.
		args := append(append([]any(nil), usageInline...), toAnyArgs(chunk)...)
		rows, err := s.db.Query(q, args...)
		panicOnFatal(err)
		for rows.Next() {
			var r graph.NodeDegreeRow
			panicOnFatal(rows.Scan(&r.NodeID, &r.InCount, &r.OutCount, &r.UsageInCount))
			out = append(out, r)
		}
		panicOnFatal(rows.Err())
		rows.Close()
	}
	return out
}

// NodeFanCounts returns per-node fan-in (incoming edges in fanInKinds)
// and fan-out (outgoing edges in fanOutKinds) for the supplied id set.
// Unknown ids produce no row; duplicates collapse.
func (s *Store) NodeFanCounts(ids []string, fanInKinds, fanOutKinds []graph.EdgeKind) []graph.NodeFanRow {
	uniq := dedupeNonEmpty(ids)
	if len(uniq) == 0 {
		return nil
	}
	_, inArgs := aggDedupeEdgeKinds(fanInKinds)
	_, outArgs := aggDedupeEdgeKinds(fanOutKinds)
	out := make([]graph.NodeFanRow, 0, len(uniq))
	for i := 0; i < len(uniq); i += lookupChunkSize {
		end := minInt(i+lookupChunkSize, len(uniq))
		chunk := uniq[i:end]

		fanInExpr := `0`
		var inInline []any
		if len(inArgs) > 0 {
			fanInExpr = `(SELECT COUNT(*) FROM edges e WHERE e.to_id = n.id AND e.kind IN (` +
				inPlaceholders(len(inArgs)) + `))`
			inInline = inArgs
		}
		fanOutExpr := `0`
		var outInline []any
		if len(outArgs) > 0 {
			fanOutExpr = `(SELECT COUNT(*) FROM edges e WHERE e.from_id = n.id AND e.kind IN (` +
				inPlaceholders(len(outArgs)) + `))`
			outInline = outArgs
		}
		q := `SELECT n.id, ` + fanInExpr + ` AS fan_in, ` + fanOutExpr + ` AS fan_out
		FROM nodes n
		WHERE n.id IN (` + inPlaceholders(len(chunk)) + `)`
		// Bind order matches placeholder order in the SELECT list:
		// fan-in subquery, fan-out subquery, then the id IN-list.
		args := append([]any(nil), inInline...)
		args = append(args, outInline...)
		args = append(args, toAnyArgs(chunk)...)
		rows, err := s.db.Query(q, args...)
		panicOnFatal(err)
		for rows.Next() {
			var r graph.NodeFanRow
			panicOnFatal(rows.Scan(&r.NodeID, &r.FanIn, &r.FanOut))
			out = append(out, r)
		}
		panicOnFatal(rows.Err())
		rows.Close()
	}
	return out
}

// CommunityCrossingsByKind returns per-source crossing counts for edges
// whose kind is in the supplied set, given a node→community map. A
// crossing is an edge whose source community differs from its target
// community; zero-count sources are dropped. Empty kinds or empty
// community map returns nil. The community comparison runs Go-side
// because community membership is not a node column.
func (s *Store) CommunityCrossingsByKind(kinds []graph.EdgeKind, nodeToComm map[string]string) map[string]int {
	_, args := aggDedupeEdgeKinds(kinds)
	if len(args) == 0 || len(nodeToComm) == 0 {
		return nil
	}
	q := `SELECT from_id, to_id FROM edges WHERE kind IN (` + inPlaceholders(len(args)) + `)`
	rows, err := s.db.Query(q, args...)
	panicOnFatal(err)
	defer rows.Close()
	out := make(map[string]int)
	for rows.Next() {
		var from, to string
		panicOnFatal(rows.Scan(&from, &to))
		fromComm, ok := nodeToComm[from]
		if !ok {
			continue
		}
		toComm, ok := nodeToComm[to]
		if !ok {
			continue
		}
		if fromComm != toComm {
			out[from]++
		}
	}
	panicOnFatal(rows.Err())
	if len(out) == 0 {
		return nil
	}
	return out
}
