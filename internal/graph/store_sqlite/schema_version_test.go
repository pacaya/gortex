package store_sqlite

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// withRawDB opens a bare *sql.DB on path, runs fn, and closes it — used to
// simulate an on-disk store written by an older/newer build (set user_version,
// insert rows) without going through Open's reconciliation.
func withRawDB(t *testing.T, path string, fn func(db *sql.DB)) {
	t.Helper()
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	defer func() { _ = db.Close() }()
	fn(db)
}

func nodeCount(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM nodes").Scan(&n); err != nil {
		t.Fatalf("count nodes: %v", err)
	}
	return n
}

// TestOpenStampsFreshDB: a brand-new on-disk store is stamped to the current
// schema version.
func TestOpenStampsFreshDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open fresh: %v", err)
	}
	defer func() { _ = s.Close() }()
	if v, err := readUserVersion(s.db); err != nil || v != currentSchemaVersion {
		t.Fatalf("fresh user_version = %d (err %v), want %d", v, err, currentSchemaVersion)
	}
}

// TestOpenBaselineStampsOldDBWithoutWipe: a pre-versioning store (user_version
// 0, reconcilable to current by schemaSQL + ensureNodeColumns) is stamped in
// place — its data must survive, not be wiped.
func TestOpenBaselineStampsOldDBWithoutWipe(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.sqlite")

	// Create the store, then simulate a pre-versioning DB: a row + user_version 0.
	s, err := Open(path)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	withRawDB(t, path, func(db *sql.DB) {
		if _, err := db.Exec(`INSERT INTO nodes (id, kind, name, file_path) VALUES ('n1','func','Foo','f.go')`); err != nil {
			t.Fatalf("seed node: %v", err)
		}
		if _, err := db.Exec(`PRAGMA user_version = 0`); err != nil {
			t.Fatalf("reset user_version: %v", err)
		}
	})

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen old DB: %v", err)
	}
	defer func() { _ = s2.Close() }()
	if v, _ := readUserVersion(s2.db); v != currentSchemaVersion {
		t.Fatalf("user_version after baseline = %d, want %d", v, currentSchemaVersion)
	}
	if n := nodeCount(t, s2.db); n != 1 {
		t.Fatalf("node count after baseline = %d, want 1 (data must NOT be wiped)", n)
	}
}

// TestOpenRebuildsNewerDB: a store written by a NEWER build (user_version above
// current) cannot be trusted, so Open drops and rebuilds it — the data is gone
// and the version is re-stamped to current. Proves the wipe path (and that the
// -wal/-shm companions are cleared along with the main file).
func TestOpenRebuildsNewerDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.sqlite")

	s, err := Open(path)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	withRawDB(t, path, func(db *sql.DB) {
		if _, err := db.Exec(`INSERT INTO nodes (id, kind, name, file_path) VALUES ('n1','func','Foo','f.go')`); err != nil {
			t.Fatalf("seed node: %v", err)
		}
		if _, err := db.Exec(`PRAGMA user_version = 999`); err != nil { // a future version this binary doesn't know
			t.Fatalf("set future user_version: %v", err)
		}
	})

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen newer DB: %v", err)
	}
	defer func() { _ = s2.Close() }()
	if v, _ := readUserVersion(s2.db); v != currentSchemaVersion {
		t.Fatalf("user_version after rebuild = %d, want %d", v, currentSchemaVersion)
	}
	if n := nodeCount(t, s2.db); n != 0 {
		t.Fatalf("node count after rebuild = %d, want 0 (newer DB must be wiped)", n)
	}
}

// TestPlanSchemaMigration covers the pure decision logic, including the
// in-place vs rebuild dispatch a future currentSchemaVersion=2 would exercise.
func TestPlanSchemaMigration(t *testing.T) {
	inPlace := schemaMigration{version: 2, name: "add-index", inPlace: func(*sql.Tx) error { return nil }}
	rebuild := schemaMigration{version: 2, name: "typed-column", rebuild: true}

	cases := []struct {
		name            string
		stored, current int
		migs            []schemaMigration
		wantWipe        bool
		wantStamp       bool
		wantInPlace     int
	}{
		{"up to date", 1, 1, nil, false, false, 0},
		{"fresh at v1 baseline-stamps", 0, 1, nil, false, true, 0},
		{"newer DB rebuilds", 2, 1, nil, true, true, 0},
		{"v0 skipping a later migration rebuilds", 0, 2, []schemaMigration{inPlace}, true, true, 0},
		{"v1->v2 in-place", 1, 2, []schemaMigration{inPlace}, false, true, 1},
		{"v1->v2 rebuild", 1, 2, []schemaMigration{rebuild}, true, true, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := planSchemaMigrationWith(c.stored, c.current, c.migs)
			if got.wipe != c.wantWipe || got.stamp != c.wantStamp || len(got.inPlace) != c.wantInPlace {
				t.Fatalf("plan(%d->%d) = {wipe:%v stamp:%v inPlace:%d}, want {wipe:%v stamp:%v inPlace:%d}",
					c.stored, c.current, got.wipe, got.stamp, len(got.inPlace), c.wantWipe, c.wantStamp, c.wantInPlace)
			}
		})
	}
}

