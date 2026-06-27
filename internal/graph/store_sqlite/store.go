// Package store_sqlite is the on-disk, SQLite-backed implementation of
// graph.Store. It uses the pure-Go modernc.org/sqlite driver so the
// binary stays CGO-free on this code path, and satisfies the same
// conformance suite as the in-memory store (see
// internal/graph/storetest).
//
// Hot queries are precompiled as prepared statements in Open and
// closed in Close. Writes serialize through a single Go-side mutex
// because SQLite already serialises writers internally and an explicit
// mutex sidesteps SQLITE_BUSY contention when the conformance suite
// fans out 8 concurrent writers; reads still run concurrently under
// WAL mode.
//
// Meta maps are encoded as JSON (see meta_json.go); an empty / nil Meta
// is stored as NULL so the common case adds no row weight beyond the
// column header.
//
// EdgeIdentityRevisions is tracked in memory (atomic counter) -- it
// mirrors the in-memory store's monotonic "provenance churn" signal
// and does not need to survive process restarts (the in-memory store
// resets it on every New(), so the contract is per-process).
package store_sqlite

import (
	"database/sql"
	"errors"
	"fmt"
	"iter"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zzet/gortex/internal/graph"

	_ "modernc.org/sqlite"
)

// Store is the SQLite-backed graph.Store implementation.
type Store struct {
	db *sql.DB

	// dbPath is the on-disk SQLite file path, retained for size
	// telemetry — the WAL high-water mark surfaces in daemon_health so a
	// runaway -wal is observable rather than silently filling the disk.
	dbPath string

	// writeMu serialises every mutation. SQLite serialises writers
	// internally; doing the same on the Go side turns SQLITE_BUSY
	// contention into clean lock-wait and keeps the conformance
	// concurrency test predictable.
	writeMu sync.Mutex

	// resolveMu is the resolver-coordination mutex returned by
	// ResolveMutex. Held by cross-repo / temporal / external resolver
	// passes to keep their edge mutations from interleaving. Separate
	// from writeMu so the resolver can hold it across multiple writes
	// without blocking unrelated steady-state mutations.
	resolveMu sync.Mutex

	edgeIdentityRevs atomic.Int64

	// wiped records that Open dropped an incompatible on-disk DB and
	// recreated it empty (a schema-version mismatch that an in-place ALTER
	// could not satisfy). Surfaced via NeedsRebuild so the daemon forces a
	// full re-index on warm restart instead of an incremental reconcile,
	// rather than relying on the side effect that a total wipe also empties
	// file_mtimes.
	wiped bool

	// WAL-checkpoint loop lifecycle. In WAL mode a COMMIT only appends to
	// the -wal file; pages move into the main DB (and the WAL becomes
	// reusable) at a checkpoint. SQLite's default passive auto-checkpoint
	// reuses the WAL in place and never shrinks the file, so under steady
	// writes with ever-present readers (the pooled connections here, plus
	// any other process holding the store open) the -wal ratchets up to a
	// large high-water mark and stays there. runCheckpointLoop periodically
	// runs `PRAGMA wal_checkpoint(TRUNCATE)` to drain the log into the DB
	// and shrink the file back down. nil for in-memory stores (no WAL).
	stopCheckpoint chan struct{} // closed by Close to stop the loop
	checkpointDone chan struct{} // closed by the loop when it returns
	stopOnce       sync.Once     // makes stopCheckpointLoop idempotent

	// bundles is the content-addressed package-scoped cache over
	// SearchSymbolBundles: a query serves cached Node + in/out edges for
	// packages whose content fingerprint is unchanged and skips the node
	// + edge fan-out for them. nil until SetBundleFingerprints is first
	// called (the daemon wires it from the analysis pass); a nil cache
	// makes SearchSymbolBundles fall through to the uncached path.
	bundles *bundleCache

	// Prepared statements (compiled once in Open, closed in Close).
	stmtInsertNode         *sql.Stmt
	stmtGetNode            *sql.Stmt
	stmtGetNodeByQual      *sql.Stmt
	stmtFindByName         *sql.Stmt
	stmtFindByNameInRepo   *sql.Stmt
	stmtFileNodes          *sql.Stmt
	stmtRepoNodes          *sql.Stmt
	stmtAllNodes           *sql.Stmt
	stmtNodeCount          *sql.Stmt
	stmtRepoPrefixes       *sql.Stmt
	stmtRepoStatsNodes     *sql.Stmt
	stmtRepoStatsEdges     *sql.Stmt
	stmtRepoNodeCount      *sql.Stmt
	stmtRepoEdgeCount      *sql.Stmt
	stmtAllRepoCountsNodes *sql.Stmt
	stmtAllRepoCountsEdges *sql.Stmt
	stmtStatsByKind        *sql.Stmt
	stmtStatsByLanguage    *sql.Stmt

	stmtInsertEdge       *sql.Stmt
	stmtOutEdges         *sql.Stmt
	stmtOutEdgesLight    *sql.Stmt
	stmtInEdges          *sql.Stmt
	stmtRepoEdges        *sql.Stmt
	stmtAllEdges         *sql.Stmt
	stmtEdgeCount        *sql.Stmt
	stmtRemoveEdge       *sql.Stmt
	stmtUpdateEdgeOrigin *sql.Stmt
	stmtUpdateEdgeAttrs  *sql.Stmt
	stmtSelectEdgeOrigin *sql.Stmt
	stmtDeleteEdgeByKey  *sql.Stmt

	stmtSelectFileNodeIDs *sql.Stmt
	stmtSelectRepoNodeIDs *sql.Stmt
	stmtDeleteNodeByFile  *sql.Stmt
	stmtDeleteNodeByRepo  *sql.Stmt
}

// Compile-time assertion: *Store satisfies graph.Store.
var _ graph.Store = (*Store)(nil)

// ResolveMutex returns the resolver-coordination mutex. Held by
// cross-repo / temporal / external resolver passes to serialise edge
// mutations. Separate from writeMu (which protects per-statement
// write serialisation against SQLITE_BUSY) so the resolver can hold
// it across multi-write batches without blocking unrelated steady-
// state mutations on the same store.
func (s *Store) ResolveMutex() *sync.Mutex { return &s.resolveMu }

// NeedsRebuild reports that Open dropped an incompatible on-disk database and
// recreated it empty, so the daemon's warm-restart path should force a full
// re-index (bypassing an incremental reconcile that would carry stale state)
// — see cmd/gortex.storeNeedsRebuild, the capability probe this satisfies.
func (s *Store) NeedsRebuild() bool { return s.wiped }

// Open opens (or creates) the SQLite database at path, runs the schema
// migration, and prepares hot statements. The DB is opened with WAL
// journaling and synchronous=NORMAL -- the same durability/throughput
// tradeoff every embedded-SQLite app uses for write-heavy workloads.
//
// Pass ":memory:" for an ephemeral in-process database (handy for
// tests when you don't need on-disk persistence).
//
// By default Open will NOT destroy an incompatible on-disk database: if the
// stored schema version requires a rebuild (a newer build's DB, or an older
// one crossing a rebuild migration) it returns ErrSchemaRebuildRequired and
// leaves the file untouched. Pass WithRebuild to permit the drop-and-recreate
// — only a caller that holds exclusive access to the store may do so (see
// WithRebuild).
func Open(path string, opts ...Option) (*Store, error) {
	var o openOptions
	for _, opt := range opts {
		opt(&o)
	}
	return openWith(path, currentSchemaVersion, schemaMigrations, o.allowRebuild)
}

// Option configures Open.
type Option func(*openOptions)

type openOptions struct {
	allowRebuild bool
}

