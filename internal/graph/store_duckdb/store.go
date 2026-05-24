// Package store_duckdb is the on-disk, DuckDB-backed implementation of
// graph.Store. DuckDB is an embedded columnar OLAP engine; its
// query-planner exploits the secondary indexes the schema declares,
// and the native Appender API turns bulk inserts (AddBatch) into the
// columnar-friendly fast path.
//
// Hot queries are precompiled as prepared statements in Open and
// closed in Close. Writes serialize through a single Go-side mutex
// because the conformance suite fans out 8 concurrent writers and the
// DuckDB Appender / DELETE-then-INSERT idempotency paths need a
// stable single-writer view; reads still run concurrently across the
// pool's NumCPU connections (DuckDB supports concurrent readers
// natively).
//
// Meta maps are encoded with gob; an empty / nil Meta is stored as
// NULL so the common case adds no row weight beyond the column header.
//
// EdgeIdentityRevisions is tracked in memory (atomic counter) -- it
// mirrors the in-memory store's monotonic "provenance churn" signal
// and does not need to survive process restarts (the in-memory store
// resets it on every New(), so the contract is per-process).
//
// DuckDB quirks worth knowing:
//   - No AUTOINCREMENT. edge_id is allocated by a Go-side atomic
//     counter, seeded from MAX(edge_id) at Open so re-opening an
//     existing DB doesn't collide.
//   - No INSERT OR REPLACE / OR IGNORE in the SQLite dialect. AddNode
//     emulates last-write-wins via DELETE+INSERT under writeMu, and
//     AddEdge / Appender paths pre-delete colliding logical rows
//     (from_id,to_id,kind,file_path,line) so the re-add is a no-op.
package store_duckdb

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/gob"
	"errors"
	"fmt"
	"iter"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/zzet/gortex/internal/graph"

	duckdb "github.com/marcboeker/go-duckdb/v2"
)

// Store is the DuckDB-backed graph.Store implementation.
type Store struct {
	db *sql.DB
	// connector is the *duckdb.Connector we registered the *sql.DB
	// against. Holding the pointer lets AddBatch lease a raw
	// *duckdb.Conn for the Appender API without re-opening the file.
	connector *duckdb.Connector

	// writeMu serialises every mutation. DuckDB serialises writers
	// internally too, but doing the same on the Go side keeps the
	// DELETE-then-INSERT idempotency paths and the Appender API path
	// stable under the conformance suite's 8-goroutine concurrency
	// test.
	writeMu sync.Mutex

	// resolveMu is the resolver-coordination mutex returned by
	// ResolveMutex. Held by cross-repo / temporal / external resolver
	// passes to keep their edge mutations from interleaving. Separate
	// from writeMu so the resolver can hold it across multiple writes
	// without blocking unrelated steady-state mutations.
	resolveMu sync.Mutex

	edgeIdentityRevs atomic.Int64
	// nextEdgeID is the Go-side autoincrement for edges.edge_id.
	// Seeded from MAX(edge_id) on Open. All mutation paths (AddEdge,
	// AddBatch, ReindexEdge, ReindexEdges) bump it before inserting.
	nextEdgeID atomic.Int64

	// Prepared statements (compiled once in Open, closed in Close).
	//
	// We deliberately do NOT pre-prepare any aggregate / GROUP BY /
	// DISTINCT query: duckdb-go-bindings v0.1.21 caches a query plan
	// at Prepare time, and a statement prepared against an empty
	// table returns mangled (single-character) string columns when
	// later re-executed against populated data. The aggregate methods
	// (Stats, RepoStats, RepoPrefixes, RepoNodeCount / RepoEdgeCount,
	// AllRepo*) run inline via s.db.Query instead.
	stmtInsertNode       *sql.Stmt
	stmtDeleteNode       *sql.Stmt
	stmtGetNode          *sql.Stmt
	stmtGetNodeByQual    *sql.Stmt
	stmtFindByName       *sql.Stmt
	stmtFindByNameInRepo *sql.Stmt
	stmtFileNodes        *sql.Stmt
	stmtRepoNodes        *sql.Stmt
	stmtAllNodes         *sql.Stmt
	stmtNodeCount        *sql.Stmt

	stmtInsertEdge        *sql.Stmt
	stmtDeleteEdgeLogical *sql.Stmt
	stmtOutEdges          *sql.Stmt
	stmtInEdges           *sql.Stmt
	stmtAllEdges          *sql.Stmt
	stmtEdgeCount         *sql.Stmt
	stmtRemoveEdge        *sql.Stmt
	stmtUpdateEdgeOrigin  *sql.Stmt
	stmtSelectEdgeOrigin  *sql.Stmt
	stmtDeleteEdgeByKey   *sql.Stmt

	stmtSelectFileNodeIDs *sql.Stmt
	stmtSelectRepoNodeIDs *sql.Stmt
	stmtDeleteNodeByFile  *sql.Stmt
	stmtDeleteNodeByRepo  *sql.Stmt

	// Bulk-load fast path (see BeginBulkLoad). When active, AddBatch
	// buffers rows in memory instead of opening an Appender per call;
	// FlushBulk dedupes the buffers and streams everything through a
	// single Appender pass — skipping the per-batch DELETE pre-pass,
	// per-batch transaction commit, and per-batch Appender open/close.
	bulkMu     sync.Mutex
	bulkActive bool
	bulkNodes  []*graph.Node
	bulkEdges  []*graph.Edge
}

