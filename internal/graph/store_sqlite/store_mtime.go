package store_sqlite

import (
	"github.com/zzet/gortex/internal/graph"
)

// Compile-time assertions that the SQLite Store satisfies the optional
// per-file mtime persistence capabilities. Lifting this state into the
// same backend the graph lives in means warm restarts read it through
// one persistence surface instead of a second gob snapshot.
var (
	_ graph.FileMtimeWriter = (*Store)(nil)
	_ graph.FileMtimeReader = (*Store)(nil)
)

// mtimeChunk bounds how many (repo_prefix, file_path, mtime_ns) tuples
// ride in a single multi-row INSERT. SQLite's default compiled-in host
// parameter limit is 999; at 3 params per row that caps a statement at
// 333 rows, so 300 leaves headroom.
const mtimeChunk = 300

// SetFileMtime records one file's modification time (nanoseconds since
// the epoch) for a repo prefix, replacing any prior value. It is a
// convenience single-row form of BulkSetFileMtimes.
func (s *Store) SetFileMtime(repoPrefix, filePath string, mtimeNs int64) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.db.Exec(
		`INSERT OR REPLACE INTO file_mtimes (repo_prefix, file_path, mtime_ns) VALUES (?, ?, ?)`,
		repoPrefix, filePath, mtimeNs,
	)
	return err
}

// BulkSetFileMtimes persists every (filePath -> mtimeNs) entry for one
// repo prefix in a single transaction, chunked so no statement exceeds
// SQLite's host-parameter limit. Idempotent on (repoPrefix, filePath):
// re-running with overlapping keys replaces in place. Empty input is a
// no-op.
func (s *Store) BulkSetFileMtimes(repoPrefix string, mtimes map[string]int64) error {
	if len(mtimes) == 0 {
		return nil
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	// Stable ordering is not required for correctness, but iterating the
	// map directly is fine — we only chunk by count.
	type kv struct {
		path string
		ns   int64
	}
	pending := make([]kv, 0, len(mtimes))
	for p, ns := range mtimes {
		pending = append(pending, kv{path: p, ns: ns})
	}

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // rollback after Commit is a no-op

	for start := 0; start < len(pending); start += mtimeChunk {
		end := start + mtimeChunk
		if end > len(pending) {
			end = len(pending)
		}
		batch := pending[start:end]

		// Build a multi-row INSERT OR REPLACE: (?, ?, ?), (?, ?, ?), ...
		args := make([]any, 0, len(batch)*3)
		stmt := make([]byte, 0, 64+len(batch)*16)
		stmt = append(stmt, "INSERT OR REPLACE INTO file_mtimes (repo_prefix, file_path, mtime_ns) VALUES "...)
		for i, e := range batch {
			if i > 0 {
				stmt = append(stmt, ',')
			}
			stmt = append(stmt, "(?, ?, ?)"...)
			args = append(args, repoPrefix, e.path, e.ns)
		}
		if _, err := tx.Exec(string(stmt), args...); err != nil {
			return err
		}
	}

	return tx.Commit()
}

// LoadFileMtimes returns the recorded mtimes for one repo prefix as a
// fresh map. Returns nil when there is no data for the prefix (the
// "no recorded state" signal warmup expects).
func (s *Store) LoadFileMtimes(repoPrefix string) map[string]int64 {
	rows, err := s.db.Query(
		`SELECT file_path, mtime_ns FROM file_mtimes WHERE repo_prefix = ?`,
		repoPrefix,
	)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var out map[string]int64
	for rows.Next() {
		var path string
		var ns int64
		if err := rows.Scan(&path, &ns); err != nil {
			return nil
		}
		if out == nil {
			out = make(map[string]int64)
		}
		out[path] = ns
	}
	if err := rows.Err(); err != nil {
		return nil
	}
	return out
}

// FileMtimes is a fallible read form of LoadFileMtimes. It always
// returns a non-nil (possibly empty) map for a known/unknown prefix and
// surfaces any query error. The interface method LoadFileMtimes is the
// daemon's entry point; this variant exists for callers (and tests)
// that want the error and an always-materialised map.
func (s *Store) FileMtimes(repoPrefix string) (map[string]int64, error) {
	rows, err := s.db.Query(
		`SELECT file_path, mtime_ns FROM file_mtimes WHERE repo_prefix = ?`,
		repoPrefix,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]int64)
	for rows.Next() {
		var path string
		var ns int64
		if err := rows.Scan(&path, &ns); err != nil {
			return nil, err
		}
		out[path] = ns
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