// WithRebuild permits Open to drop and recreate an on-disk database whose
// schema version is incompatible (a newer build's, or an older one crossing a
// migration that an in-place ALTER cannot satisfy).
//
// The caller MUST hold exclusive cross-process access to the store file —
// removing a SQLite file another process has open silently splits its state.
// The daemon satisfies this: it takes an exclusive flock on <store>.lock for
// the writable on-disk sqlite lifecycle and passes this option only in that
// branch (see serverstack.NewSharedServer / OpenBackend). Without it, a wipe
// plan yields ErrSchemaRebuildRequired and the file is left intact, so a
// caller that does not hold the lock cannot corrupt a live store.
func WithRebuild() Option { return func(o *openOptions) { o.allowRebuild = true } }

// ErrSchemaRebuildRequired is returned by Open when an on-disk database needs a
// destructive rebuild but the caller did not pass WithRebuild (i.e. cannot
// prove it holds the store lock).
var ErrSchemaRebuildRequired = errors.New("store_sqlite: on-disk schema is incompatible and must be rebuilt; reopen with WithRebuild while holding the store lock")

// openWith is Open parameterised by the target schema version, migration
// registry, and rebuild permission so tests can drive the baseline / in-place
// / rebuild arms without mutating package globals. Open passes the package
// defaults (currentSchemaVersion, schemaMigrations) and the WithRebuild flag.
func openWith(path string, current int, migrations []schemaMigration, allowRebuild bool) (*Store, error) {
	// Pragmas: WAL + synchronous=NORMAL is the standard write-heavy
	// embedded tradeoff. cache_size(-32768) gives each pooled connection a
	// 32 MiB page cache; temp_store(MEMORY) keeps GROUP BY / ORDER BY scratch
	// off disk; mmap_size(256 MiB) lets reads fault pages straight from the
	// OS page cache instead of copying through SQLite's. These materially
	// speed the resolver/query phases on a large graph.
	//
	// journal_size_limit(64 MiB) caps the -wal high-water mark: after any
	// checkpoint SQLite truncates the WAL back down to this size instead of
	// leaving it at whatever it grew to. Without it the WAL only ratchets
	// up (a passive checkpoint reuses the file in place, never shrinking
	// it), which is how a 535 MB DB ends up with an 11 GB -wal. This bounds
	// the file even between the explicit TRUNCATE checkpoints runCheckpointLoop
	// issues, and even if that loop is not running.
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(OFF)&_pragma=cache_size(-32768)&_pragma=temp_store(MEMORY)&_pragma=mmap_size(268435456)&_pragma=journal_size_limit(67108864)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("sqlite open: %w", err)
	}
	// Pool up to NumCPU connections so the resolver's parallel
	// worker fan-out (NumCPU goroutines doing FindNodesByName /
	// GetNode / GetOutEdges concurrently) doesn't serialise through
	// a single connection — the dominant gap between the SQLite and
	// bbolt backends on the bench's resolver stage was exactly that.
	// SQLite's WAL mode allows concurrent readers across multiple
	// connections; writes still serialise via writeMu on the Go
	// side, then via SQLite's internal write lock. Every connection
	// the pool opens picks up the journal-mode / synchronous /
	// busy-timeout pragmas from the DSN above, so we don't need to
	// pin one connection to "remember" them.
	db.SetMaxOpenConns(runtime.NumCPU())

	// Reconcile the on-disk schema version before applying schemaSQL. The graph
	// store is a rebuildable cache, so an incompatible (older needing a rebuild
	// step, or newer) DB is dropped and reindexed rather than migrated in place
	// (see schema_version.go). The daemon holds an exclusive store.lock around
	// Open, so wiping the file here cannot race another process.
	stored, err := readUserVersion(db)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("sqlite read schema version: %w", err)
	}
	plan := planSchemaMigrationWith(stored, current, migrations)
	didWipe := false
	if plan.wipe && !isMemoryPath(path) {
		// Refuse the destructive rebuild unless the caller proved it holds
		// exclusive access (WithRebuild). This keeps the file safe even if a
		// future caller reaches a wipe plan without the daemon's store lock.
		if !allowRebuild {
			_ = db.Close()
			return nil, ErrSchemaRebuildRequired
		}
		if err := db.Close(); err != nil {
			return nil, fmt.Errorf("sqlite close for rebuild: %w", err)
		}
		if err := removeStoreFiles(path); err != nil {
			return nil, fmt.Errorf("sqlite rebuild: %w", err)
		}
		db, err = sql.Open("sqlite", dsn)
		if err != nil {
			return nil, fmt.Errorf("sqlite reopen for rebuild: %w", err)
		}
		db.SetMaxOpenConns(runtime.NumCPU())
		didWipe = true
	}

	if _, err := db.Exec(schemaSQL); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("sqlite schema: %w", err)
	}
	// edges_external is a partial index over exactly the external-call
	// terminals, so ExternalCallCandidateEdges scans a tiny index instead
	// of the full edges table. Built from the shared predicate const (not
	// inlined in schemaSQL) so the index WHERE and the query WHERE stay
	// byte-identical — SQLite only uses a partial index when the query's
	// WHERE matches the index's.
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS edges_external ON edges(kind) WHERE ` + externalCallTargetPredicate); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("sqlite edges_external index: %w", err)
	}
	// Add the promoted node columns to databases created before they
	// existed (CREATE TABLE IF NOT EXISTS won't alter an existing table).
	// Must run before prepare(), whose node INSERT references them.
	if err := ensureNodeColumns(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("sqlite node columns: %w", err)
	}

	// Backfill the FTS rowid sidecar for databases built before it existed,
	// so the first incremental UpsertSymbolFTS on an already-indexed symbol
	// can do its O(log n) docid delete instead of leaking a duplicate row.
	// One-time; a no-op once the map is populated or the FTS index is empty.
	if err := backfillSymbolFTSRowidMap(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("sqlite fts rowid backfill: %w", err)
	}

	// Apply any in-place migration steps (none on a fresh, baseline, or wiped
	// DB), then stamp the current schema version. After a wipe the store is
	// empty and the daemon's normal indexing repopulates it.
	if plan.stamp {
		if err := applyInPlaceMigrations(db, plan.inPlace); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("sqlite schema migrate: %w", err)
		}
		if err := setUserVersion(db, current); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("sqlite stamp schema version: %w", err)
		}
	}

	s := &Store{db: db, dbPath: path, wiped: didWipe}
	// Initialise the bundle cache at construction so its pointer is
	// never written after Open — concurrent SearchSymbolBundles reads
	// and SetBundleFingerprints writes then race only on the cache's
	// own mutex-guarded maps, not on the Store field. The cache stays
	// inert (every lookup a miss) until the daemon supplies fingerprints.
	s.bundles = &bundleCache{
		fingerprints: map[string]uint64{},
		entries:      map[string]*bundleCacheEntry{},
	}
	if err := s.prepare(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("sqlite prepare: %w", err)
	}
	// In-memory databases have no WAL file to drain, so the periodic
	// checkpoint is pointless there (and would leak a goroutine per
	// short-lived test store). Only run it for on-disk stores.
	if !strings.Contains(path, ":memory:") {
		s.stopCheckpoint = make(chan struct{})
		s.checkpointDone = make(chan struct{})
		go s.runCheckpointLoop(walCheckpointInterval)
	}
	return s, nil
}

// walCheckpointInterval is how often runCheckpointLoop drains the WAL into
// the main DB and truncates the -wal file. Five minutes keeps the file
// bounded under steady writes without making the checkpoint itself a hot
// path; journal_size_limit in the DSN bounds growth between ticks.
const walCheckpointInterval = 5 * time.Minute

// runCheckpointLoop issues a TRUNCATE checkpoint every interval until Close
// stops it. Best-effort: a checkpoint that can't fully complete because a
// reader or writer holds the WAL just truncates what it can and retries on
// the next tick.
func (s *Store) runCheckpointLoop(interval time.Duration) {
	defer close(s.checkpointDone)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-s.stopCheckpoint:
			return
		case <-ticker.C:
			_ = s.CheckpointWAL()
		}
	}
}

// CheckpointWAL runs `PRAGMA wal_checkpoint(TRUNCATE)`: it flushes the
// write-ahead log into the main database file and shrinks the -wal back to
// zero. A passive checkpoint (SQLite's default) only reuses the WAL in
// place and never reclaims the space; TRUNCATE is the mode that does.
// Exposed so a daemon shutdown path or an operator command can force a
// drain; the background loop calls it on a timer. Not held under writeMu —
// SQLite coordinates checkpoints against writers internally, and blocking
// steady-state writes on a maintenance op is the wrong tradeoff.
func (s *Store) CheckpointWAL() error {
	_, err := s.db.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
	return err
}

// stopCheckpointLoop signals the background loop to exit and waits for it,
// so callers can be sure no checkpoint is in flight before closing s.db.
// Idempotent: safe to call from Close more than once.
func (s *Store) stopCheckpointLoop() {
	s.stopOnce.Do(func() {
		if s.stopCheckpoint != nil {
			close(s.stopCheckpoint)
			<-s.checkpointDone
		}
	})
}

// Close closes every prepared statement and the underlying *sql.DB. It
// first stops the WAL-checkpoint loop and issues one final TRUNCATE
// checkpoint so the -wal file is drained and shrunk on graceful shutdown
// rather than lingering at its high-water mark until the next open.
func (s *Store) Close() error {
	s.stopCheckpointLoop()
	if s.checkpointDone != nil { // on-disk store: drain the WAL one last time
		_ = s.CheckpointWAL()
	}
	stmts := []*sql.Stmt{
		s.stmtInsertNode, s.stmtGetNode, s.stmtGetNodeByQual,
		s.stmtFindByName, s.stmtFindByNameInRepo,
		s.stmtFileNodes, s.stmtRepoNodes,
		s.stmtAllNodes, s.stmtNodeCount, s.stmtRepoPrefixes,
		s.stmtRepoStatsNodes, s.stmtRepoStatsEdges,
		s.stmtRepoNodeCount, s.stmtRepoEdgeCount,
		s.stmtAllRepoCountsNodes, s.stmtAllRepoCountsEdges,
		s.stmtStatsByKind, s.stmtStatsByLanguage,
		s.stmtInsertEdge, s.stmtOutEdges, s.stmtOutEdgesLight, s.stmtInEdges,
		s.stmtRepoEdges,
		s.stmtAllEdges, s.stmtEdgeCount, s.stmtRemoveEdge,
		s.stmtUpdateEdgeOrigin, s.stmtUpdateEdgeAttrs, s.stmtSelectEdgeOrigin, s.stmtDeleteEdgeByKey,
		s.stmtSelectFileNodeIDs, s.stmtSelectRepoNodeIDs,
		s.stmtDeleteNodeByFile, s.stmtDeleteNodeByRepo,
	}
	for _, st := range stmts {
		if st != nil {
			_ = st.Close()
		}
	}
	return s.db.Close()
}

func (s *Store) prepare() error {
	var err error
	prep := func(out **sql.Stmt, q string) {
		if err != nil {
			return
		}
		var st *sql.Stmt
		st, err = s.db.Prepare(q)
		if err != nil {
			err = fmt.Errorf("prepare %q: %w", q, err)
			return
		}
		*out = st
	}

	const nodeCols = lookupNodeCols

	prep(&s.stmtInsertNode,
		`INSERT OR REPLACE INTO nodes (`+nodeCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	prep(&s.stmtGetNode,
		`SELECT `+nodeCols+` FROM nodes WHERE id = ?`)
	prep(&s.stmtGetNodeByQual,
		`SELECT `+nodeCols+` FROM nodes WHERE qual_name = ? LIMIT 1`)
	prep(&s.stmtFindByName,
		`SELECT `+nodeCols+` FROM nodes WHERE name = ?`)
	prep(&s.stmtFindByNameInRepo,
		`SELECT `+nodeCols+` FROM nodes WHERE name = ? AND repo_prefix = ?`)
	prep(&s.stmtFileNodes,
		`SELECT `+nodeCols+` FROM nodes WHERE file_path = ?`)
	prep(&s.stmtRepoNodes,
		`SELECT `+nodeCols+` FROM nodes WHERE repo_prefix = ?`)
	prep(&s.stmtAllNodes,
		`SELECT `+nodeCols+` FROM nodes`)
	prep(&s.stmtNodeCount,
		`SELECT COUNT(*) FROM nodes`)
	prep(&s.stmtRepoPrefixes,
		`SELECT DISTINCT repo_prefix FROM nodes WHERE repo_prefix <> ''`)

	prep(&s.stmtRepoStatsNodes,
		`SELECT repo_prefix, kind, language, COUNT(*) FROM nodes WHERE repo_prefix <> '' GROUP BY repo_prefix, kind, language`)
	prep(&s.stmtRepoStatsEdges,
		`SELECT n.repo_prefix, COUNT(*)
		 FROM edges e
		 JOIN nodes n ON n.id = e.from_id
		 WHERE n.repo_prefix <> ''
		 GROUP BY n.repo_prefix`)
	prep(&s.stmtRepoNodeCount,
		`SELECT COUNT(*) FROM nodes WHERE repo_prefix = ?`)
	prep(&s.stmtRepoEdgeCount,
		`SELECT COUNT(*)
		 FROM edges e
		 JOIN nodes n ON n.id = e.from_id
		 WHERE n.repo_prefix = ?`)
	prep(&s.stmtAllRepoCountsNodes,
		`SELECT repo_prefix, COUNT(*) FROM nodes WHERE repo_prefix <> '' GROUP BY repo_prefix`)
	prep(&s.stmtAllRepoCountsEdges,
		`SELECT n.repo_prefix, COUNT(*)
		 FROM edges e
		 JOIN nodes n ON n.id = e.from_id
		 WHERE n.repo_prefix <> ''
		 GROUP BY n.repo_prefix`)

	prep(&s.stmtStatsByKind,
		`SELECT kind, COUNT(*) FROM nodes GROUP BY kind`)
	prep(&s.stmtStatsByLanguage,
		`SELECT language, COUNT(*) FROM nodes GROUP BY language`)

	const edgeCols = `from_id, to_id, kind, file_path, line, confidence, confidence_label, origin, tier, cross_repo, meta`

	prep(&s.stmtInsertEdge,
		`INSERT OR IGNORE INTO edges (`+edgeCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?)`)
	prep(&s.stmtOutEdges,
		`SELECT `+edgeCols+` FROM edges WHERE from_id = ?`)
	const edgeColsLight = `from_id, to_id, kind, file_path, line, confidence, confidence_label, origin, tier, cross_repo`
	prep(&s.stmtOutEdgesLight,
		`SELECT `+edgeColsLight+` FROM edges WHERE from_id = ?`)
	prep(&s.stmtInEdges,
		`SELECT `+edgeCols+` FROM edges WHERE to_id = ?`)
	prep(&s.stmtRepoEdges,
		`SELECT e.from_id, e.to_id, e.kind, e.file_path, e.line,
		        e.confidence, e.confidence_label, e.origin, e.tier,
		        e.cross_repo, e.meta
		   FROM edges e
		   JOIN nodes n ON n.id = e.from_id
		  WHERE n.repo_prefix = ?`)
	prep(&s.stmtAllEdges,
		`SELECT `+edgeCols+` FROM edges`)
	prep(&s.stmtEdgeCount,
		`SELECT COUNT(*) FROM edges`)
	prep(&s.stmtRemoveEdge,
		`DELETE FROM edges WHERE from_id = ? AND to_id = ? AND kind = ?`)

	prep(&s.stmtSelectEdgeOrigin,
		`SELECT origin FROM edges WHERE from_id = ? AND to_id = ? AND kind = ? AND file_path = ? AND line = ?`)
	prep(&s.stmtUpdateEdgeOrigin,
		`UPDATE edges SET origin = ?, tier = ? WHERE from_id = ? AND to_id = ? AND kind = ? AND file_path = ? AND line = ?`)
	prep(&s.stmtUpdateEdgeAttrs,
		`UPDATE edges SET confidence = ?, confidence_label = ?, origin = ?, tier = ?, meta = ? WHERE from_id = ? AND to_id = ? AND kind = ? AND file_path = ? AND line = ?`)
	prep(&s.stmtDeleteEdgeByKey,
		`DELETE FROM edges WHERE from_id = ? AND to_id = ? AND kind = ? AND file_path = ? AND line = ?`)

	prep(&s.stmtSelectFileNodeIDs,
		`SELECT id FROM nodes WHERE file_path = ?`)
	prep(&s.stmtSelectRepoNodeIDs,
		`SELECT id FROM nodes WHERE repo_prefix = ?`)
	prep(&s.stmtDeleteNodeByFile,
		`DELETE FROM nodes WHERE file_path = ?`)
	prep(&s.stmtDeleteNodeByRepo,
		`DELETE FROM nodes WHERE repo_prefix = ?`)

	return err
}