// Compile-time assertion: *Store satisfies graph.Store.
var _ graph.Store = (*Store)(nil)

// ResolveMutex returns the resolver-coordination mutex.
func (s *Store) ResolveMutex() *sync.Mutex { return &s.resolveMu }

// Open opens (or creates) the DuckDB database at path, runs the schema
// migration, and prepares hot statements.
//
// Pass "" or ":memory:" for an ephemeral in-process database.
func Open(path string) (*Store, error) {
	connectorPath := path
	if connectorPath == ":memory:" {
		connectorPath = ""
	}
	connector, err := duckdb.NewConnector(connectorPath, nil)
	if err != nil {
		return nil, fmt.Errorf("duckdb connector: %w", err)
	}
	db := sql.OpenDB(connector)
	// Pool up to NumCPU connections so the resolver's parallel
	// worker fan-out doesn't serialise through a single connection.
	// DuckDB natively supports concurrent readers across multiple
	// connections; writes still serialise via writeMu on the Go
	// side.
	db.SetMaxOpenConns(runtime.NumCPU())

	if _, err := db.Exec(schemaSQL); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("duckdb schema: %w", err)
	}

	s := &Store{db: db, connector: connector}
	if err := s.prepare(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("duckdb prepare: %w", err)
	}
	// Seed the edge-id allocator from MAX(edge_id) so re-opening an
	// existing database doesn't collide with rows already on disk.
	var maxID sql.NullInt64
	if err := db.QueryRow(`SELECT MAX(edge_id) FROM edges`).Scan(&maxID); err != nil {
		_ = s.Close()
		return nil, fmt.Errorf("duckdb seed edge_id: %w", err)
	}
	if maxID.Valid {
		s.nextEdgeID.Store(maxID.Int64)
	}
	return s, nil
}

