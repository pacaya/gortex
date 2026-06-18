package persistence

import (
	"database/sql"
	"fmt"
	"strings"
)

// Sidecar schema migrations.
//
// The sidecar DB has no migration framework historically: sidecarSchema was
// run with CREATE TABLE / CREATE INDEX IF NOT EXISTS, and columns added after
// a table's original shape were retrofitted with best-effort ALTERs. That
// breaks when an index (or any statement) inside sidecarSchema references a
// column the IF-NOT-EXISTS create never adds to a pre-existing table — the
// whole schema batch aborts with "no such column" and the install can no
// longer open its ledger.
//
// runMigrations replaces that with a forward-only, version-stamped sequence
// keyed on SQLite's built-in PRAGMA user_version. sidecarSchema keeps only the
// idempotent base shape (tables + original-shape indexes); every later column
// and every column-dependent index lives in a migration that runs after the
// column's ALTER. Existing installs upgrade in place on the next OpenSidecar —
// no user action, no data loss (migrations are additive only).
//
// Concurrency: applyOne relies on the sidecar DSN's _txlock=immediate so
// db.Begin() takes the write lock at BEGIN, making the in-transaction
// user_version check authoritative. Two processes opening a stale DB at once
// serialise on busy_timeout; the loser re-reads the bumped version and skips.

// migration is one forward step. Steps are append-only and ascending: never
// edit or renumber a migration that has shipped — add a new higher version.
type migration struct {
	version int
	name    string
	fn      func(tx *sql.Tx) error
}

// currentSidecarVersion is the schema version a fully-migrated DB reports via
// PRAGMA user_version. It must equal the highest version in sidecarMigrations.
const currentSidecarVersion = 1

// sidecarMigrations is the ordered, forward-only migration list.
var sidecarMigrations = []migration{
	{version: 1, name: "baseline-reconcile", fn: migrateV1Baseline},
}

// runMigrations applies every pending migration in order. Each runs in its own
// IMMEDIATE-locked transaction and bumps user_version on success, so a failure
// at version N leaves versions < N committed and stamped and is safe to retry
// on the next open.
func runMigrations(db *sql.DB) error {
	for _, m := range sidecarMigrations {
		if err := applyOne(db, m); err != nil {
			return fmt.Errorf("persistence: sidecar migration v%d (%s): %w", m.version, m.name, err)
		}
	}
	return nil
}

// applyOne applies a single migration if the DB's user_version is below it.
//
// The sidecar DSN sets _txlock=immediate, so db.Begin() emits BEGIN IMMEDIATE
// and the reserved write lock is held before the user_version read below —
// the gate is therefore authoritative across processes. A concurrent opener
// blocks on busy_timeout at BEGIN, then (this winner having committed) reads
// the bumped version and returns without writing. PRAGMA user_version is
// transactional, so on any failure the deferred Rollback reverts both the
// partial DDL and the version bump.
func applyOne(db *sql.DB, m migration) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op once Commit succeeds

	var cur int
	if err := tx.QueryRow("PRAGMA user_version").Scan(&cur); err != nil {
		return fmt.Errorf("read user_version: %w", err)
	}
	if cur >= m.version {
		return nil // already applied (this or another process)
	}

	if err := m.fn(tx); err != nil {
		return err
	}

	// PRAGMA takes no bound parameters; m.version is an int constant we own.
	// Bumped last so it rolls back with the transaction on any earlier error.
	if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", m.version)); err != nil {
		return fmt.Errorf("bump user_version: %w", err)
	}
	return tx.Commit()
}

// migrateV1Baseline reconciles any historical sidecar shape (which all report
// user_version 0) to the current schema. It is purely additive and idempotent:
// CREATE TABLE IF NOT EXISTS for every table runs earlier in sidecarSchema, so
// this only adds the columns and column-dependent indexes that a pre-existing
// table cannot gain from CREATE TABLE.
//
// savings_events.model / .client are the only column-level drift in the whole
// sidecar history; every other shape difference is a whole-table addition
// already covered by the base schema.
func migrateV1Baseline(tx *sql.Tx) error {
	if err := addColumnIfMissingTx(tx, "savings_events", "model", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := addColumnIfMissingTx(tx, "savings_events", "client", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	// Column-dependent indexes LAST — only after the columns are guaranteed to
	// exist. These are exactly the statements that aborted sidecarSchema on a
	// database created before model/client existed.
	if _, err := tx.Exec(`CREATE INDEX IF NOT EXISTS idx_savings_events_model ON savings_events (model, ts)`); err != nil {
		return fmt.Errorf("index idx_savings_events_model: %w", err)
	}
	if _, err := tx.Exec(`CREATE INDEX IF NOT EXISTS idx_savings_events_client ON savings_events (client, ts)`); err != nil {
		return fmt.Errorf("index idx_savings_events_client: %w", err)
	}
	return nil
}

// addColumnIfMissingTx runs ALTER TABLE ... ADD COLUMN inside a transaction,
// swallowing ONLY the "duplicate column name" error that a table which already
// has the column returns (idempotency for fresh / already-upgraded databases).
// Every other error propagates — surfacing a genuine failure with its real
// cause instead of letting a later column-dependent statement fail with a
// misleading "no such column".
func addColumnIfMissingTx(tx *sql.Tx, table, column, decl string) error {
	if _, err := tx.Exec("ALTER TABLE " + table + " ADD COLUMN " + column + " " + decl); err != nil {
		if strings.Contains(err.Error(), "duplicate column name") {
			return nil
		}
		return fmt.Errorf("add column %s.%s: %w", table, column, err)
	}
	return nil
}