// encodeMeta / decodeMeta live in meta_json.go (JSON codec + the
// metaWire typed DTO + the legacy-gob dual-read fallback).

// -- row scanners ---------------------------------------------------------

func scanNode(scanner interface {
	Scan(...any) error
}) (*graph.Node, error) {
	var (
		n        graph.Node
		metaBlob []byte
		p        promotedNodeMeta
	)
	err := scanner.Scan(
		&n.ID, &n.Kind, &n.Name, &n.QualName, &n.FilePath,
		&n.StartLine, &n.EndLine, &n.StartColumn, &n.EndColumn, &n.Language,
		&n.RepoPrefix, &n.WorkspaceID, &n.ProjectID,
		&p.sig, &p.vis, &p.doc, &p.external, &p.returnType,
		&p.isAsync, &p.isStatic, &p.isAbstract, &p.isExported, &p.updatedAt,
		&metaBlob,
	)
	if err != nil {
		return nil, err
	}
	if len(metaBlob) > 0 {
		m, derr := decodeMeta(metaBlob)
		if derr != nil {
			return nil, derr
		}
		n.Meta = m
	}
	// Restore the promoted columns into Meta. They are authoritative for
	// rows written after the promotion; a NULL column (legacy gob rows)
	// is left alone so the blob-carried value survives.
	restorePromotedMeta(&n, p)
	return &n, nil
}