// TestApplyInPlaceMigrations: steps run in order and commit; a failing step
// rolls the whole transaction back.
func TestApplyInPlaceMigrations(t *testing.T) {
	t.Run("commit", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "m.sqlite")
		withRawDB(t, path, func(db *sql.DB) {
			step := schemaMigration{version: 2, name: "mk", inPlace: func(tx *sql.Tx) error {
				_, err := tx.Exec(`CREATE TABLE marker (x TEXT)`)
				return err
			}}
			if err := applyInPlaceMigrations(db, []schemaMigration{step}); err != nil {
				t.Fatalf("apply: %v", err)
			}
			var name string
			if err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name='marker'`).Scan(&name); err != nil {
				t.Fatalf("marker table not created: %v", err)
			}
		})
	})

	t.Run("rollback on failure preserves cause and rolls back every step", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "m.sqlite")
		withRawDB(t, path, func(db *sql.DB) {
			// Two steps in one batch: the first creates table A, the second
			// creates B then fails. Both must roll back, proving the steps
			// share a single transaction.
			stepA := schemaMigration{version: 2, name: "make-a", inPlace: func(tx *sql.Tx) error {
				_, err := tx.Exec(`CREATE TABLE a (x TEXT)`)
				return err
			}}
			stepB := schemaMigration{version: 3, name: "boom", inPlace: func(tx *sql.Tx) error {
				if _, err := tx.Exec(`CREATE TABLE b (x TEXT)`); err != nil {
					return err
				}
				return sql.ErrConnDone // synthetic failure after a partial write
			}}
			err := applyInPlaceMigrations(db, []schemaMigration{stepA, stepB})
			if err == nil {
				t.Fatal("expected applyInPlaceMigrations to surface the step error")
			}
			if !errors.Is(err, sql.ErrConnDone) {
				t.Fatalf("error should wrap the step's cause; got %v", err)
			}
			if !strings.Contains(err.Error(), "v3") || !strings.Contains(err.Error(), "boom") {
				t.Fatalf("error should name the failing migration (v3/boom); got %q", err.Error())
			}
			for _, tbl := range []string{"a", "b"} {
				var name string
				e := db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, tbl).Scan(&name)
				if e != sql.ErrNoRows {
					t.Fatalf("table %q should have rolled back (shared transaction), got name=%q err=%v", tbl, name, e)
				}
			}
		})
	})
}

// TestOpenAtCurrentVersionIsNoOp covers the highest-frequency path — every
// daemon restart reopens an up-to-date store. It must be a no-op that
// preserves data; an off-by-one to wipe here would destroy the cache on every
// restart.
func TestOpenAtCurrentVersionIsNoOp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	withRawSeed := func(db *sql.DB) {
		if _, err := db.Exec(`INSERT INTO nodes (id, kind, name, file_path) VALUES ('n1','func','Foo','f.go')`); err != nil {
			t.Fatalf("seed node: %v", err)
		}
	}
	withRawSeed(s.db)
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen at current version: %v", err)
	}
	defer func() { _ = s2.Close() }()
	if v, _ := readUserVersion(s2.db); v != currentSchemaVersion {
		t.Fatalf("user_version = %d, want %d", v, currentSchemaVersion)
	}
	if n := nodeCount(t, s2.db); n != 1 {
		t.Fatalf("node count = %d, want 1 (a no-op reopen must NOT wipe)", n)
	}
	if s2.NeedsRebuild() {
		t.Fatal("a no-op reopen must not signal NeedsRebuild")
	}
}

// TestOpenWithInPlaceMigration drives the in-place arm end-to-end through the
// real Open composition (via the openWith seam): an older store at version 1
// is upgraded to version 2 by a registered in-place step that runs AFTER
// schemaSQL, the step's effect is visible, the existing data survives, and the
// version is stamped.
func TestOpenWithInPlaceMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.sqlite")

	// Create a v1 store with a row, then close.
	s, err := Open(path)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	if _, err := s.db.Exec(`INSERT INTO nodes (id, kind, name, file_path) VALUES ('n1','func','Foo','f.go')`); err != nil {
		t.Fatalf("seed node: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// An in-place v2 step that depends on the base schema (an index on a
	// nodes column) — proving it runs after schemaSQL/ensureNodeColumns.
	ran := false
	v2 := schemaMigration{version: 2, name: "idx-language", inPlace: func(tx *sql.Tx) error {
		ran = true
		_, err := tx.Exec(`CREATE INDEX IF NOT EXISTS test_nodes_by_language ON nodes(language)`)
		return err
	}}

	s2, err := openWith(path, 2, []schemaMigration{v2})
	if err != nil {
		t.Fatalf("openWith v2 in-place: %v", err)
	}
	defer func() { _ = s2.Close() }()
	if !ran {
		t.Fatal("the in-place migration step did not run")
	}
	if v, _ := readUserVersion(s2.db); v != 2 {
		t.Fatalf("user_version = %d, want 2", v)
	}
	if n := nodeCount(t, s2.db); n != 1 {
		t.Fatalf("node count = %d, want 1 (in-place upgrade must preserve data)", n)
	}
	var name string
	if err := s2.db.QueryRow(`SELECT name FROM sqlite_master WHERE type='index' AND name='test_nodes_by_language'`).Scan(&name); err != nil {
		t.Fatalf("in-place index not created: %v", err)
	}
	if s2.NeedsRebuild() {
		t.Fatal("an in-place upgrade must not signal NeedsRebuild")
	}
}

// TestOpenWithInPlaceFailureDoesNotStamp: a failing in-place step makes Open
// return an error and leaves the stored version unchanged, so the next open
// retries the upgrade rather than treating it as done.
func TestOpenWithInPlaceFailureDoesNotStamp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.sqlite")
	s, err := Open(path) // v1 store
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	boom := schemaMigration{version: 2, name: "boom", inPlace: func(*sql.Tx) error {
		return sql.ErrConnDone
	}}
	if _, err := openWith(path, 2, []schemaMigration{boom}); err == nil {
		t.Fatal("expected openWith to fail when an in-place step errors")
	}

	withRawDB(t, path, func(db *sql.DB) {
		var v int
		if err := db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
			t.Fatalf("read user_version: %v", err)
		}
		if v != 1 {
			t.Fatalf("user_version = %d after a failed migration, want 1 (unstamped, so the next open retries)", v)
		}
	})
}

// TestOpenWithMemoryUnderWipePlanStampsWithoutError: an in-memory store under a
// plan that would wipe an on-disk DB must not attempt a file removal — it is
// always fresh and simply stamps the current version.
func TestOpenWithMemoryUnderWipePlanStampsWithoutError(t *testing.T) {
	rebuildV2 := schemaMigration{version: 2, name: "typed-col", rebuild: true}
	// stored==0, current==2, a pending rebuild => plan.wipe==true; the memory
	// guard must skip the wipe and stamp anyway.
	s, err := openWith(":memory:", 2, []schemaMigration{rebuildV2})
	if err != nil {
		t.Fatalf("openWith :memory: under wipe plan: %v", err)
	}
	defer func() { _ = s.Close() }()
	if v, _ := readUserVersion(s.db); v != 2 {
		t.Fatalf("user_version = %d, want 2", v)
	}
	if s.NeedsRebuild() {
		t.Fatal(":memory: must never report a wipe (nothing to remove)")
	}
}

// TestNeedsRebuildSignalAfterWipe: a store written by a newer build is wiped on
// open and reports NeedsRebuild so the daemon forces a full re-index.
func TestNeedsRebuildSignalAfterWipe(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	withRawDB(t, path, func(db *sql.DB) {
		if _, err := db.Exec(`PRAGMA user_version = 999`); err != nil {
			t.Fatalf("set future version: %v", err)
		}
	})
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen newer DB: %v", err)
	}
	defer func() { _ = s2.Close() }()
	if !s2.NeedsRebuild() {
		t.Fatal("a wiped store must report NeedsRebuild so the daemon re-indexes")
	}
}

// TestSchemaMigrationsWellFormed asserts the shipped registry is valid and that
// the validator rejects the dangerous misconfigurations — above all, bumping
// currentSchemaVersion without appending a matching migration.
func TestSchemaMigrationsWellFormed(t *testing.T) {
	if err := validateSchemaMigrations(currentSchemaVersion, schemaMigrations); err != nil {
		t.Fatalf("shipped registry is invalid: %v", err)
	}

	inPlace := func(*sql.Tx) error { return nil }
	bad := []struct {
		name    string
		current int
		migs    []schemaMigration
	}{
		{"bumped version with no migration", 2, nil},
		{"highest below current", 3, []schemaMigration{{version: 2, name: "x", rebuild: true}}},
		{"both strategies set", 2, []schemaMigration{{version: 2, name: "x", rebuild: true, inPlace: inPlace}}},
		{"neither strategy set", 2, []schemaMigration{{version: 2, name: "x"}}},
		{"not strictly ascending", 3, []schemaMigration{{version: 2, name: "a", rebuild: true}, {version: 2, name: "b", rebuild: true}}},
		{"v1 entry (baseline is implicit)", 1, []schemaMigration{{version: 1, name: "a", rebuild: true}}},
	}
	for _, c := range bad {
		if err := validateSchemaMigrations(c.current, c.migs); err == nil {
			t.Errorf("%s: expected a validation error, got nil", c.name)
		}
	}
}
