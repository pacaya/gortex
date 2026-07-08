package store_sqlite

import "database/sql"

// isUnresolvedColumnDDL is the edges.is_unresolved generated column: a
// VIRTUAL, indexed boolean mirroring graph.IsUnresolvedTarget's two shapes
// (the bare `unresolved::Name` prefix and the multi-repo COPY-rewrite
// `<repoPrefix>::unresolved::Name` infix), computed by SQLite itself from
// to_id — no Go call site has to remember to keep it in sync. VIRTUAL, not
// STORED: SQLite refuses `ALTER TABLE ADD COLUMN ... STORED` on a non-empty
// table ("cannot add a STORED column"), which every real installed store is.
// VIRTUAL has no such restriction and is just as fast here — the read path
// always goes through the index below, and an index always stores its own
// materialised key values regardless of whether the underlying column is
// virtual or stored. Added via ensureEdgeColumns (ALTER TABLE) rather than
// baked into schemaSQL's CREATE TABLE so one code path handles both a fresh
// DB (column missing right after CREATE TABLE) and an existing one (column
// missing from before this was introduced) identically — see
// ensureNodeColumns for the same pattern on the nodes table.
//
// Measured on a real 26-repo store (2.57M edges, 847,684 unresolved, ~33%
// selectivity): replacing the OR'd `to_id` range/LIKE query with
// `is_unresolved = 1` cut EdgesWithUnresolvedTarget from 7.96s to 2.95s
// (2.7x). The prior approach of splitting the OR into two to_id-based
// queries (one indexed range, one LIKE) was WORSE (13.49s) despite a
// better-looking EXPLAIN QUERY PLAN: at ~33% selectivity the to_id index's
// matching rows are ordered by string value, so the mandatory per-row
// bookmark lookup back into the main table is effectively random I/O. The
// boolean column's matching rows are all rowid-tie-broken (identical index
// key), so its bookmark lookups land in ascending rowid order — sequential,
// not random. Same "SEARCH ... USING INDEX" in EXPLAIN QUERY PLAN either way;
// only real measurement told them apart.
const isUnresolvedColumnDDL = `is_unresolved INTEGER GENERATED ALWAYS AS (
    CASE WHEN (to_id >= 'unresolved::' AND to_id < 'unresolved:;') OR to_id LIKE '%::unresolved::%' THEN 1 ELSE 0 END
) VIRTUAL`

// edgeGeneratedColumns is the set of edges.* generated columns ensureEdgeColumns
// adds to a table created before they existed — which, since none of them are
// in schemaSQL's CREATE TABLE, includes a freshly created table too.
var edgeGeneratedColumns = []struct {
	name string
	ddl  string
}{
	{"is_unresolved", isUnresolvedColumnDDL},
}

// edgePromotedColumns lifts the resolver's resolve_terminal /
// resolve_terminal_reason Meta keys (see resolver/terminal.go) out of the
// meta blob into their own nullable columns — the edge-side sibling of
// promotedMetaColumns on nodes (see meta_json.go's "promoted edge columns"
// section for extractPromotedEdgeMeta/restorePromotedEdgeMeta and why a
// json_extract-derived generated column was tried first and abandoned:
// encodeMeta's common case is a custom flat binary codec, not JSON, so
// json_extract/json_valid against a real store's meta blobs evaluates to
// NULL for effectively every row). Plain (non-generated) columns, so they
// share ensureEdgeColumns' table_xinfo scan but are ordinary ALTER TABLE ADD
// COLUMN statements, not GENERATED ALWAYS AS expressions.
//
// Exists to let a future bulk classification query (replacing per-edge
// Go-side classifyTerminal calls in reconcileTerminalStamps) read the
// CURRENT terminal state as a plain indexed column instead of decoding
// Meta, and compare it against a freshly computed value to find only the
// edges whose state actually changed — reconcileTerminalStamps measured
// only ~1% of examined edges (9,599 of 833,828) ever change state.
var edgePromotedColumns = []struct {
	name string
	ddl  string
}{
	{"resolve_terminal", "resolve_terminal INTEGER"},
	{"resolve_terminal_reason", "resolve_terminal_reason TEXT"},
}