func scanEdge(scanner interface {
	Scan(...any) error
}) (*graph.Edge, error) {
	var (
		e         graph.Edge
		metaBlob  []byte
		crossRepo int64
	)
	err := scanner.Scan(
		&e.From, &e.To, &e.Kind, &e.FilePath, &e.Line,
		&e.Confidence, &e.ConfidenceLabel, &e.Origin, &e.Tier,
		&crossRepo, &metaBlob,
	)
	if err != nil {
		return nil, err
	}
	e.CrossRepo = crossRepo != 0
	if len(metaBlob) > 0 {
		m, derr := decodeMeta(metaBlob)
		if derr != nil {
			return nil, derr
		}
		e.Meta = m
	}
	return &e, nil
}

// scanEdgeLight scans an edge WITHOUT decoding its meta blob -- for hot
// read paths (dataflow call-target lookup) that read only endpoints,
// kind, and line. Skipping the meta column avoids the JSON decode + map
// allocation that dominates large edge scans on this backend; the
// returned edge's Meta is nil.
func scanEdgeLight(scanner interface {
	Scan(...any) error
}) (*graph.Edge, error) {
	var (
		e         graph.Edge
		crossRepo int64
	)
	err := scanner.Scan(
		&e.From, &e.To, &e.Kind, &e.FilePath, &e.Line,
		&e.Confidence, &e.ConfidenceLabel, &e.Origin, &e.Tier,
		&crossRepo,
	)
	if err != nil {
		return nil, err
	}
	e.CrossRepo = crossRepo != 0
	return &e, nil
}

// -- writes ---------------------------------------------------------------

