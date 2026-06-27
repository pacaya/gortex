package store_sqlite

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
    meta          BLOB
) WITHOUT ROWID;

CREATE INDEX IF NOT EXISTS nodes_by_name ON nodes(name);
CREATE INDEX IF NOT EXISTS nodes_by_kind ON nodes(kind);
CREATE INDEX IF NOT EXISTS nodes_by_file ON nodes(file_path);
CREATE INDEX IF NOT EXISTS nodes_by_repo ON nodes(repo_prefix) WHERE repo_prefix <> '';
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

CREATE INDEX IF NOT EXISTS edges_by_from ON edges(from_id, kind);
CREATE INDEX IF NOT EXISTS edges_by_to   ON edges(to_id, kind);
-- edges_by_kind backs EdgesByKind / EdgesByKinds (resolver whole-graph
-- passes probe single kinds like provides/imports on every file save);
-- without it those are full edges-table scans — edges_by_from/to lead
-- with an id column and the partial edges_external index only covers
-- its own predicate.
CREATE INDEX IF NOT EXISTS edges_by_kind ON edges(kind);

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