// ensureEdgeColumns adds edgeGeneratedColumns + edgePromotedColumns to an
// edges table created before they existed. Mirrors ensureNodeColumns'
// PRAGMA + conditional ALTER pattern, but queries table_xinfo rather than
// table_info: table_info silently OMITS generated columns from its result
// set (verified against the pinned modernc.org/sqlite driver — a reopened
// store's is_unresolved column is invisible to table_info, so the existence
// check always came back false and every reopen re-ran the ALTER, failing
// with "duplicate column name"). table_xinfo lists every column, generated
// ones included, with an extra hidden column (3 == generated) table_info
// doesn't have — and works identically for the plain promoted columns too,
// so one scan serves both lists.
func ensureEdgeColumns(db *sql.DB) error {
	rows, err := db.Query(`PRAGMA table_xinfo(edges)`)
	if err != nil {
		return err
	}
	existing := make(map[string]bool)
	for rows.Next() {
		var (
			cid, notnull, pk, hidden int
			name, ctype              string
			dflt                     sql.NullString
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk, &hidden); err != nil {
			_ = rows.Close()
			return err
		}
		existing[name] = true
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	_ = rows.Close()
	for _, c := range edgeGeneratedColumns {
		if existing[c.name] {
			continue
		}
		if _, err := db.Exec(`ALTER TABLE edges ADD COLUMN ` + c.ddl); err != nil {
			return err
		}
	}
	for _, c := range edgePromotedColumns {
		if existing[c.name] {
			continue
		}
		if _, err := db.Exec(`ALTER TABLE edges ADD COLUMN ` + c.ddl); err != nil {
			return err
		}
	}
	return nil
}

// isStubColumnDDL is nodes.is_stub: a VIRTUAL generated column mirroring
// graph.IsStub/StubKind's id-prefix logic (stdlib:: / builtin:: /
// external_call:: / module::, bare or repo-prefixed as <repo>::<kind>::...).
// Same rationale as isUnresolvedColumnDDL: computed from the existing id
// column, no Go call site has to keep it in sync. Exists so a future
// SQL-side terminal classification (see resolveTerminalColumnDDL) can check
// "is this candidate a stub" via a plain column instead of a per-row Go
// IsStub(n.ID) call.
const isStubColumnDDL = `is_stub INTEGER GENERATED ALWAYS AS (
    CASE WHEN
        id LIKE 'stdlib::%' OR id LIKE 'builtin::%' OR id LIKE 'external_call::%' OR id LIKE 'module::%'
        OR (
            instr(id, '::') > 0 AND (
                substr(id, instr(id, '::') + 2) LIKE 'stdlib::%'
                OR substr(id, instr(id, '::') + 2) LIKE 'builtin::%'
                OR substr(id, instr(id, '::') + 2) LIKE 'external_call::%'
                OR substr(id, instr(id, '::') + 2) LIKE 'module::%'
            )
        )
    THEN 1 ELSE 0 END
) VIRTUAL`

// nodeGeneratedColumns is the nodes-table sibling of edgeGeneratedColumns.
// Kept as its own list (and ensureNodeGeneratedColumns as its own function,
// rather than folded into ensureNodeColumns) because ensureNodeColumns
// checks existence via PRAGMA table_info, which — like the edges case
// documented on ensureEdgeColumns — silently omits generated columns.
// Reusing that function's table_info scan for is_stub would hit the exact
// same "always looks missing, ALTER re-runs, duplicate column name" bug.
var nodeGeneratedColumns = []struct {
	name string
	ddl  string
}{
	{"is_stub", isStubColumnDDL},
}

// ensureNodeGeneratedColumns adds nodeGeneratedColumns to a nodes table
// created before they existed. See ensureEdgeColumns for the table_xinfo
// vs table_info rationale this mirrors.
func ensureNodeGeneratedColumns(db *sql.DB) error {
	rows, err := db.Query(`PRAGMA table_xinfo(nodes)`)
	if err != nil {
		return err
	}
	existing := make(map[string]bool)
	for rows.Next() {
		var (
			cid, notnull, pk, hidden int
			name, ctype              string
			dflt                     sql.NullString
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk, &hidden); err != nil {
			_ = rows.Close()
			return err
		}
		existing[name] = true
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	_ = rows.Close()
	for _, c := range nodeGeneratedColumns {
		if existing[c.name] {
			continue
		}
		if _, err := db.Exec(`ALTER TABLE nodes ADD COLUMN ` + c.ddl); err != nil {
			return err
		}
	}
	return nil
}

// schemaSQL is the canonical DDL applied on Open. Statements are
// idempotent (IF NOT EXISTS) so they run cleanly against a fresh DB
// and against an existing one.
//
// Schema choices
//
//   - nodes.id is the primary key; INSERT OR REPLACE on the id column
//     gives idempotent re-adds with last-write-wins on every other
//     column, matching the in-memory store's behaviour.
//
//   - edges has a synthetic INTEGER PRIMARY KEY plus a UNIQUE
//     constraint over (from_id, to_id, kind, file_path, line) -- the
//     logical edge key the in-memory store uses for dedup. INSERT OR
//     IGNORE on that constraint matches the in-memory "second AddEdge
//     for the same key is a no-op" semantics.
//
//   - meta is a JSON document (see meta_json.go). nil / empty Meta is
//     stored as NULL. Four universal, hot-read node keys are promoted to
//     their own nullable columns (signature / visibility / doc /
//     external): they are stripped from the JSON blob on write and
//     restored into Meta on read, so the in-memory map is unchanged. A
//     NULL column means "not set" (legacy gob rows predate the columns
//     and keep their values in the blob). Existing databases gain the
//     columns via ALTER on the next Open (ensureNodeColumns).
//
//   - Secondary indexes mirror the in-memory store's hot lookup paths:
//     nodes_by_name      -- FindNodesByName / FindNodesByNameInRepo
//     nodes_by_kind      -- Stats (group-by-kind)
//     nodes_by_file      -- GetFileNodes, EvictFile
//     nodes_by_repo      -- GetRepoNodes, RepoStats, EvictRepo
//     (partial index -- empty repo_prefix is
//     the common case and indexing it would
//     be pure overhead)
//     nodes_by_qual      -- GetNodeByQualName, unique so duplicate
//     qual_names surface as constraint errors
//     edges_by_from      -- GetOutEdges (kind included so RemoveEdge
//     can probe by (from, kind) without a
//     second hop)
//     edges_by_to        -- GetInEdges
const schemaSQL = `
CREATE TABLE IF NOT EXISTS nodes (
    id            TEXT PRIMARY KEY,
    kind          TEXT NOT NULL,
    name          TEXT NOT NULL,
    qual_name     TEXT NOT NULL DEFAULT '',
    file_path     TEXT NOT NULL,
    start_line    INTEGER NOT NULL DEFAULT 0,
    end_line      INTEGER NOT NULL DEFAULT 0,
    start_column  INTEGER NOT NULL DEFAULT 0,
    end_column    INTEGER NOT NULL DEFAULT 0,
    language      TEXT NOT NULL DEFAULT '',
    repo_prefix   TEXT NOT NULL DEFAULT '',
    workspace_id  TEXT NOT NULL DEFAULT '',
    project_id    TEXT NOT NULL DEFAULT '',
    signature     TEXT,
    visibility    TEXT,
    doc           TEXT,
    external      INTEGER,
    return_type   TEXT,
    is_async      INTEGER,
    is_static     INTEGER,
    is_abstract   INTEGER,
    is_exported   INTEGER,
    updated_at    INTEGER,
    data_class    TEXT,
    meta          BLOB
) WITHOUT ROWID;

-- nodes_by_name / _kind / _file / _repo are created from the shared
-- bulkDroppableIndexes set (see bulk_load.go), not here, so the bulk-load
-- fast path can drop and rebuild the EXACT same DDL without drift.
-- nodes_by_qual is UNIQUE — it enforces qual_name dedup on every
-- INSERT OR REPLACE, so it is never dropped and stays defined here.
CREATE UNIQUE INDEX IF NOT EXISTS nodes_by_qual ON nodes(qual_name) WHERE qual_name <> '';

CREATE TABLE IF NOT EXISTS edges (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    from_id          TEXT NOT NULL,
    to_id            TEXT NOT NULL,
    kind             TEXT NOT NULL,
    file_path        TEXT NOT NULL DEFAULT '',
    line             INTEGER NOT NULL DEFAULT 0,
    confidence       REAL NOT NULL DEFAULT 1.0,
    confidence_label TEXT NOT NULL DEFAULT '',
    origin           TEXT NOT NULL DEFAULT '',
    tier             TEXT NOT NULL DEFAULT '',
    cross_repo       INTEGER NOT NULL DEFAULT 0,
    meta             BLOB,
    UNIQUE(from_id, to_id, kind, file_path, line)
);

-- edges_by_from / _to / _kind are created from the shared
-- bulkDroppableIndexes set (see bulk_load.go), not here, so the bulk-load
-- fast path can drop and rebuild the EXACT same DDL without drift.
-- edges_by_kind backs EdgesByKind / EdgesByKinds (resolver whole-graph
-- passes probe single kinds like provides/imports on every file save);
-- without it those are full edges-table scans — edges_by_from/to lead
-- with an id column and the partial edges_external index only covers
-- its own predicate.

CREATE TABLE IF NOT EXISTS file_mtimes (
    repo_prefix TEXT NOT NULL,
    file_path   TEXT NOT NULL,
    mtime_ns    INTEGER NOT NULL,
    PRIMARY KEY (repo_prefix, file_path)
) WITHOUT ROWID;

-- repo_index_state records per-repo freshness provenance written at the
-- end of a (re)index: the git revision + dirty flag the graph reflects,
-- the Merkle workspace fingerprint (Tree.Root) that gates global-pass
-- short-circuiting, node/edge counts for the index-plausibility baseline,
-- and the JSON per-language extractor versions that produced the graph.
-- One row per repo_prefix; WITHOUT ROWID — the PK index IS the table,
-- like file_mtimes / clone_shingles.
CREATE TABLE IF NOT EXISTS repo_index_state (
    repo_prefix        TEXT PRIMARY KEY,
    indexed_sha        TEXT NOT NULL DEFAULT '',
    dirty              INTEGER NOT NULL DEFAULT 0,
    indexed_at         INTEGER NOT NULL DEFAULT 0,
    workspace_fp       TEXT NOT NULL DEFAULT '',
    node_count         INTEGER NOT NULL DEFAULT 0,
    edge_count         INTEGER NOT NULL DEFAULT 0,
    extractor_versions TEXT NOT NULL DEFAULT ''
) WITHOUT ROWID;

-- enrichment_state records, per (repo, semantic provider), the git revision
-- the graph was enriched at plus the coverage that pass reached. Enrichment
-- completion otherwise lives only in an in-memory map, so a restart forgets it
-- and re-runs full LSP hover passes for a repo whose persisted graph already
-- carries the edges. The deferred-enrichment gate reads this row and skips a
-- provider whose IndexedSHA still matches HEAD on a clean tree. One row per
-- (repo_prefix, provider); WITHOUT ROWID — the PK index IS the table, like
-- file_mtimes / repo_index_state.
CREATE TABLE IF NOT EXISTS enrichment_state (
    repo_prefix  TEXT NOT NULL,
    provider     TEXT NOT NULL,
    indexed_sha  TEXT NOT NULL DEFAULT '',
    completed_at INTEGER NOT NULL DEFAULT 0,
    coverage     REAL NOT NULL DEFAULT 0,
    PRIMARY KEY (repo_prefix, provider)
) WITHOUT ROWID;

-- clone_shingles is the per-symbol MinHash shingle-set sidecar. Each
-- function/method node's []uint64 shingle set is stored as a little-
-- endian BLOB (8 bytes/elem) keyed by node_id so the maintained clone-
-- detection count-min sketch can be rebuilt after a warm restart from
-- the snapshot instead of re-parsing every body. repo_prefix carries
-- the owning repo so per-repo reseeds (SELECT … WHERE repo_prefix = ?)
-- and per-repo wipes don't clobber other repos' shingle sets. node_id
-- is the PK (the join key back to nodes.id); like file_mtimes this is a
-- WITHOUT ROWID sidecar so the PK index IS the table.
CREATE TABLE IF NOT EXISTS clone_shingles (
    node_id     TEXT PRIMARY KEY,
    repo_prefix TEXT NOT NULL DEFAULT '',
    shingles    BLOB
) WITHOUT ROWID;

-- constant_values is the per-KindConstant literal-value sidecar: one row
-- per constant whose RHS is a string / numeric literal, keyed by node_id
-- (the join key back to nodes.id). Lifting the value out of the JSON Meta
-- blob keeps it queryable (and out of the every-node-load decode path) so
-- the resolver can dereference a const-identifier dispatch name to its
-- value across files. file_path scopes per-file eviction on reindex;
-- repo_prefix scopes per-repo wipes. WITHOUT ROWID — the PK index IS the
-- table, like file_mtimes / clone_shingles.
CREATE TABLE IF NOT EXISTS constant_values (
    node_id     TEXT PRIMARY KEY,
    repo_prefix TEXT NOT NULL DEFAULT '',
    file_path   TEXT NOT NULL DEFAULT '',
    value       TEXT NOT NULL DEFAULT ''
) WITHOUT ROWID;

CREATE INDEX IF NOT EXISTS constant_values_by_file ON constant_values(repo_prefix, file_path);

-- files is the per-file metadata sidecar: one row per indexed file carrying
-- the BLAKE3 content hash (the Merkle leaf), byte size, extracted node count,
-- and a JSON array of parse-error locations. The Merkle tree stays the
-- authoritative change detector; this table is queryable supplementary
-- metadata (index_health reports per-file parse errors + node counts from it).
-- PK is (repo_prefix, file_path) so a reindex replaces the row in place;
-- WITHOUT ROWID — the PK index IS the table, like file_mtimes.
CREATE TABLE IF NOT EXISTS files (
    repo_prefix  TEXT NOT NULL DEFAULT '',
    file_path    TEXT NOT NULL,
    content_hash TEXT NOT NULL DEFAULT '',
    size         INTEGER NOT NULL DEFAULT 0,
    node_count   INTEGER NOT NULL DEFAULT 0,
    errors       TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (repo_prefix, file_path)
) WITHOUT ROWID;
-- files_with_errors backs the index_health "files with parse errors" rollup
-- so it scans only the (usually tiny) set of erroring files, not every row.
CREATE INDEX IF NOT EXISTS files_with_errors ON files(repo_prefix) WHERE errors <> '';

-- ref_facts is the resolved-reference sidecar: one row per reference edge
-- that resolved to a concrete target, recording the target + the provenance
-- tier that resolved it. Denormalized file_path + lang make "all reference
-- facts originating in file X" a single indexed query (the scope unit for
-- incremental re-resolution and the audit/diff surface). repo_prefix scopes
-- per-repo. PK is (repo_prefix, from_id, to_id, kind, line) so re-resolving a
-- file replaces its facts in place; WITHOUT ROWID — the PK index IS the table.
CREATE TABLE IF NOT EXISTS ref_facts (
    repo_prefix TEXT NOT NULL DEFAULT '',
    from_id     TEXT NOT NULL,
    to_id       TEXT NOT NULL,
    kind        TEXT NOT NULL,
    ref_name    TEXT NOT NULL DEFAULT '',
    line        INTEGER NOT NULL DEFAULT 0,
    origin      TEXT NOT NULL DEFAULT '',
    tier        TEXT NOT NULL DEFAULT '',
    candidates  TEXT NOT NULL DEFAULT '',
    file_path   TEXT NOT NULL DEFAULT '',
    lang        TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (repo_prefix, from_id, to_id, kind, line)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS ref_facts_by_file ON ref_facts(repo_prefix, file_path);
-- ref_facts_by_target backs the reverse lookup ("which files hold a fact
-- resolving TO these symbols") that affected-by re-resolution runs when a
-- file's symbol signatures change. Without it that query is a full
-- ref_facts scan — the PK leads with from_id, not to_id.
CREATE INDEX IF NOT EXISTS ref_facts_by_target ON ref_facts(repo_prefix, to_id);

CREATE TABLE IF NOT EXISTS vectors (
    node_id TEXT PRIMARY KEY,
    dims    INTEGER NOT NULL,
    vec     BLOB NOT NULL
) WITHOUT ROWID;

-- churn_enrichment is the per-node git-churn sidecar (change A: move
-- enrichment OUT of nodes.meta so the node hot path stops encoding
-- rarely-read data into the blob and get_churn_rate does an indexed read
-- instead of an AllNodes+meta-decode scan). One typed row per enriched
-- file/function/method node, keyed by node_id (join key back to
-- nodes.id); repo_prefix scopes
-- per-repo reseeds/wipes. head_sha/branch/computed_at are file-level only
-- (empty for symbols). WITHOUT ROWID: the PK index IS the table.
CREATE TABLE IF NOT EXISTS churn_enrichment (
    node_id        TEXT PRIMARY KEY,
    repo_prefix    TEXT NOT NULL DEFAULT '',
    commit_count   INTEGER NOT NULL DEFAULT 0,
    age_days       INTEGER NOT NULL DEFAULT 0,
    churn_rate     REAL NOT NULL DEFAULT 0,
    last_author    TEXT NOT NULL DEFAULT '',
    last_commit_at TEXT NOT NULL DEFAULT '',
    head_sha       TEXT NOT NULL DEFAULT '',
    branch         TEXT NOT NULL DEFAULT '',
    computed_at    TEXT NOT NULL DEFAULT ''
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS churn_by_repo ON churn_enrichment(repo_prefix) WHERE repo_prefix <> '';

-- coverage_enrichment: per-symbol coverage sidecar (change A). Typed
-- columns keyed by node_id; repo_prefix scopes per-repo wipes.
CREATE TABLE IF NOT EXISTS coverage_enrichment (
    node_id      TEXT PRIMARY KEY,
    repo_prefix  TEXT NOT NULL DEFAULT '',
    coverage_pct REAL NOT NULL DEFAULT 0,
    num_stmt     INTEGER NOT NULL DEFAULT 0,
    hit          INTEGER NOT NULL DEFAULT 0
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS coverage_by_repo ON coverage_enrichment(repo_prefix) WHERE repo_prefix <> '';

-- release_enrichment: per-file "added_in <tag>" sidecar (change A).
CREATE TABLE IF NOT EXISTS release_enrichment (
    node_id     TEXT PRIMARY KEY,
    repo_prefix TEXT NOT NULL DEFAULT '',
    added_in    TEXT NOT NULL DEFAULT ''
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS release_by_repo ON release_enrichment(repo_prefix) WHERE repo_prefix <> '';

-- blame_enrichment: per-symbol latest-author sidecar (change A).
CREATE TABLE IF NOT EXISTS blame_enrichment (
    node_id     TEXT PRIMARY KEY,
    repo_prefix TEXT NOT NULL DEFAULT '',
    commit_sha  TEXT NOT NULL DEFAULT '',
    email       TEXT NOT NULL DEFAULT '',
    ts          INTEGER NOT NULL DEFAULT 0
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS blame_by_repo ON blame_enrichment(repo_prefix) WHERE repo_prefix <> '';

-- symbol_fts is the FTS5 full-text index over pre-tokenised symbol
-- names. It replaces the multi-GB in-heap Bleve/BM25 index with an
-- on-disk inverted index the SymbolSearcher / SymbolBundleSearcher
-- query through. A standard (NOT contentless) FTS5 table; individual
-- rows are deleted by their FTS5 docid via the symbol_fts_rowid sidecar
-- below (node_id is UNINDEXED, so a DELETE keyed on it would full-scan
-- the index). node_id is the join key back to nodes.id; repo_prefix is
-- carried UNINDEXED so per-repo staleness wipes (DELETE … WHERE
-- repo_prefix = ?) hit a literal column without a separate b-tree.
-- Only "tokens" is indexed for matching. IF NOT EXISTS makes this
-- idempotent on every Open, so an existing .sqlite gains the vtable
-- on its next open + reindex.
CREATE VIRTUAL TABLE IF NOT EXISTS symbol_fts USING fts5(node_id UNINDEXED, repo_prefix UNINDEXED, tokens);

-- symbol_fts_rowid maps a node_id to the rowid (FTS5 docid) of its row in
-- symbol_fts. node_id is UNINDEXED in the FTS5 vtable, so deleting a node's
-- prior row with "DELETE … WHERE node_id = ?" full-scans the entire index
-- once PER symbol — quadratic on the per-edit reindex hot path. This sidecar
-- turns the delete into an O(log n) docid delete ("WHERE rowid = ?", the FTS5
-- docid IS indexed). One row per indexed symbol, keyed by node_id (the join
-- key back to nodes.id); repo_prefix scopes the per-repo wipe that
-- BulkUpsertSymbolFTS performs in lockstep with symbol_fts. WITHOUT ROWID:
-- the PK index IS the table, like file_mtimes / clone_shingles.
CREATE TABLE IF NOT EXISTS symbol_fts_rowid (
    node_id     TEXT PRIMARY KEY,
    repo_prefix TEXT NOT NULL DEFAULT '',
    fts_rowid   INTEGER NOT NULL
) WITHOUT ROWID;

-- content_fts is the FTS5 full-text index over CONTENT (data_class=
-- "content") section bodies — text / pdf / pptx / xlsx chunks. It is
-- kept SEPARATE from symbol_fts so content text never enters the symbol
-- search or the code-oriented analysis passes: a content-heavy repo of a
-- few hundred large documents explodes into hundreds of thousands of
-- section nodes, and streaming their bodies here (per file, on disk)
-- instead of into symbol_fts + graph nodes keeps the code index and the
-- graph passes bounded. Only "body" is indexed for matching; node_id /
-- repo_prefix / file_path / ordinal ride UNINDEXED so the per-repo and
-- per-file staleness wipes hit literal columns without a b-tree.
CREATE VIRTUAL TABLE IF NOT EXISTS content_fts USING fts5(node_id UNINDEXED, repo_prefix UNINDEXED, file_path UNINDEXED, ordinal UNINDEXED, body);
`