// AddNode inserts or replaces a node. Idempotent on the id column --
// re-adding the same id with new content does a last-write-wins
// update, matching the in-memory store's behaviour.
func (s *Store) AddNode(n *graph.Node) {
	if n == nil || n.ID == "" {
		return
	}
	// Cross-daemon proxy nodes are volatile remote-derived state and
	// must never reach disk. The durable writer is the single gate —
	// neither the resolver mint path nor the hydrator carries its own
	// "don't persist" branch. A dropped proxy node is re-minted on
	// demand after a restart.
	if graph.IsProxyNode(n) {
		return
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.insertNodeLocked(s.stmtInsertNode, n); err != nil {
		// graph.Store.AddNode has no error channel; the in-memory
		// store can't fail either. We swallow the error here for API
		// parity; surface as a panic only on a clearly catastrophic
		// failure (closed DB), not on a transient busy.
		panicOnFatal(err)
	}
}

func (s *Store) insertNodeLocked(stmt *sql.Stmt, n *graph.Node) error {
	p, blobMeta := extractPromotedMeta(n.Meta)
	metaBlob, err := encodeMeta(blobMeta)
	if err != nil {
		return err
	}
	_, err = stmt.Exec(
		n.ID, string(n.Kind), n.Name, n.QualName, n.FilePath,
		n.StartLine, n.EndLine, n.StartColumn, n.EndColumn, n.Language,
		n.RepoPrefix, n.WorkspaceID, n.ProjectID,
		p.sig, p.vis, p.doc, p.external, p.returnType,
		p.isAsync, p.isStatic, p.isAbstract, p.isExported, p.updatedAt,
		metaBlob,
	)
	return err
}

// AddEdge inserts an edge. Idempotent on the logical edge key (from,
// to, kind, file_path, line) -- a second AddEdge with the same key is
// a no-op (INSERT OR IGNORE), matching the in-memory store's "stored
// pointer replaced in place" semantics. Origin upgrades on a re-add
// are NOT applied through this path; use SetEdgeProvenance for that
// (matches the in-memory store: AddEdge replaces the *Edge pointer,
// but the conformance suite only verifies dedup-by-key, not pointer
// replacement, and the in-memory store also routes provenance
// upgrades through SetEdgeProvenance).
func (s *Store) AddEdge(e *graph.Edge) {
	if e == nil {
		return
	}
	// An edge to/from a cross-daemon proxy node is volatile and never
	// persisted (the proxy node itself is dropped at AddNode).
	if graph.IsProxyID(e.From) || graph.IsProxyID(e.To) {
		return
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.insertEdgeLocked(s.stmtInsertEdge, e); err != nil {
		panicOnFatal(err)
	}
}

func (s *Store) insertEdgeLocked(stmt *sql.Stmt, e *graph.Edge) error {
	metaBlob, err := encodeMeta(e.Meta)
	if err != nil {
		return err
	}
	var crossRepo int64
	if e.CrossRepo {
		crossRepo = 1
	}
	_, err = stmt.Exec(
		e.From, e.To, string(e.Kind), e.FilePath, e.Line,
		e.Confidence, e.ConfidenceLabel, e.Origin, e.Tier,
		crossRepo, metaBlob,
	)
	return err
}

// AddBatch inserts nodes and edges in a single transaction -- the
// 10-100x speedup vs per-statement commits at indexing scale.
func (s *Store) AddBatch(nodes []*graph.Node, edges []*graph.Edge) {
	if len(nodes) == 0 && len(edges) == 0 {
		return
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		panicOnFatal(err)
		return
	}
	commit := false
	defer func() {
		if !commit {
			_ = tx.Rollback()
		}
	}()

	insertNode := tx.Stmt(s.stmtInsertNode)
	defer insertNode.Close()
	insertEdge := tx.Stmt(s.stmtInsertEdge)
	defer insertEdge.Close()

	for _, n := range nodes {
		if n == nil || n.ID == "" {
			continue
		}
		// Cross-daemon proxy nodes never reach disk.
		if graph.IsProxyNode(n) {
			continue
		}
		if err := s.insertNodeLocked(insertNode, n); err != nil {
			panicOnFatal(err)
			return
		}
	}
	for _, e := range edges {
		if e == nil {
			continue
		}
		// An edge to or from a proxy node is volatile remote-derived
		// state too; never persist it (it would dangle on reload since
		// the proxy node itself is dropped).
		if graph.IsProxyID(e.From) || graph.IsProxyID(e.To) {
			continue
		}
		if err := s.insertEdgeLocked(insertEdge, e); err != nil {
			panicOnFatal(err)
			return
		}
	}

	if err := tx.Commit(); err != nil {
		panicOnFatal(err)
		return
	}
	commit = true
}

// SetEdgeProvenance mutates an existing edge's origin in-place and
// bumps the identity-revision counter when the origin actually
// changes. Returns true iff a change was applied. Mirrors the
// in-memory store's "delete-then-insert of identity" semantics.
func (s *Store) SetEdgeProvenance(e *graph.Edge, newOrigin string) bool {
	if e == nil {
		return false
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	// Look up the stored origin -- the caller-supplied *Edge may be a
	// detached copy whose Origin already matches newOrigin even though
	// the row still has the old value.
	var storedOrigin string
	row := s.stmtSelectEdgeOrigin.QueryRow(e.From, e.To, string(e.Kind), e.FilePath, e.Line)
	if err := row.Scan(&storedOrigin); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false
		}
		panicOnFatal(err)
		return false
	}
	if storedOrigin == newOrigin {
		return false
	}
	newTier := e.Tier
	if newTier != "" {
		newTier = graph.ResolvedBy(newOrigin)
	}
	if _, err := s.stmtUpdateEdgeOrigin.Exec(newOrigin, newTier, e.From, e.To, string(e.Kind), e.FilePath, e.Line); err != nil {
		panicOnFatal(err)
		return false
	}
	// Reflect the change on the caller's struct, mirroring the
	// in-memory store which mutates the in-graph *Edge in place.
	e.Origin = newOrigin
	if e.Tier != "" {
		e.Tier = newTier
	}
	s.edgeIdentityRevs.Add(1)
	return true
}

// PersistEdgeAttributes durably rewrites the mutable attribute columns
// (confidence, confidence_label, origin, tier, meta) of the edge row
// identified by e's full logical key. It is the disk-backend counterpart
// to the in-memory store's "mutate the live *Edge in place" behaviour: a
// pass that confirms an edge's full provenance bundle (not just origin)
// calls this so the confidence / label / meta survive a reload. A missing
// row is a silent no-op (UPDATE ... WHERE matches nothing).
func (s *Store) PersistEdgeAttributes(e *graph.Edge) {
	if e == nil {
		return
	}
	metaBlob, err := encodeMeta(e.Meta)
	if err != nil {
		panicOnFatal(err)
		return
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if _, err := s.stmtUpdateEdgeAttrs.Exec(
		e.Confidence, e.ConfidenceLabel, e.Origin, e.Tier, metaBlob,
		e.From, e.To, string(e.Kind), e.FilePath, e.Line,
	); err != nil {
		panicOnFatal(err)
	}
}

// ReindexEdge updates the stored row after e.To has been mutated from
// oldTo to e.To. Implemented as delete-old + insert-new under the
// same write lock (SQLite's UNIQUE constraint on (from,to,kind,file,
// line) makes "UPDATE to_id" a one-shot, but the delete+insert form
// keeps semantics identical when the new (from,to,...) key happens to
// already exist -- the INSERT OR IGNORE drops the dup, just like the
// in-memory store's bucket-replace).
func (s *Store) ReindexEdge(e *graph.Edge, oldTo string) {
	if e == nil || oldTo == e.To {
		return
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	if _, err := s.stmtDeleteEdgeByKey.Exec(e.From, oldTo, string(e.Kind), e.FilePath, e.Line); err != nil {
		panicOnFatal(err)
		return
	}
	if err := s.insertEdgeLocked(s.stmtInsertEdge, e); err != nil {
		panicOnFatal(err)
		return
	}
}

// reindexChunkSize bounds the number of edge re-binds per BEGIN/COMMIT.
// Same shape as the bbolt sibling: large enough to amortise the
// per-tx overhead (BEGIN+COMMIT plus WAL fsync) but small enough that
// the WAL doesn't balloon and a crash mid-batch only loses ≤chunk
// mutations.
const reindexChunkSize = 5000

// ReindexEdges chunks the batch into reindexChunkSize-mutation
// transactions and runs each through prepared statements re-used
// across the chunk. Per-edge ReindexEdge was the resolver hot path
// (10k+ calls = 10k+ BEGIN/COMMIT pairs); this collapses them to two.
func (s *Store) ReindexEdges(batch []graph.EdgeReindex) {
	if len(batch) == 0 {
		return
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	for i := 0; i < len(batch); i += reindexChunkSize {
		end := minInt(i+reindexChunkSize, len(batch))
		chunk := batch[i:end]
		tx, err := s.db.Begin()
		if err != nil {
			panicOnFatal(err)
			return
		}
		delStmt := tx.Stmt(s.stmtDeleteEdgeByKey)
		insStmt := tx.Stmt(s.stmtInsertEdge)
		for _, r := range chunk {
			if r.Edge == nil || r.OldTo == r.Edge.To {
				continue
			}
			if _, err := delStmt.Exec(r.Edge.From, r.OldTo, string(r.Edge.Kind), r.Edge.FilePath, r.Edge.Line); err != nil {
				_ = tx.Rollback()
				panicOnFatal(err)
				return
			}
			if err := s.insertEdgeLocked(insStmt, r.Edge); err != nil {
				_ = tx.Rollback()
				panicOnFatal(err)
				return
			}
		}
		if err := tx.Commit(); err != nil {
			panicOnFatal(err)
			return
		}
	}
}

// SetEdgeProvenanceBatch chunks origin promotions into one BEGIN/
// COMMIT per chunk and bumps the in-process revision counter once
// per actual change, matching the per-edge SetEdgeProvenance's
// semantics. Returns the total number of edges whose Origin changed.
func (s *Store) SetEdgeProvenanceBatch(batch []graph.EdgeProvenanceUpdate) int {
	if len(batch) == 0 {
		return 0
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	totalChanged := 0
	for i := 0; i < len(batch); i += reindexChunkSize {
		end := minInt(i+reindexChunkSize, len(batch))
		chunk := batch[i:end]
		tx, err := s.db.Begin()
		if err != nil {
			panicOnFatal(err)
			return totalChanged
		}
		selStmt := tx.Stmt(s.stmtSelectEdgeOrigin)
		updStmt := tx.Stmt(s.stmtUpdateEdgeOrigin)
		chunkChanged := 0
		for _, u := range chunk {
			if u.Edge == nil {
				continue
			}
			var storedOrigin string
			row := selStmt.QueryRow(u.Edge.From, u.Edge.To, string(u.Edge.Kind), u.Edge.FilePath, u.Edge.Line)
			if err := row.Scan(&storedOrigin); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					continue
				}
				_ = tx.Rollback()
				panicOnFatal(err)
				return totalChanged
			}
			if storedOrigin == u.NewOrigin {
				continue
			}
			newTier := u.Edge.Tier
			if newTier != "" {
				newTier = graph.ResolvedBy(u.NewOrigin)
			}
			if _, err := updStmt.Exec(u.NewOrigin, newTier, u.Edge.From, u.Edge.To, string(u.Edge.Kind), u.Edge.FilePath, u.Edge.Line); err != nil {
				_ = tx.Rollback()
				panicOnFatal(err)
				return totalChanged
			}
			u.Edge.Origin = u.NewOrigin
			if u.Edge.Tier != "" {
				u.Edge.Tier = newTier
			}
			chunkChanged++
		}
		if err := tx.Commit(); err != nil {
			panicOnFatal(err)
			return totalChanged
		}
		if chunkChanged > 0 {
			s.edgeIdentityRevs.Add(int64(chunkChanged))
		}
		totalChanged += chunkChanged
	}
	return totalChanged
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// RemoveEdge deletes every edge between (from, to) with the given
// kind. Returns true iff at least one row was deleted.
func (s *Store) RemoveEdge(from, to string, kind graph.EdgeKind) bool {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	res, err := s.stmtRemoveEdge.Exec(from, to, string(kind))
	if err != nil {
		panicOnFatal(err)
		return false
	}
	n, err := res.RowsAffected()
	if err != nil {
		panicOnFatal(err)
		return false
	}
	return n > 0
}

// EvictFile removes every node anchored to filePath and every edge
// that touches one of those nodes. Returns (nodesRemoved,
// edgesRemoved).
func (s *Store) EvictFile(filePath string) (nodesRemoved, edgesRemoved int) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.evictByScopeLocked(s.stmtSelectFileNodeIDs, s.stmtDeleteNodeByFile, filePath)
}

// EvictRepo removes every node in repoPrefix and every edge that
// touches one. Returns (nodesRemoved, edgesRemoved).
func (s *Store) EvictRepo(repoPrefix string) (nodesRemoved, edgesRemoved int) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.evictByScopeLocked(s.stmtSelectRepoNodeIDs, s.stmtDeleteNodeByRepo, repoPrefix)
}

// evictByScopeLocked is the shared body of EvictFile / EvictRepo --
// collect the affected node IDs, delete every edge touching one of
// them, then delete the nodes themselves.
func (s *Store) evictByScopeLocked(selectIDs, deleteNodes *sql.Stmt, scope string) (int, int) {
	rows, err := selectIDs.Query(scope)
	if err != nil {
		panicOnFatal(err)
		return 0, 0
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			panicOnFatal(err)
			return 0, 0
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		panicOnFatal(err)
		return 0, 0
	}
	_ = rows.Close()
	if len(ids) == 0 {
		return 0, 0
	}

	// Delete every edge touching one of these nodes. We run a single
	// DELETE per node id to avoid bumping into SQLite's bound-variable
	// limit on big batches; under the write lock this is a
	// straight-line walk.
	var edgesRemoved int
	for _, id := range ids {
		res, err := s.db.Exec(`DELETE FROM edges WHERE from_id = ? OR to_id = ?`, id, id)
		if err != nil {
			panicOnFatal(err)
			return 0, edgesRemoved
		}
		if n, err := res.RowsAffected(); err == nil {
			edgesRemoved += int(n)
		}
	}

	res, err := deleteNodes.Exec(scope)
	if err != nil {
		panicOnFatal(err)
		return 0, edgesRemoved
	}
	n, err := res.RowsAffected()
	if err != nil {
		panicOnFatal(err)
		return 0, edgesRemoved
	}
	return int(n), edgesRemoved
}

// -- reads ---------------------------------------------------------------

func (s *Store) GetNode(id string) *graph.Node {
	row := s.stmtGetNode.QueryRow(id)
	n, err := scanNode(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		panicOnFatal(err)
		return nil
	}
	return n
}

func (s *Store) GetNodeByQualName(qualName string) *graph.Node {
	if qualName == "" {
		return nil
	}
	row := s.stmtGetNodeByQual.QueryRow(qualName)
	n, err := scanNode(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		panicOnFatal(err)
		return nil
	}
	return n
}

func (s *Store) FindNodesByName(name string) []*graph.Node {
	return s.queryNodes(s.stmtFindByName, name)
}

func (s *Store) FindNodesByNameInRepo(name, repoPrefix string) []*graph.Node {
	return s.queryNodes(s.stmtFindByNameInRepo, name, repoPrefix)
}

func (s *Store) GetFileNodes(filePath string) []*graph.Node {
	return s.queryNodes(s.stmtFileNodes, filePath)
}

func (s *Store) GetRepoNodes(repoPrefix string) []*graph.Node {
	return s.queryNodes(s.stmtRepoNodes, repoPrefix)
}

func (s *Store) AllNodes() []*graph.Node {
	return s.queryNodes(s.stmtAllNodes)
}

func (s *Store) queryNodes(stmt *sql.Stmt, args ...any) []*graph.Node {
	rows, err := stmt.Query(args...)
	if err != nil {
		panicOnFatal(err)
		return nil
	}
	defer rows.Close()
	var out []*graph.Node
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			panicOnFatal(err)
			return out
		}
		out = append(out, n)
	}
	return out
}

