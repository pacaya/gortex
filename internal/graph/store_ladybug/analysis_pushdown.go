package store_ladybug

import (
	"github.com/zzet/gortex/internal/graph"
)

// Compile-time assertions: *Store satisfies the new pushdown
// capabilities for the performance-wave handlers. A drift in any
// signature fails the build here instead of silently dropping to the
// Go-loop fallback.
var (
	_ graph.InDegreeForNodes        = (*Store)(nil)
	_ graph.ReachableForwardByKinds = (*Store)(nil)
	_ graph.ThrowerErrorSurfacer    = (*Store)(nil)
)

// InDegreeForNodes runs the per-target incoming-edge count entirely
// inside Ladybug. Replaces the AllEdges() + Go-side bucket pass the
// surprising-connections handler used to feed its hub heuristic — on
// the gortex workspace that materialised ~286k edges over cgo just
// to count fan-in for a few thousand scoped nodes.
//
// COUNT { … } sub-query returns the bucket size without materialising
// the edges. The IN-list constrains the rows to the caller's scoped
// id set so the planner can index-walk the in-edge adjacency.
func (s *Store) InDegreeForNodes(ids []string) map[string]int {
	if len(ids) == 0 {
		return nil
	}
	uniq := dedupeNonEmpty(ids)
	if len(uniq) == 0 {
		return nil
	}
	const q = `
MATCH (n:Node)
WHERE n.id IN $ids
RETURN n.id, COUNT { MATCH (:Node)-[:Edge]->(n) }`
	rows := s.querySelect(q, map[string]any{"ids": stringSliceToAny(uniq)})
	if len(rows) == 0 {
		return nil
	}
	out := make(map[string]int, len(rows))
	for _, r := range rows {
		if len(r) < 2 {
			continue
		}
		id, _ := r[0].(string)
		if id == "" {
			continue
		}
		c := int(asInt64(r[1]))
		if c == 0 {
			continue
		}
		out[id] = c
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// ReachableForwardByKinds runs the layer-by-layer forward BFS inside
// Ladybug. The Go fallback walks GetOutEdges per frontier id — on a
// repo with thousands of seeds the loop fires tens of thousands of
// cgo round-trips. Each layer here is one Cypher query that returns
// every distinct To-node reachable from the current frontier through
// the allowed edge kinds; the loop terminates when no new ids
// surface.
//
// Layer-driven instead of one giant recursive var-length match: the
// closure size matters more than the number of round-trips, and
// Kuzu's planner picks better index-walks against a small frontier
// IN-list than against an unbounded `*1..N` pattern with a kind
// filter in the relationship body.
func (s *Store) ReachableForwardByKinds(seeds []string, kinds []graph.EdgeKind) map[string]bool {
	if len(seeds) == 0 {
		return nil
	}
	covered := make(map[string]bool, len(seeds))
	frontier := make([]string, 0, len(seeds))
	for _, id := range seeds {
		if id == "" || covered[id] {
			continue
		}
		covered[id] = true
		frontier = append(frontier, id)
	}
	if len(kinds) == 0 || len(frontier) == 0 {
		return covered
	}
	kindArgs := edgeKindSliceToAny(dedupeEdgeKinds(kinds))
	if len(kindArgs) == 0 {
		return covered
	}
	const q = `
MATCH (src:Node)-[e:Edge]->(dst:Node)
WHERE src.id IN $frontier
  AND e.kind IN $kinds
RETURN DISTINCT dst.id`
	for len(frontier) > 0 {
		rows := s.querySelect(q, map[string]any{
			"frontier": stringSliceToAny(frontier),
			"kinds":    kindArgs,
		})
		next := frontier[:0:0]
		for _, r := range rows {
			if len(r) < 1 {
				continue
			}
			id, _ := r[0].(string)
			if id == "" || covered[id] {
				continue
			}
			covered[id] = true
			next = append(next, id)
		}
		frontier = next
	}
	return covered
}

// throwerAgg is the intermediate per-thrower aggregator used while
// stitching the two ThrowerErrorSurface passes together.
type throwerAgg struct {
	throws   int
	targets  []string
	emitMsgs []string
	file     string
	line     int
}

// ThrowerErrorSurface runs the analyze(error_surface) rollup as two
// Cypher GROUP BYs inside Ladybug. Replaces the legacy walk that
// scanned EdgeThrows then issued GetOutEdges per thrower for the
// EdgeEmits → KindString attachment — on the gortex workspace that
// loop materialised the throws bucket plus ~thousands of per-thrower
// cgo round-trips just to land at a few dozen aggregated rows.
//
// The pathPrefix filter is evaluated with Kuzu's starts_with on the
// EdgeThrows e.file_path column. An empty prefix is dropped from the
// WHERE clause so the planner picks the kind-only index walk.
func (s *Store) ThrowerErrorSurface(pathPrefix string) []graph.ThrowerErrorRow {
	args := map[string]any{"throws": string(graph.EdgeThrows)}
	pass1 := `
MATCH (from:Node)-[e:Edge]->(to:Node)
WHERE e.kind = $throws`
	if pathPrefix != "" {
		pass1 += "\n  AND starts_with(e.file_path, $prefix)"
		args["prefix"] = pathPrefix
	}
	pass1 += `
RETURN from.id, to.id, count(*), min(e.file_path), min(e.line)`

	rows := s.querySelect(pass1, args)
	if len(rows) == 0 {
		return nil
	}

	byThrower := map[string]*throwerAgg{}
	addUnique := func(set []string, v string) []string {
		for _, s := range set {
			if s == v {
				return set
			}
		}
		return append(set, v)
	}
	for _, r := range rows {
		if len(r) < 5 {
			continue
		}
		from, _ := r[0].(string)
		to, _ := r[1].(string)
		if from == "" || to == "" {
			continue
		}
		count := int(asInt64(r[2]))
		file, _ := r[3].(string)
		line := int(asInt64(r[4]))
		agg, ok := byThrower[from]
		if !ok {
			agg = &throwerAgg{file: file, line: line}
			byThrower[from] = agg
		}
		agg.throws += count
		agg.targets = addUnique(agg.targets, to)
		if agg.file == "" && file != "" {
			agg.file = file
		}
		if agg.line == 0 && line != 0 {
			agg.line = line
		}
	}
	if len(byThrower) == 0 {
		return nil
	}

	// Backfill missing file / line from the thrower node row itself
	// when the edge metadata didn't carry them.
	missingMeta := make([]string, 0)
	for id, r := range byThrower {
		if r.file == "" || r.line == 0 {
			missingMeta = append(missingMeta, id)
		}
	}
	if len(missingMeta) > 0 {
		const probe = `MATCH (n:Node) WHERE n.id IN $ids RETURN n.id, n.file_path, n.start_line`
		mrows := s.querySelect(probe, map[string]any{"ids": stringSliceToAny(missingMeta)})
		for _, r := range mrows {
			if len(r) < 3 {
				continue
			}
			id, _ := r[0].(string)
			file, _ := r[1].(string)
			line := int(asInt64(r[2]))
			agg, ok := byThrower[id]
			if !ok {
				continue
			}
			if agg.file == "" {
				agg.file = file
			}
			if agg.line == 0 {
				agg.line = line
			}
		}
	}

	// Pass 2: per-(thrower, error_msg) emit join. Pulls every
	// EdgeEmits→KindString edge whose source is a known thrower, then
	// filters on meta.context = error_msg Go-side (the meta column is
	// the encoded blob — same shape IfaceImplementsScanner consumes).
	throwerIDs := make([]string, 0, len(byThrower))
	for id := range byThrower {
		throwerIDs = append(throwerIDs, id)
	}
	const emitQ = `
MATCH (from:Node)-[e:Edge]->(to:Node)
WHERE e.kind = $emits
  AND from.id IN $throwers
  AND to.kind = $strKind
RETURN from.id, to.name, to.meta`
	emitRows := s.querySelect(emitQ, map[string]any{
		"emits":    string(graph.EdgeEmits),
		"throwers": stringSliceToAny(throwerIDs),
		"strKind":  string(graph.KindString),
	})
	for _, r := range emitRows {
		if len(r) < 3 {
			continue
		}
		from, _ := r[0].(string)
		name, _ := r[1].(string)
		metaStr, _ := r[2].(string)
		if from == "" || name == "" || metaStr == "" {
			continue
		}
		agg, ok := byThrower[from]
		if !ok {
			continue
		}
		m, err := decodeMeta(metaStr)
		if err != nil || m == nil {
			continue
		}
		ctxLabel, _ := m["context"].(string)
		if ctxLabel != "error_msg" {
			continue
		}
		agg.emitMsgs = addUnique(agg.emitMsgs, name)
	}

	out := make([]graph.ThrowerErrorRow, 0, len(byThrower))
	for id, r := range byThrower {
		out = append(out, graph.ThrowerErrorRow{
			ThrowerID:    id,
			FilePath:     r.file,
			Line:         r.line,
			Throws:       r.throws,
			ErrorTargets: append([]string(nil), r.targets...),
			ErrorMsgs:    append([]string(nil), r.emitMsgs...),
		})
	}
	return out
}
