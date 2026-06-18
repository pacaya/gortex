package store_sqlite

import (
	"database/sql"
	"fmt"
	"os"
	"strings"
)

// Schema versioning for the graph store.
//
// Unlike the sidecar (which holds irreplaceable user data and must migrate in
// place), the graph store is a DERIVED CACHE: every row is reconstructable by
// re-indexing the source. So the cheapest *always-correct* reaction to a schema
// change an old on-disk DB can't satisfy is to drop the file and let the daemon
// rebuild it on the next index. A migration may therefore declare rebuild=true
// instead of writing an in-place transform that would have to re-derive the new
// data from source anyway. In-place steps remain the cheap path for purely
// mechanical changes (a new index, a denormalisation, a column with a
// computable default) that spare a large repo a multi-minute reindex.
//
// The whole mechanism keys off SQLite's built-in PRAGMA user_version, read on
// Open before schemaSQL runs. There is no separate version table.
//
// Concurrency: the daemon holds an exclusive flock on <store>.lock around Open
// (see serverstack.NewSharedServer), so reading the version, wiping the file,
// and stamping it cannot race another process. That is why — unlike the
// sidecar — this path needs no BEGIN IMMEDIATE / busy-loop handling.

// currentSchemaVersion is the version a fully-reconciled store reports via
// PRAGMA user_version. Bump it whenever schemaSQL's typed-column shape or an
// index changes in a way an old on-disk DB would not already have, and append a
// matching schemaMigrations entry describing how to bring an older store
// forward (in place, or by rebuild).
const currentSchemaVersion = 1

// schemaMigration is one forward step. Exactly one strategy applies:
//   - rebuild=true: the change introduces structure/data that can only come
//     from re-indexing the source; an older store is wiped and rebuilt.
//   - inPlace!=nil: the change is mechanically derivable from the existing
//     store and is applied in a transaction with no reindex.
//
// Steps are append-only and ascending; never edit or renumber a shipped one.
// Any inPlace step must be idempotent (IF NOT EXISTS / ADD COLUMN guarded).
type schemaMigration struct {
	version int
	name    string
	inPlace func(tx *sql.Tx) error
	rebuild bool
}

// schemaMigrations is the ordered, forward-only registry. It is intentionally
// empty at version 1: a v1 store is reconciled entirely by schemaSQL's
// idempotent CREATE ... IF NOT EXISTS plus ensureNodeColumns, so any
// pre-versioning database baseline-stamps to v1 without a rebuild. Append
// entries for version 2 and up as the schema evolves.
var schemaMigrations []schemaMigration

// schemaPlan is the decision planSchemaMigration derives from the stored
// PRAGMA user_version. It mutates nothing on its own.
type schemaPlan struct {
	wipe    bool              // drop the on-disk DB and rebuild from source
	inPlace []schemaMigration // ordered in-place steps to run after schemaSQL
	stamp   bool              // write currentSchemaVersion once reconciled
}

// planSchemaMigrationWith decides how to reconcile a store at the stored
// PRAGMA user_version to current, given the migration registry. It mutates
// nothing. Open passes (currentSchemaVersion, schemaMigrations); tests pass
// fixtures.
func planSchemaMigrationWith(stored, current int, migrations []schemaMigration) schemaPlan {
	switch {
	case stored == current:
		return schemaPlan{} // up to date, nothing to do
	case stored > current:
		// Written by a newer build than this binary understands; the shape may
		// have changed under us. For a cache the safe move is to rebuild.
		return schemaPlan{wipe: true, stamp: true}
	case stored == 0:
		// Fresh DB, or a pre-versioning store of unknown shape. When nothing is
		// registered above the baseline, schemaSQL + ensureNodeColumns reconcile
		// it to the current shape, so stamp it. Once any later migration exists,
		// a store that skipped the versioning-introduction release could be
		// missing migration-introduced structure, so rebuild instead.
		if len(pendingBetween(0, current, migrations)) == 0 {
			return schemaPlan{stamp: true}
		}
		return schemaPlan{wipe: true, stamp: true}
	default: // 0 < stored < current: a known prior version
		pending := pendingBetween(stored, current, migrations)
		if anyRebuild(pending) {
			return schemaPlan{wipe: true, stamp: true}
		}
		return schemaPlan{inPlace: pending, stamp: true}
	}
}