// GetRepoNonContentNodes is the graph.NonContentNodeReader fast path: a
// SQL-level enumeration that drops CONTENT (data_class="content") section
// nodes, so the code-oriented passes never materialise a content-heavy
// repo's hundreds of thousands of sections. Meta is JSON (encodeMeta) in a
// BLOB column, so json_extract reads it via CAST(... AS TEXT); the NULL-safe
// `IS NOT 'content'` keeps every node whose meta is absent or carries any
// other (or no) data_class. An empty repoPrefix spans all repos.
func (s *Store) GetRepoNonContentNodes(repoPrefix string) []*graph.Node {
	const filter = `json_extract(CAST(meta AS TEXT), '$.data_class') IS NOT 'content'`
	if repoPrefix == "" {
		return s.scanNodeQuery(`SELECT ` + lookupNodeCols + ` FROM nodes WHERE ` + filter)
	}
	return s.scanNodeQuery(`SELECT `+lookupNodeCols+` FROM nodes WHERE repo_prefix = ? AND `+filter, repoPrefix)
}

// scanNodeQuery runs an ad-hoc node SELECT (columns = lookupNodeCols) and
// scans its rows into nodes — for the few non-hot enumerations that need a
// WHERE clause the prepared statements don't cover.
func (s *Store) scanNodeQuery(query string, args ...any) []*graph.Node {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		panicOnFatal(err)
		return nil
	}
	defer rows.Close()
	var out []*graph.Node
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			panicOnFatal(err)
			return out
		}
		out = append(out, n)
	}
	return out
}

func (s *Store) GetOutEdges(nodeID string) []*graph.Edge {
	return s.queryEdges(s.stmtOutEdges, nodeID)
}

// GetOutEdgesLight returns a node's out-edges without decoding the
// per-edge Meta blob -- for hot dataflow lookups that need only
// endpoints/kind/line. The returned edges have a nil Meta.
func (s *Store) GetOutEdgesLight(nodeID string) []*graph.Edge {
	return s.queryEdgesLight(s.stmtOutEdgesLight, nodeID)
}

func (s *Store) GetInEdges(nodeID string) []*graph.Edge {
	return s.queryEdges(s.stmtInEdges, nodeID)
}

func (s *Store) AllEdges() []*graph.Edge {
	return s.queryEdges(s.stmtAllEdges)
}

// GetRepoEdges returns every edge whose source node has the given
// RepoPrefix. The pre-Store idiom — GetRepoNodes(r) followed by
// GetOutEdges(n.ID) per node — was O(repo_nodes) prepared-statement
// invocations, which on a multi-repo workspace dominated the
// per-repo extractor passes. A single JOIN over edges/nodes keyed
// on n.repo_prefix runs as one prepared statement and hits the
// existing repo_prefix index.
func (s *Store) GetRepoEdges(repoPrefix string) []*graph.Edge {
	if repoPrefix == "" {
		return nil
	}
	return s.queryEdges(s.stmtRepoEdges, repoPrefix)
}

func (s *Store) queryEdges(stmt *sql.Stmt, args ...any) []*graph.Edge {
	rows, err := stmt.Query(args...)
	if err != nil {
		panicOnFatal(err)
		return nil
	}
	defer rows.Close()
	var out []*graph.Edge
	for rows.Next() {
		e, err := scanEdge(rows)
		if err != nil {
			panicOnFatal(err)
			return out
		}
		out = append(out, e)
	}
	return out
}

