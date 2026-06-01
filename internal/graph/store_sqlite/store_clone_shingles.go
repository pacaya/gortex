package store_sqlite

import (
	"encoding/binary"

	"github.com/zzet/gortex/internal/graph"
)

// Compile-time assertions that the SQLite Store satisfies the optional
// per-symbol clone-shingle persistence capabilities. Lifting this state
// into the same backend the graph lives in means warm restarts rebuild
// the clone-detection CMS through one persistence surface instead of a
// second gob snapshot.
var (
	_ graph.CloneShingleWriter = (*Store)(nil)
	_ graph.CloneShingleReader = (*Store)(nil)
)

// shingleChunk bounds how many (node_id, repo_prefix, shingles) tuples
// ride in a single multi-row INSERT. SQLite's default compiled-in host
// parameter limit is 999; at 3 params per row that caps a statement at
// 333 rows, so 300 leaves headroom. Mirrors mtimeChunk.
const shingleChunk = 300

// encodeShingles serialises a uint64 slice to a little-endian BLOB
// (8 bytes per element). A nil/empty slice encodes to an empty BLOB.
func encodeShingles(shingles []uint64) []byte {
	b := make([]byte, len(shingles)*8)
	for i, s := range shingles {
		binary.LittleEndian.PutUint64(b[i*8:], s)
	}
	return b
}

// decodeShingles is the inverse of encodeShingles. A BLOB whose length
// is not a multiple of 8 yields nil (corrupt row); callers skip nil
// sets. An empty BLOB decodes to an empty (non-nil) slice.
func decodeShingles(b []byte) []uint64 {
	if len(b)%8 != 0 {
		return nil
	}
	out := make([]uint64, len(b)/8)
	for i := range out {
		out[i] = binary.LittleEndian.Uint64(b[i*8:])
	}
	return out
}

// BulkSetCloneShingles persists every (nodeID -> shingles) entry for
// one repo prefix in a single transaction, chunked so no statement
// exceeds SQLite's host-parameter limit. Idempotent on node_id:
// re-running with overlapping keys replaces in place. Empty input is a
// no-op.
func (s *Store) BulkSetCloneShingles(repoPrefix string, rows map[string][]uint64) error {
	if len(rows) == 0 {
		return nil
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	// Stable ordering is not required for correctness, but iterating the
	// map directly is fine — we only chunk by count.
	type kv struct {
		id   string
		blob []byte
	}
	pending := make([]kv, 0, len(rows))
	for id, sh := range rows {
		pending = append(pending, kv{id: id, blob: encodeShingles(sh)})
	}

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // rollback after Commit is a no-op

	for start := 0; start < len(pending); start += shingleChunk {
		end := start + shingleChunk
		if end > len(pending) {
			end = len(pending)
		}
		batch := pending[start:end]

		// Build a multi-row INSERT OR REPLACE: (?, ?, ?), (?, ?, ?), ...
		args := make([]any, 0, len(batch)*3)
		stmt := make([]byte, 0, 64+len(batch)*16)
		stmt = append(stmt, "INSERT OR REPLACE INTO clone_shingles (node_id, repo_prefix, shingles) VALUES "...)
		for i, e := range batch {
			if i > 0 {
				stmt = append(stmt, ',')
			}
			stmt = append(stmt, "(?, ?, ?)"...)
			args = append(args, e.id, repoPrefix, e.blob)
		}
		if _, err := tx.Exec(string(stmt), args...); err != nil {
			return err
		}
	}

	return tx.Commit()
}

// DeleteCloneShingles drops the rows for the supplied node ids, chunked
// into `node_id IN (?, ?, …)` DELETEs so no statement exceeds SQLite's
// host-parameter limit. Empty input is a no-op; missing ids are simply
// not deleted.
func (s *Store) DeleteCloneShingles(nodeIDs []string) error {
	if len(nodeIDs) == 0 {
		return nil
	}

	// Dedupe + skip empty up front to keep the chunk loop honest.
	seen := make(map[string]struct{}, len(nodeIDs))
	uniq := make([]string, 0, len(nodeIDs))
	for _, id := range nodeIDs {
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		uniq = append(uniq, id)
	}
	if len(uniq) == 0 {
		return nil
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // rollback after Commit is a no-op

	for start := 0; start < len(uniq); start += shingleChunk {
		end := start + shingleChunk
		if end > len(uniq) {
			end = len(uniq)
		}
		chunk := uniq[start:end]
		args := make([]any, len(chunk))
		stmt := make([]byte, 0, 48+len(chunk)*2)
		stmt = append(stmt, "DELETE FROM clone_shingles WHERE node_id IN ("...)
		for i, id := range chunk {
			if i > 0 {
				stmt = append(stmt, ',')
			}
			stmt = append(stmt, '?')
			args[i] = id
		}
		stmt = append(stmt, ')')
		if _, err := tx.Exec(string(stmt), args...); err != nil {
			return err
		}
	}

	return tx.Commit()
}

// LoadCloneShingles returns the recorded shingle sets for one repo
// prefix as a fresh map. It always returns a non-nil (possibly empty)
// map and surfaces any query error. An empty/absent prefix yields an
// empty map, not an error.
func (s *Store) LoadCloneShingles(repoPrefix string) (map[string][]uint64, error) {
	rows, err := s.db.Query(
		`SELECT node_id, shingles FROM clone_shingles WHERE repo_prefix = ?`,
		repoPrefix,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := make(map[string][]uint64)
	for rows.Next() {
		var id string
		var blob []byte
		if err := rows.Scan(&id, &blob); err != nil {
			return nil, err
		}
		out[id] = decodeShingles(blob)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