func pendingBetween(stored, current int, migrations []schemaMigration) []schemaMigration {
	var out []schemaMigration
	for _, m := range migrations {
		if m.version > stored && m.version <= current {
			out = append(out, m)
		}
	}
	return out
}

func anyRebuild(ms []schemaMigration) bool {
	for _, m := range ms {
		if m.rebuild {
			return true
		}
	}
	return false
}

// validateSchemaMigrations checks the registry is well-formed. A test asserts
// this against the shipped (currentSchemaVersion, schemaMigrations) so the
// dangerous mistake — bumping currentSchemaVersion without appending a matching
// entry — fails CI instead of silently baseline-stamping an un-migrated store
// to the new version at runtime. Rules:
//   - versions are >= 2 (v1 is the implicit baseline, never an entry) and
//     strictly ascending;
//   - each step sets exactly one strategy (inPlace xor rebuild);
//   - the highest version equals current, so the registry actually defines how
//     to reach it. An empty registry is valid only at version 1.
func validateSchemaMigrations(current int, migs []schemaMigration) error {
	if len(migs) == 0 {
		if current != 1 {
			return fmt.Errorf("schema version %d has no migrations: only v1 may have an empty registry", current)
		}
		return nil
	}
	prev := 0
	for i, m := range migs {
		if m.version < 2 {
			return fmt.Errorf("migration %q has version %d: entries must be >= 2 (v1 is the implicit baseline)", m.name, m.version)
		}
		if i > 0 && m.version <= prev {
			return fmt.Errorf("migrations must be strictly ascending: v%d (%s) does not follow v%d", m.version, m.name, prev)
		}
		if (m.inPlace != nil) == m.rebuild {
			return fmt.Errorf("migration v%d (%s) must set exactly one of inPlace / rebuild", m.version, m.name)
		}
		prev = m.version
	}
	if prev != current {
		return fmt.Errorf("highest migration version %d != currentSchemaVersion %d: a version bump needs a matching migration entry", prev, current)
	}
	return nil
}

// readUserVersion reads PRAGMA user_version (0 on a fresh database).
func readUserVersion(db *sql.DB) (int, error) {
	var v int
	if err := db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		return 0, err
	}
	return v, nil
}

// setUserVersion stamps the schema version. PRAGMA takes no bound parameters;
// v is an int we control, so the format is safe.
func setUserVersion(db *sql.DB, v int) error {
	if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version = %d", v)); err != nil {
		return err
	}
	return nil
}

// applyInPlaceMigrations runs the in-place steps in a single transaction.
func applyInPlaceMigrations(db *sql.DB, steps []schemaMigration) error {
	if len(steps) == 0 {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }() // no-op once Commit succeeds
	for _, m := range steps {
		if err := m.inPlace(tx); err != nil {
			return fmt.Errorf("schema migration v%d (%s): %w", m.version, m.name, err)
		}
	}
	return tx.Commit()
}

// removeStoreFiles deletes the SQLite database and its companions. A missing
// file is not an error. Never called for ":memory:".
//
// The suffix list covers the files the DSN's journal_mode(WAL) produces (-wal,
// -shm) plus the rollback -journal a non-WAL fallback would use; keep it in
// sync if the journal_mode in Open's DSN ever changes.
func removeStoreFiles(path string) error {
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		if err := os.Remove(path + suffix); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove %s: %w", path+suffix, err)
		}
	}
	return nil
}

// isMemoryPath reports whether path is an in-process SQLite database (no file
// on disk to wipe, always built fresh by schemaSQL).
func isMemoryPath(path string) bool {
	return strings.Contains(path, ":memory:")
}