// queryEdgesLight mirrors queryEdges but scans each row without its
// meta blob (scanEdgeLight), leaving Meta nil. Only for callers that
// never read edge Meta.
func (s *Store) queryEdgesLight(stmt *sql.Stmt, args ...any) []*graph.Edge {
	rows, err := stmt.Query(args...)
	if err != nil {
		panicOnFatal(err)
		return nil
	}
	defer rows.Close()
	var out []*graph.Edge
	for rows.Next() {
		e, err := scanEdgeLight(rows)
		if err != nil {
			panicOnFatal(err)
			return out
		}
		out = append(out, e)
	}
	return out
}

// -- counts and stats -----------------------------------------------------

func (s *Store) NodeCount() int {
	var n int
	if err := s.stmtNodeCount.QueryRow().Scan(&n); err != nil {
		panicOnFatal(err)
		return 0
	}
	return n
}

func (s *Store) EdgeCount() int {
	var n int
	if err := s.stmtEdgeCount.QueryRow().Scan(&n); err != nil {
		panicOnFatal(err)
		return 0
	}
	return n
}

func (s *Store) Stats() graph.GraphStats {
	st := graph.GraphStats{
		ByKind:     map[string]int{},
		ByLanguage: map[string]int{},
	}
	st.TotalNodes = s.NodeCount()
	st.TotalEdges = s.EdgeCount()

	rows, err := s.stmtStatsByKind.Query()
	if err != nil {
		panicOnFatal(err)
		return st
	}
	for rows.Next() {
		var kind string
		var n int
		if err := rows.Scan(&kind, &n); err != nil {
			_ = rows.Close()
			panicOnFatal(err)
			return st
		}
		st.ByKind[kind] = n
	}
	_ = rows.Close()

	rows, err = s.stmtStatsByLanguage.Query()
	if err != nil {
		panicOnFatal(err)
		return st
	}
	for rows.Next() {
		var lang string
		var n int
		if err := rows.Scan(&lang, &n); err != nil {
			_ = rows.Close()
			panicOnFatal(err)
			return st
		}
		st.ByLanguage[lang] = n
	}
	_ = rows.Close()
	return st
}

func (s *Store) RepoStats() map[string]graph.GraphStats {
	out := map[string]graph.GraphStats{}
	rows, err := s.stmtRepoStatsNodes.Query()
	if err != nil {
		panicOnFatal(err)
		return out
	}
	for rows.Next() {
		var repo, kind, lang string
		var n int
		if err := rows.Scan(&repo, &kind, &lang, &n); err != nil {
			_ = rows.Close()
			panicOnFatal(err)
			return out
		}
		st, ok := out[repo]
		if !ok {
			st = graph.GraphStats{ByKind: map[string]int{}, ByLanguage: map[string]int{}}
		}
		st.TotalNodes += n
		st.ByKind[kind] += n
		st.ByLanguage[lang] += n
		out[repo] = st
	}
	_ = rows.Close()

	rows, err = s.stmtRepoStatsEdges.Query()
	if err != nil {
		panicOnFatal(err)
		return out
	}
	for rows.Next() {
		var repo string
		var n int
		if err := rows.Scan(&repo, &n); err != nil {
			_ = rows.Close()
			panicOnFatal(err)
			return out
		}
		st, ok := out[repo]
		if !ok {
			st = graph.GraphStats{ByKind: map[string]int{}, ByLanguage: map[string]int{}}
		}
		st.TotalEdges = n
		out[repo] = st
	}
	_ = rows.Close()
	return out
}

func (s *Store) RepoPrefixes() []string {
	rows, err := s.stmtRepoPrefixes.Query()
	if err != nil {
		panicOnFatal(err)
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			panicOnFatal(err)
			return out
		}
		out = append(out, p)
	}
	return out
}

// -- provenance verification ---------------------------------------------

func (s *Store) EdgeIdentityRevisions() int {
	return int(s.edgeIdentityRevs.Load())
}

// VerifyEdgeIdentities is a no-op for the SQL backend: the in-memory
// store's invariant is "the same *Edge pointer lives in both
// adjacency views". The SQL store has a single row per edge, so the
// invariant is trivially satisfied -- no walk can find a divergence
// to report.
func (s *Store) VerifyEdgeIdentities() error { return nil }

// -- memory estimation (advisory) ----------------------------------------

// perRowByteEstimate is a deliberately rough per-row byte cost --
// the disk backend doesn't have an in-memory footprint to report, so
// the contract (per Store interface comment) is "return what you can
// compute and callers treat the result as advisory". The conformance
// test only checks NodeCount.
const (
	perNodeByteEstimate = 256
	perEdgeByteEstimate = 128
)

func (s *Store) RepoMemoryEstimate(repoPrefix string) graph.RepoMemoryEstimate {
	var est graph.RepoMemoryEstimate
	var n, e int
	if err := s.stmtRepoNodeCount.QueryRow(repoPrefix).Scan(&n); err != nil {
		panicOnFatal(err)
		return est
	}
	if err := s.stmtRepoEdgeCount.QueryRow(repoPrefix).Scan(&e); err != nil {
		panicOnFatal(err)
		return est
	}
	est.NodeCount = n
	est.EdgeCount = e
	est.NodeBytes = uint64(n) * perNodeByteEstimate
	est.EdgeBytes = uint64(e) * perEdgeByteEstimate
	return est
}

func (s *Store) AllRepoMemoryEstimates() map[string]graph.RepoMemoryEstimate {
	out := map[string]graph.RepoMemoryEstimate{}
	rows, err := s.stmtAllRepoCountsNodes.Query()
	if err != nil {
		panicOnFatal(err)
		return out
	}
	for rows.Next() {
		var repo string
		var n int
		if err := rows.Scan(&repo, &n); err != nil {
			_ = rows.Close()
			panicOnFatal(err)
			return out
		}
		est := out[repo]
		est.NodeCount = n
		est.NodeBytes = uint64(n) * perNodeByteEstimate
		out[repo] = est
	}
	_ = rows.Close()

	rows, err = s.stmtAllRepoCountsEdges.Query()
	if err != nil {
		panicOnFatal(err)
		return out
	}
	for rows.Next() {
		var repo string
		var n int
		if err := rows.Scan(&repo, &n); err != nil {
			_ = rows.Close()
			panicOnFatal(err)
			return out
		}
		est := out[repo]
		est.EdgeCount = n
		est.EdgeBytes = uint64(n) * perEdgeByteEstimate
		out[repo] = est
	}
	_ = rows.Close()
	return out
}

// -- helpers --------------------------------------------------------------

// panicOnFatal turns truly catastrophic SQLite errors (closed DB,
// schema mismatch, disk-full at insert time) into a panic so callers
// see them, while letting expected sql.ErrNoRows / busy / no-affected
// callers stay quiet. The graph.Store interface deliberately does not
// surface errors -- it mirrors the in-memory store's "everything
// succeeds" contract -- so a fatal storage failure cannot be ignored.
func panicOnFatal(err error) {
	if err == nil {
		return
	}
	if errors.Is(err, sql.ErrNoRows) {
		return
	}
	// A closed statement / database / connection is a teardown race, not
	// data corruption: Close() shuts the store (daemon shutdown, restart,
	// or store swap) while an in-flight reader -- e.g. a deferred
	// parallel-enrich goroutine still holding a cached *sql.Stmt -- runs a
	// query. Crashing the whole daemon over a benign shutdown race is
	// strictly worse than the read returning empty (or a winding-down write
	// being dropped), so treat these as non-fatal.
	if errors.Is(err, sql.ErrConnDone) || isStoreClosedErr(err) {
		return
	}
	panic(fmt.Errorf("store_sqlite: %w", err))
}