// Close closes every prepared statement and the underlying *sql.DB.
func (s *Store) Close() error {
	stmts := []*sql.Stmt{
		s.stmtInsertNode, s.stmtDeleteNode, s.stmtGetNode, s.stmtGetNodeByQual,
		s.stmtFindByName, s.stmtFindByNameInRepo,
		s.stmtFileNodes, s.stmtRepoNodes,
		s.stmtAllNodes, s.stmtNodeCount,
		s.stmtInsertEdge, s.stmtDeleteEdgeLogical,
		s.stmtOutEdges, s.stmtInEdges,
		s.stmtAllEdges, s.stmtEdgeCount, s.stmtRemoveEdge,
		s.stmtUpdateEdgeOrigin, s.stmtSelectEdgeOrigin, s.stmtDeleteEdgeByKey,
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

	const nodeCols = `id, kind, name, qual_name, file_path, start_line, end_line, language, repo_prefix, workspace_id, project_id, absolute_file_path, meta`

	prep(&s.stmtInsertNode,
		`INSERT INTO nodes (`+nodeCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	prep(&s.stmtDeleteNode,
		`DELETE FROM nodes WHERE id = ?`)
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
	// NOTE: RepoPrefixes / RepoStats / RepoNodeCount / RepoEdgeCount /
	// AllRepo* / StatsByKind / StatsByLanguage all run inline via
	// s.db.Query. See the comment on the Store struct for the
	// duckdb-go-bindings prepared-aggregate bug.

	const edgeColsNoID = `from_id, to_id, kind, file_path, line, confidence, confidence_label, origin, tier, cross_repo, meta`
	const edgeColsWithID = `edge_id, ` + edgeColsNoID

	prep(&s.stmtInsertEdge,
		`INSERT INTO edges (`+edgeColsWithID+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`)
	prep(&s.stmtDeleteEdgeLogical,
		`DELETE FROM edges WHERE from_id = ? AND to_id = ? AND kind = ? AND file_path = ? AND line = ?`)
	prep(&s.stmtOutEdges,
		`SELECT `+edgeColsNoID+` FROM edges WHERE from_id = ?`)
	prep(&s.stmtInEdges,
		`SELECT `+edgeColsNoID+` FROM edges WHERE to_id = ?`)
	prep(&s.stmtAllEdges,
		`SELECT `+edgeColsNoID+` FROM edges`)
	prep(&s.stmtEdgeCount,
		`SELECT COUNT(*) FROM edges`)
	prep(&s.stmtRemoveEdge,
		`DELETE FROM edges WHERE from_id = ? AND to_id = ? AND kind = ?`)

	prep(&s.stmtSelectEdgeOrigin,
		`SELECT origin FROM edges WHERE from_id = ? AND to_id = ? AND kind = ? AND file_path = ? AND line = ?`)
	prep(&s.stmtUpdateEdgeOrigin,
		`UPDATE edges SET origin = ?, tier = ? WHERE from_id = ? AND to_id = ? AND kind = ? AND file_path = ? AND line = ?`)
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

// -- meta encode/decode ----------------------------------------------------

func encodeMeta(m map[string]any) ([]byte, error) {
	if len(m) == 0 {
		return nil, nil
	}
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(m); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func decodeMeta(b []byte) (map[string]any, error) {
	if len(b) == 0 {
		return nil, nil
	}
	var m map[string]any
	if err := gob.NewDecoder(bytes.NewReader(b)).Decode(&m); err != nil {
		return nil, err
	}
	return m, nil
}

// -- row scanners ---------------------------------------------------------

func scanNode(scanner interface {
	Scan(...any) error
}) (*graph.Node, error) {
	var (
		n        graph.Node
		metaBlob []byte
	)
	err := scanner.Scan(
		&n.ID, &n.Kind, &n.Name, &n.QualName, &n.FilePath,
		&n.StartLine, &n.EndLine, &n.Language,
		&n.RepoPrefix, &n.WorkspaceID, &n.ProjectID, &n.AbsoluteFilePath,
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
	return &n, nil
}

func scanEdge(scanner interface {
	Scan(...any) error
}) (*graph.Edge, error) {
	var (
		e         graph.Edge
		metaBlob  []byte
		crossRepo bool
	)
	err := scanner.Scan(
		&e.From, &e.To, &e.Kind, &e.FilePath, &e.Line,
		&e.Confidence, &e.ConfidenceLabel, &e.Origin, &e.Tier,
		&crossRepo, &metaBlob,
	)
	if err != nil {
		return nil, err
	}
	e.CrossRepo = crossRepo
	if len(metaBlob) > 0 {
		m, derr := decodeMeta(metaBlob)
		if derr != nil {
			return nil, derr
		}
		e.Meta = m
	}
	return &e, nil
}

// -- writes ---------------------------------------------------------------

// AddNode inserts or replaces a node. Idempotent on the id column --
// re-adding the same id with new content does a last-write-wins
// update, matching the in-memory store's behaviour. DuckDB doesn't
// support INSERT OR REPLACE, so we emulate it with DELETE+INSERT
// under writeMu.
func (s *Store) AddNode(n *graph.Node) {
	if n == nil || n.ID == "" {
		return
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.replaceNodeLocked(s.stmtDeleteNode, s.stmtInsertNode, n); err != nil {
		panicOnFatal(err)
	}
}

func (s *Store) replaceNodeLocked(delStmt, insStmt *sql.Stmt, n *graph.Node) error {
	if _, err := delStmt.Exec(n.ID); err != nil {
		return err
	}
	return s.insertNodeLocked(insStmt, n)
}

func (s *Store) insertNodeLocked(stmt *sql.Stmt, n *graph.Node) error {
	metaBlob, err := encodeMeta(n.Meta)
	if err != nil {
		return err
	}
	_, err = stmt.Exec(
		n.ID, string(n.Kind), n.Name, n.QualName, n.FilePath,
		n.StartLine, n.EndLine, n.Language,
		n.RepoPrefix, n.WorkspaceID, n.ProjectID, n.AbsoluteFilePath,
		metaBlob,
	)
	return err
}

// AddEdge inserts an edge. Idempotent on the logical edge key (from,
// to, kind, file_path, line) -- a second AddEdge with the same key
// is a no-op (DELETE-then-INSERT under writeMu, equivalent to
// SQLite's INSERT OR IGNORE for this column set).
func (s *Store) AddEdge(e *graph.Edge) {
	if e == nil {
		return
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.replaceEdgeLocked(s.stmtDeleteEdgeLogical, s.stmtInsertEdge, e); err != nil {
		panicOnFatal(err)
	}
}

func (s *Store) replaceEdgeLocked(delStmt, insStmt *sql.Stmt, e *graph.Edge) error {
	if _, err := delStmt.Exec(e.From, e.To, string(e.Kind), e.FilePath, e.Line); err != nil {
		return err
	}
	return s.insertEdgeLocked(insStmt, e)
}

func (s *Store) insertEdgeLocked(stmt *sql.Stmt, e *graph.Edge) error {
	metaBlob, err := encodeMeta(e.Meta)
	if err != nil {
		return err
	}
	id := s.nextEdgeID.Add(1)
	_, err = stmt.Exec(
		id,
		e.From, e.To, string(e.Kind), e.FilePath, e.Line,
		e.Confidence, e.ConfidenceLabel, e.Origin, e.Tier,
		e.CrossRepo, metaBlob,
	)
	return err
}

// AddBatch inserts nodes and edges using DuckDB's native Appender
// API for the columnar bulk path. The Appender is multiple-orders-
// of-magnitude faster than per-row INSERTs at AddBatch's scale (10k+
// rows per call during indexing). Pre-deletes any colliding rows so
// the post-condition matches the per-row AddNode / AddEdge
// idempotency contract.
func (s *Store) AddBatch(nodes []*graph.Node, edges []*graph.Edge) {
	if len(nodes) == 0 && len(edges) == 0 {
		return
	}
	// Bulk-load fast path: buffer in memory, defer Appender to
	// FlushBulk. The buffer lock is held briefly only across the slice
	// append — the indexer's parse workers can hammer AddBatch in
	// parallel with minimal contention.
	s.bulkMu.Lock()
	if s.bulkActive {
		s.bulkNodes = append(s.bulkNodes, nodes...)
		s.bulkEdges = append(s.bulkEdges, edges...)
		s.bulkMu.Unlock()
		return
	}
	s.bulkMu.Unlock()

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	// Pre-filter the inputs so the Appender path only sees rows we
	// actually intend to insert, and pre-delete every colliding key
	// so the appended rows don't violate the UNIQUE constraints.
	//
	// Also dedupe WITHIN the input slice: the indexer's per-file
	// AddBatch frequently includes the same node ID multiple times
	// when a file declares the same identifier in different scopes
	// (e.g. a `buf` local variable in several functions inside the
	// same file). The pre-delete handles cross-batch dups; this
	// dedupes within-batch so the Appender doesn't trip its own
	// uniqueness check. Last-write-wins matches the per-row AddNode
	// semantics (INSERT OR REPLACE).
	seenNodeIDs := make(map[string]int, len(nodes)) // id → index in validNodes
	validNodes := make([]*graph.Node, 0, len(nodes))
	for _, n := range nodes {
		if n == nil || n.ID == "" {
			continue
		}
		if idx, ok := seenNodeIDs[n.ID]; ok {
			validNodes[idx] = n // last-write-wins
			continue
		}
		seenNodeIDs[n.ID] = len(validNodes)
		validNodes = append(validNodes, n)
	}
	type edgeKey struct {
		from, to, kind, file string
		line                 int
	}
	seenEdgeKeys := make(map[edgeKey]int, len(edges))
	validEdges := make([]*graph.Edge, 0, len(edges))
	for _, e := range edges {
		if e == nil {
			continue
		}
		k := edgeKey{e.From, e.To, string(e.Kind), e.FilePath, e.Line}
		if idx, ok := seenEdgeKeys[k]; ok {
			validEdges[idx] = e // last-write-wins on (from,to,kind,file,line)
			continue
		}
		seenEdgeKeys[k] = len(validEdges)
		validEdges = append(validEdges, e)
	}
	if len(validNodes) == 0 && len(validEdges) == 0 {
		return
	}

	// Pre-delete every key the appender is about to touch. We chunk
	// the deletes so a 50k-row batch doesn't bind a 50k-element IN
	// list (DuckDB handles it but the explicit chunk keeps the plan
	// predictable). Deletes go through a single transaction.
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
	for _, n := range validNodes {
		if _, err := tx.Stmt(s.stmtDeleteNode).Exec(n.ID); err != nil {
			panicOnFatal(err)
			return
		}
	}
	for _, e := range validEdges {
		if _, err := tx.Stmt(s.stmtDeleteEdgeLogical).Exec(e.From, e.To, string(e.Kind), e.FilePath, e.Line); err != nil {
			panicOnFatal(err)
			return
		}
	}
	if err := tx.Commit(); err != nil {
		panicOnFatal(err)
		return
	}
	commit = true

	// Lease a raw *duckdb.Conn for the Appender API and stream the
	// validated rows through it. The Appender is the columnar fast
	// path -- it batches rows into a data chunk and flushes at
	// chunk-capacity boundaries, sidestepping per-row INSERT
	// overhead entirely.
	if err := s.appendNodesAndEdges(validNodes, validEdges); err != nil {
		panicOnFatal(err)
		return
	}
}

// appendNodesAndEdges leases a dedicated raw duckdb.Conn and streams
// the supplied rows through two Appender instances (one per table).
// Held under writeMu by the caller.
func (s *Store) appendNodesAndEdges(nodes []*graph.Node, edges []*graph.Edge) error {
	conn, err := s.db.Conn(context.Background())
	if err != nil {
		return err
	}
	defer conn.Close()

	return conn.Raw(func(driverConn any) error {
		dc, ok := driverConn.(driver.Conn)
		if !ok {
			return fmt.Errorf("driver conn type %T is not driver.Conn", driverConn)
		}

		if len(nodes) > 0 {
			app, aerr := duckdb.NewAppenderFromConn(dc, "", "nodes")
			if aerr != nil {
				return fmt.Errorf("nodes appender: %w", aerr)
			}
			for _, n := range nodes {
				metaBlob, merr := encodeMeta(n.Meta)
				if merr != nil {
					_ = app.Close()
					return merr
				}
				// Appender wants concrete driver.Value types. The
				// nodes table has 13 columns; align with nodeCols.
				if err := app.AppendRow(
					n.ID, string(n.Kind), n.Name, n.QualName, n.FilePath,
					int32(n.StartLine), int32(n.EndLine), n.Language,
					n.RepoPrefix, n.WorkspaceID, n.ProjectID, n.AbsoluteFilePath,
					metaBlob,
				); err != nil {
					_ = app.Close()
					return fmt.Errorf("nodes appender append: %w", err)
				}
			}
			if cerr := app.Close(); cerr != nil {
				return fmt.Errorf("nodes appender close: %w", cerr)
			}
		}

		if len(edges) > 0 {
			app, aerr := duckdb.NewAppenderFromConn(dc, "", "edges")
			if aerr != nil {
				return fmt.Errorf("edges appender: %w", aerr)
			}
			for _, e := range edges {
				metaBlob, merr := encodeMeta(e.Meta)
				if merr != nil {
					_ = app.Close()
					return merr
				}
				id := s.nextEdgeID.Add(1)
				if err := app.AppendRow(
					id,
					e.From, e.To, string(e.Kind), e.FilePath, int32(e.Line),
					e.Confidence, e.ConfidenceLabel, e.Origin, e.Tier,
					e.CrossRepo, metaBlob,
				); err != nil {
					_ = app.Close()
					return fmt.Errorf("edges appender append: %w", err)
				}
			}
			if cerr := app.Close(); cerr != nil {
				return fmt.Errorf("edges appender close: %w", cerr)
			}
		}
		return nil
	})
}

// SetEdgeProvenance mutates an existing edge's origin in-place and
// bumps the identity-revision counter when the origin actually
// changes. Returns true iff a change was applied.
func (s *Store) SetEdgeProvenance(e *graph.Edge, newOrigin string) bool {
	if e == nil {
		return false
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

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
	e.Origin = newOrigin
	if e.Tier != "" {
		e.Tier = newTier
	}
	s.edgeIdentityRevs.Add(1)
	return true
}

// ReindexEdge updates the stored row after e.To has been mutated from
// oldTo to e.To. Implemented as delete-old + insert-new under the
// same write lock.
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
	if err := s.replaceEdgeLocked(s.stmtDeleteEdgeLogical, s.stmtInsertEdge, e); err != nil {
		panicOnFatal(err)
		return
	}
}

// reindexChunkSize bounds the number of edge re-binds per BEGIN/COMMIT.
const reindexChunkSize = 5000

// ReindexEdges chunks the batch into reindexChunkSize-mutation
// transactions and runs each through prepared statements re-used
// across the chunk.
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
		delByKeyStmt := tx.Stmt(s.stmtDeleteEdgeByKey)
		delLogicalStmt := tx.Stmt(s.stmtDeleteEdgeLogical)
		insStmt := tx.Stmt(s.stmtInsertEdge)
		for _, r := range chunk {
			if r.Edge == nil || r.OldTo == r.Edge.To {
				continue
			}
			if _, err := delByKeyStmt.Exec(r.Edge.From, r.OldTo, string(r.Edge.Kind), r.Edge.FilePath, r.Edge.Line); err != nil {
				_ = tx.Rollback()
				panicOnFatal(err)
				return
			}
			if _, err := delLogicalStmt.Exec(r.Edge.From, r.Edge.To, string(r.Edge.Kind), r.Edge.FilePath, r.Edge.Line); err != nil {
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
// per actual change. Returns the total number of edges whose Origin
// changed.
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
// that touches one of those nodes.
func (s *Store) EvictFile(filePath string) (nodesRemoved, edgesRemoved int) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.evictByScopeLocked(s.stmtSelectFileNodeIDs, s.stmtDeleteNodeByFile, filePath)
}

// EvictRepo removes every node in repoPrefix and every edge that
// touches one.
func (s *Store) EvictRepo(repoPrefix string) (nodesRemoved, edgesRemoved int) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.evictByScopeLocked(s.stmtSelectRepoNodeIDs, s.stmtDeleteNodeByRepo, repoPrefix)
}

// evictByScopeLocked is the shared body of EvictFile / EvictRepo.
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
			rows.Close()
			panicOnFatal(err)
			return 0, 0
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		panicOnFatal(err)
		return 0, 0
	}
	rows.Close()
	if len(ids) == 0 {
		return 0, 0
	}

	// Delete every edge touching one of these nodes in one chunked
	// IN-list query per direction. DuckDB handles big IN lists fine.
	var edgesRemoved int
	for i := 0; i < len(ids); i += lookupChunkSize {
		end := minInt(i+lookupChunkSize, len(ids))
		chunk := ids[i:end]
		placeholders := strings.Repeat(",?", len(chunk))[1:]
		args := make([]any, len(chunk))
		for j, id := range chunk {
			args[j] = id
		}
		res, err := s.db.Exec(
			`DELETE FROM edges WHERE from_id IN (`+placeholders+`) OR to_id IN (`+placeholders+`)`,
			append(args, args...)...,
		)
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

func (s *Store) GetOutEdges(nodeID string) []*graph.Edge {
	return s.queryEdges(s.stmtOutEdges, nodeID)
}

func (s *Store) GetInEdges(nodeID string) []*graph.Edge {
	return s.queryEdges(s.stmtInEdges, nodeID)
}

func (s *Store) AllEdges() []*graph.Edge {
	return s.queryEdges(s.stmtAllEdges)
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

	// Inline (not prepared) -- see duckdb prepared-aggregate note on Store.
	rows, err := s.db.Query(`SELECT kind, COUNT(*) FROM nodes GROUP BY kind`)
	if err != nil {
		panicOnFatal(err)
		return st
	}
	for rows.Next() {
		var kind string
		var n int
		if err := rows.Scan(&kind, &n); err != nil {
			rows.Close()
			panicOnFatal(err)
			return st
		}
		st.ByKind[kind] = n
	}
	rows.Close()

	rows, err = s.db.Query(`SELECT language, COUNT(*) FROM nodes GROUP BY language`)
	if err != nil {
		panicOnFatal(err)
		return st
	}
	for rows.Next() {
		var lang string
		var n int
		if err := rows.Scan(&lang, &n); err != nil {
			rows.Close()
			panicOnFatal(err)
			return st
		}
		st.ByLanguage[lang] = n
	}
	rows.Close()
	return st
}

func (s *Store) RepoStats() map[string]graph.GraphStats {
	out := map[string]graph.GraphStats{}
	rows, err := s.db.Query(`SELECT repo_prefix, kind, language, COUNT(*) FROM nodes WHERE repo_prefix <> '' GROUP BY repo_prefix, kind, language`)
	if err != nil {
		panicOnFatal(err)
		return out
	}
	for rows.Next() {
		var repo, kind, lang string
		var n int
		if err := rows.Scan(&repo, &kind, &lang, &n); err != nil {
			rows.Close()
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
	rows.Close()

	rows, err = s.db.Query(`SELECT n.repo_prefix, COUNT(*) FROM edges e JOIN nodes n ON n.id = e.from_id WHERE n.repo_prefix <> '' GROUP BY n.repo_prefix`)
	if err != nil {
		panicOnFatal(err)
		return out
	}
	for rows.Next() {
		var repo string
		var n int
		if err := rows.Scan(&repo, &n); err != nil {
			rows.Close()
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
	rows.Close()
	return out
}

func (s *Store) RepoPrefixes() []string {
	rows, err := s.db.Query(`SELECT DISTINCT repo_prefix FROM nodes WHERE repo_prefix <> ''`)
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
// invariant is trivially satisfied.
func (s *Store) VerifyEdgeIdentities() error { return nil }

// -- memory estimation (advisory) ----------------------------------------

const (
	perNodeByteEstimate = 256
	perEdgeByteEstimate = 128
)

func (s *Store) RepoMemoryEstimate(repoPrefix string) graph.RepoMemoryEstimate {
	var est graph.RepoMemoryEstimate
	var n, e int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM nodes WHERE repo_prefix = ?`, repoPrefix).Scan(&n); err != nil {
		panicOnFatal(err)
		return est
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM edges e JOIN nodes n ON n.id = e.from_id WHERE n.repo_prefix = ?`, repoPrefix).Scan(&e); err != nil {
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
	rows, err := s.db.Query(`SELECT repo_prefix, COUNT(*) FROM nodes WHERE repo_prefix <> '' GROUP BY repo_prefix`)
	if err != nil {
		panicOnFatal(err)
		return out
	}
	for rows.Next() {
		var repo string
		var n int
		if err := rows.Scan(&repo, &n); err != nil {
			rows.Close()
			panicOnFatal(err)
			return out
		}
		est := out[repo]
		est.NodeCount = n
		est.NodeBytes = uint64(n) * perNodeByteEstimate
		out[repo] = est
	}
	rows.Close()

	rows, err = s.db.Query(`SELECT n.repo_prefix, COUNT(*) FROM edges e JOIN nodes n ON n.id = e.from_id WHERE n.repo_prefix <> '' GROUP BY n.repo_prefix`)
	if err != nil {
		panicOnFatal(err)
		return out
	}
	for rows.Next() {
		var repo string
		var n int
		if err := rows.Scan(&repo, &n); err != nil {
			rows.Close()
			panicOnFatal(err)
			return out
		}
		est := out[repo]
		est.EdgeCount = n
		est.EdgeBytes = uint64(n) * perEdgeByteEstimate
		out[repo] = est
	}
	rows.Close()
	return out
}

// -- helpers --------------------------------------------------------------

// panicOnFatal turns truly catastrophic errors into a panic so callers
// see them, while letting expected sql.ErrNoRows stay quiet. The
// graph.Store interface deliberately does not surface errors -- it
// mirrors the in-memory store's "everything succeeds" contract -- so
// a fatal storage failure cannot be ignored.
func panicOnFatal(err error) {
	if err == nil {
		return
	}
	if errors.Is(err, sql.ErrNoRows) {
		return
	}
	panic(fmt.Errorf("store_duckdb: %w", err))
}

// -- predicate-shaped reads ---------------------------------------------
//
// Each method runs one indexed SELECT and streams rows back via the
// iter.Seq[T] yield callback. We materialise the result into a slice
// before yielding (same reason as the SQLite backend: a streaming
// rows cursor pins a pool connection, which would deadlock any
// re-entrant store calls inside the yield body).

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
		out := s.queryNodesSQL(`
SELECT id, kind, name, qual_name, file_path, start_line, end_line, language,
       repo_prefix, workspace_id, project_id, absolute_file_path, meta
FROM nodes WHERE kind = ?`, string(kind))
		for _, n := range out {
			if !yield(n) {
				return
			}
		}
	}
}

// EdgesWithUnresolvedTarget: range scan on the (to_id) column using a
// half-open range. DuckDB seeks directly to the contiguous
// 'unresolved::*' slice via the to_id index.
func (s *Store) EdgesWithUnresolvedTarget() iter.Seq[*graph.Edge] {
	return func(yield func(*graph.Edge) bool) {
		out := s.queryEdgesSQL(`
SELECT from_id, to_id, kind, file_path, line, confidence, confidence_label, origin, tier, cross_repo, meta
FROM edges WHERE to_id >= 'unresolved::' AND to_id < 'unresolved:;'`)
		for _, e := range out {
			if !yield(e) {
				return
			}
		}
	}
}

// queryEdgesSQL runs an edge-shaped SELECT, materialises the rows
// into a slice, and closes the rows-cursor before returning.
func (s *Store) queryEdgesSQL(q string, args ...any) []*graph.Edge {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil
	}
	defer func() { _ = rows.Close() }()
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
	defer func() { _ = rows.Close() }()
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
const lookupChunkSize = 5000

// GetNodesByIDs collapses N per-id SELECTs into ⌈N/chunk⌉ queries
// of the form `SELECT … FROM nodes WHERE id IN (?, ?, …)`.
func (s *Store) GetNodesByIDs(ids []string) map[string]*graph.Node {
	if len(ids) == 0 {
		return nil
	}
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
	if len(uniq) == 0 {
		return nil
	}
	out := make(map[string]*graph.Node, len(uniq))
	const nodeCols = `id, kind, name, qual_name, file_path, start_line, end_line, language, repo_prefix, workspace_id, project_id, absolute_file_path, meta`
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
// by name.
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
	if len(uniq) == 0 {
		return nil
	}
	out := make(map[string][]*graph.Node, len(uniq))
	const nodeCols = `id, kind, name, qual_name, file_path, start_line, end_line, language, repo_prefix, workspace_id, project_id, absolute_file_path, meta`
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

// Compile-time assertion: *Store satisfies graph.BulkLoader.
var _ graph.BulkLoader = (*Store)(nil)

// BeginBulkLoad enters buffer-mode write. Subsequent AddBatch calls
// append into in-memory slices instead of opening an Appender per
// call. FlushBulk dedupes the buffers globally and streams everything
// through a single Appender pass — skipping the per-batch DELETE
// pre-pass (the table starts empty, so no collisions can exist),
// per-batch transaction commit, and per-batch Appender open/close.
func (s *Store) BeginBulkLoad() {
	s.bulkMu.Lock()
	defer s.bulkMu.Unlock()
	if s.bulkActive {
		panic("store_duckdb: BeginBulkLoad called twice without FlushBulk")
	}
	s.bulkActive = true
}

// FlushBulk dedupes the bulk buffers and streams everything through
// a single Appender pass per table.
func (s *Store) FlushBulk() error {
	s.bulkMu.Lock()
	if !s.bulkActive {
		s.bulkMu.Unlock()
		return fmt.Errorf("store_duckdb: FlushBulk without BeginBulkLoad")
	}
	nodes := s.bulkNodes
	edges := s.bulkEdges
	s.bulkNodes = nil
	s.bulkEdges = nil
	s.bulkActive = false
	s.bulkMu.Unlock()

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	// Dedup nodes by ID (last write wins). Mirrors the per-batch
	// within-batch dedup that AddBatch already does, just applied
	// across all buffered batches at once.
	seenNodeIDs := make(map[string]int, len(nodes))
	validNodes := make([]*graph.Node, 0, len(nodes))
	for _, n := range nodes {
		if n == nil || n.ID == "" {
			continue
		}
		if idx, ok := seenNodeIDs[n.ID]; ok {
			validNodes[idx] = n
			continue
		}
		seenNodeIDs[n.ID] = len(validNodes)
		validNodes = append(validNodes, n)
	}
	type edgeKey struct {
		from, to, kind, file string
		line                 int
	}
	seenEdgeKeys := make(map[edgeKey]int, len(edges))
	validEdges := make([]*graph.Edge, 0, len(edges))
	for _, e := range edges {
		if e == nil {
			continue
		}
		k := edgeKey{e.From, e.To, string(e.Kind), e.FilePath, e.Line}
		if idx, ok := seenEdgeKeys[k]; ok {
			validEdges[idx] = e
			continue
		}
		seenEdgeKeys[k] = len(validEdges)
		validEdges = append(validEdges, e)
	}
	if len(validNodes) == 0 && len(validEdges) == 0 {
		return nil
	}

	// When the store already has data — which is the case on every
	// chunk except the first under streaming-flush — pre-DELETE the
	// colliding rows before the Appender pass so the UNIQUE index
	// doesn't reject the second insert of an `unresolved::*` stub.
	// Empty-store case (the cold-load contract) skips the DELETE
	// because no collisions can exist yet.
	if s.nodeCountLocked() > 0 || s.edgeCountLocked() > 0 {
		if err := s.preDeleteColliders(validNodes, validEdges); err != nil {
			return fmt.Errorf("bulk pre-delete: %w", err)
		}
	}
	if err := s.appendNodesAndEdges(validNodes, validEdges); err != nil {
		return fmt.Errorf("bulk appender: %w", err)
	}
	return nil
}

// preDeleteColliders removes any row that would collide with the
// upcoming Appender pass. Held under writeMu.
func (s *Store) preDeleteColliders(nodes []*graph.Node, edges []*graph.Edge) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	commit := false
	defer func() {
		if !commit {
			_ = tx.Rollback()
		}
	}()
	for _, n := range nodes {
		if _, err := tx.Stmt(s.stmtDeleteNode).Exec(n.ID); err != nil {
			return err
		}
	}
	for _, e := range edges {
		if _, err := tx.Stmt(s.stmtDeleteEdgeLogical).Exec(e.From, e.To, string(e.Kind), e.FilePath, e.Line); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	commit = true
	return nil
}

// nodeCountLocked / edgeCountLocked are the writeMu-already-held
// variants of NodeCount / EdgeCount. They avoid the re-entrant lock
// the public methods would take.
func (s *Store) nodeCountLocked() int {
	row := s.stmtNodeCount.QueryRow()
	var n int
	_ = row.Scan(&n)
	return n
}

func (s *Store) edgeCountLocked() int {
	row := s.stmtEdgeCount.QueryRow()
	var n int
	_ = row.Scan(&n)
	return n
}

// -- BackendResolver implementation --------------------------------------

// Compile-time assertion: *Store satisfies graph.BackendResolver.
var _ graph.BackendResolver = (*Store)(nil)

// ResolveUniqueNames pushes the unique-name resolution pass into
// DuckDB as a single UPDATE...FROM. For every edge whose to_id
// matches "unresolved::Name", if exactly one Node carries that name
// in the graph, rewrite to_id to the resolved Node's id and promote
// origin/tier to ast_resolved. Ambiguous (multiple candidates) and
// unresolvable (no candidates) edges stay untouched; the Go
// resolver picks them up afterward with the language/scope rules.
//
// Two indexed CTE passes are cheaper than the per-edge round-trip
// the Go resolver would otherwise do; on a 50k-file repo this
// collapses what would be ~30k per-edge SQL UPDATEs into one
// statement.
func (s *Store) ResolveUniqueNames() (int, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	// Step 1: build a map of unique-name candidates (name -> id) using
	// HAVING count = 1 so only unambiguous names land in the lookup.
	// Step 2: update edges whose to_id matches "unresolved::<name>"
	// and whose stripped name lands in the unique-name lookup.
	//
	// edges_unique UNIQUE INDEX on (from_id, to_id, kind, file_path,
	// line) means an update that would create a duplicate identity
	// tuple is rejected — that's fine, the resolver's contract is
	// "resolve at most once per pending edge" and the prior path
	// would also fail the duplicate-key check.
	const q = `
WITH unique_names AS (
    SELECT name, MIN(id) AS id
    FROM nodes
    WHERE name <> ''
    GROUP BY name
    HAVING COUNT(*) = 1
)
UPDATE edges
SET to_id  = un.id,
    origin = 'ast_resolved',
    tier   = 'ast_resolved'
FROM unique_names un
WHERE edges.to_id LIKE 'unresolved::%'
  AND un.name = substring(edges.to_id, 13)
`
	res, err := s.db.Exec(q)
	if err != nil {
		return 0, fmt.Errorf("backend-resolver: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if n > 0 {
		s.edgeIdentityRevs.Add(n)
	}
	return int(n), nil
}