// isStoreClosedErr reports whether err is the database/sql sentinel for a
// closed prepared statement or a closed database -- string-matched because
// database/sql does not export these as typed errors.
func isStoreClosedErr(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "statement is closed") ||
		strings.Contains(msg, "database is closed")
}

// -- predicate-shaped reads ---------------------------------------------
//
// Each method runs one indexed SELECT and streams rows back via the
// iter.Seq[T] yield callback. Stops cleanly when yield returns false.
// Heavier than the equivalent bolt path (sql parsing + driver row
// materialisation) but cuts the resolver's wasted full-table scans
// down to "match-only" cardinality, which is the whole point.

// All three predicate iterators here MATERIALISE the query result
// into a slice before yielding, then iterate the slice. This avoids
// a deadlock peculiar to the SQLite backend's single-connection
// pool: a streaming rows-cursor holds THE connection, and any
// callback in the yield body that re-enters the store (e.g. GetNode
// to resolve an edge's caller) blocks forever waiting on the same
// connection. Materialise-then-yield releases the connection before
// the body runs, so re-entrant store calls work.
//
// The "predicate-shaped" win still holds: the indexed SELECT only
// fetches matching rows, not the whole table. We give up streaming
// memory savings (we still build a Go slice of *Edge / *Node) but
// keep the structural advantage that the row count flowing through
// scanEdge is proportional to the result, not the table.

// EdgesByKind: indexed SELECT on the (kind) column.
func (s *Store) EdgesByKind(kind graph.EdgeKind) iter.Seq[*graph.Edge] {
	return func(yield func(*graph.Edge) bool) {
		out := s.queryEdgesSQL(`
SELECT from_id, to_id, kind, file_path, line, confidence, confidence_label, origin, tier, cross_repo, meta
FROM edges WHERE kind = ?`, string(kind))
		for _, e := range out {
			if !yield(e) {
				return
			}
		}
	}
}

// NodesByKind: indexed SELECT on the (kind) column.
func (s *Store) NodesByKind(kind graph.NodeKind) iter.Seq[*graph.Node] {
	return func(yield func(*graph.Node) bool) {
		out := s.queryNodesSQL(`SELECT `+lookupNodeCols+` FROM nodes WHERE kind = ?`, string(kind))
		for _, n := range out {
			if !yield(n) {
				return
			}
		}
	}
}

// EdgesWithUnresolvedTarget yields edges whose target is an unresolved
// stub in EITHER form: the bare `unresolved::X` (a half-open range scan
// that seeks directly to the contiguous slice via the to_id b-tree) or
// the multi-repo `<repo>::unresolved::X` rewrite (an infix LIKE — the
// unresolved set is small, so the scan is cheap). Mirrors
// graph.IsUnresolvedTarget over both shapes.
func (s *Store) EdgesWithUnresolvedTarget() iter.Seq[*graph.Edge] {
	return func(yield func(*graph.Edge) bool) {
		out := s.queryEdgesSQL(`
SELECT from_id, to_id, kind, file_path, line, confidence, confidence_label, origin, tier, cross_repo, meta
FROM edges WHERE (to_id >= 'unresolved::' AND to_id < 'unresolved:;') OR to_id LIKE '%::unresolved::%'`)
		for _, e := range out {
			if !yield(e) {
				return
			}
		}
	}
}

// queryEdgesSQL runs an edge-shaped SELECT, materialises the rows
// into a slice, and closes the rows-cursor before returning —
// releasing the underlying sql.Conn so the predicate-iterator's
// callback body is free to make re-entrant store calls without
// deadlocking on the MaxOpenConns=1 pool. Companion to the existing
// queryEdges helper that takes a *sql.Stmt; this one takes a raw
// SQL string so the predicate iterators can pass inline queries.
func (s *Store) queryEdgesSQL(q string, args ...any) []*graph.Edge {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []*graph.Edge
	for rows.Next() {
		e, err := scanEdge(rows)
		if err != nil || e == nil {
			continue
		}
		out = append(out, e)
	}
	return out
}

// queryNodesSQL is the node-shaped sibling of queryEdgesSQL.
func (s *Store) queryNodesSQL(q string, args ...any) []*graph.Node {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []*graph.Node
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil || n == nil {
			continue
		}
		out = append(out, n)
	}
	return out
}

// lookupChunkSize bounds the IN-list parameter count per SQL query.
// SQLite's default SQLITE_MAX_VARIABLE_NUMBER is 32766 in modern
// builds, but staying well under that keeps query plans stable and
// avoids surprising the parser on monster lists.
const lookupChunkSize = 5000

// GetNodesByIDs collapses N per-id SELECTs into ⌈N/chunk⌉ queries
// of the form `SELECT … FROM nodes WHERE id IN (?, ?, …)`. The
// resolver fires hundreds of thousands of these on a large pass;
// chunking turns hundreds of seconds into single-digit seconds.
func (s *Store) GetNodesByIDs(ids []string) map[string]*graph.Node {
	if len(ids) == 0 {
		return nil
	}
	// Dedupe + skip empty up front to keep the chunk loop honest.
	seen := make(map[string]struct{}, len(ids))
	uniq := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		uniq = append(uniq, id)
	}
	out := make(map[string]*graph.Node, len(uniq))
	const nodeCols = lookupNodeCols
	for i := 0; i < len(uniq); i += lookupChunkSize {
		end := minInt(i+lookupChunkSize, len(uniq))
		chunk := uniq[i:end]
		placeholders := strings.Repeat(",?", len(chunk))[1:]
		q := `SELECT ` + nodeCols + ` FROM nodes WHERE id IN (` + placeholders + `)`
		args := make([]any, len(chunk))
		for j, id := range chunk {
			args[j] = id
		}
		for _, n := range s.queryNodesSQL(q, args...) {
			if n != nil {
				out[n.ID] = n
			}
		}
	}
	return out
}

// FindNodesByNames collapses N per-name FindNodesByName queries into
// one `SELECT … FROM nodes WHERE name IN (…)` plus an in-Go bucket
// by name. The (name) index makes the SELECT seek-driven, and the
// caller sees the same map[name][]*Node it would have built by
// calling FindNodesByName N times.
func (s *Store) FindNodesByNames(names []string) map[string][]*graph.Node {
	if len(names) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(names))
	uniq := make([]string, 0, len(names))
	for _, name := range names {
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		uniq = append(uniq, name)
	}
	out := make(map[string][]*graph.Node, len(uniq))
	const nodeCols = lookupNodeCols
	for i := 0; i < len(uniq); i += lookupChunkSize {
		end := minInt(i+lookupChunkSize, len(uniq))
		chunk := uniq[i:end]
		placeholders := strings.Repeat(",?", len(chunk))[1:]
		q := `SELECT ` + nodeCols + ` FROM nodes WHERE name IN (` + placeholders + `)`
		args := make([]any, len(chunk))
		for j, name := range chunk {
			args[j] = name
		}
		for _, n := range s.queryNodesSQL(q, args...) {
			if n == nil {
				continue
			}
			out[n.Name] = append(out[n.Name], n)
		}
	}
	return out
}

// -- BulkLoader implementation -------------------------------------------

// Compile-time assertion: *Store satisfies graph.BulkLoader. The
// sqlite AddBatch path already runs inside one transaction per
// chunk and the resolver's batched mutators (ReindexEdges,
// SetEdgeProvenanceBatch) are already amortised. The BulkLoad
// bracket is marker-only here: it exists so the indexer's
// in-memory shadow swap activates — the resolver and its
// post-resolve passes then run against an in-memory *Graph at
// nanosecond latency, and the final AddBatch dumps the resolved
// graph to sqlite in one shot.
var _ graph.BulkLoader = (*Store)(nil)

// BeginBulkLoad enters bulk mode. No-op for sqlite.
func (s *Store) BeginBulkLoad() {}

// FlushBulk exits bulk mode. No-op for sqlite.
func (s *Store) FlushBulk() error { return nil }
