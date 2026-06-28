package languages

import (
	"maps"
	"strings"

	sitter "github.com/zzet/gortex/internal/parser/tsitter"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/sql"
)

// goSQLExecMethods is the set of Go database-driver method names
// whose first string-literal argument is treated as SQL. Covers
// database/sql (Query, Exec, QueryRow, Prepare and their *Context
// variants), sqlx (Get, Select, NamedExec, NamedQuery, MustExec),
// pgx (Query, Exec, QueryRow on a pool/conn). Other Go SQL
// libraries that share these method names land transparently —
// the heuristic is shape-driven, not import-driven, which keeps
// the detector free of per-library plumbing.
//
// Methods with name collisions outside SQL contexts (e.g. Get on a
// cache, Query on a search index) are accepted as a known false-
// positive surface — the spec recommends the gate stays default-off
// for exactly this reason. Users opt in via
// .gortex.yaml::index.coverage.sql.enabled.
var goSQLExecMethods = map[string]struct{}{
	// database/sql
	"Query":           {},
	"QueryContext":    {},
	"QueryRow":        {},
	"QueryRowContext": {},
	"Exec":            {},
	"ExecContext":     {},
	"Prepare":         {},
	"PrepareContext":  {},
	// sqlx
	"Get":           {},
	"GetContext":    {},
	"Select":        {},
	"SelectContext": {},
	"NamedExec":     {},
	"NamedQuery":    {},
	"MustExec":      {},
	// pgx
	"QueryRowContextScan": {},
}

// goSQLEvent is a deferred record of one detected SQL call site.
// Mirrors the goObservabilityEvent / goFlagEvent shape so the
// post-pass emit step can match the same patterns.
//
// query holds the raw string-literal SQL that produced the tables /
// columns slices, used to anchor a KindString node under context="sql"
// so the SQL extractor can be re-run from the string registry alone
// (short-circuit, without re-parsing source).
type goSQLEvent struct {
	method  string
	tables  []sql.TableRef
	columns []sql.ColumnRef
	query   string
	line    int
}

// detectGoSQLCall returns the table refs and the raw query extracted
// from a callm.expr capture when the method name matches the SQL exec
// set and the call's first argument is a string literal. ok=false on
// any other shape — non-SQL methods, dynamic queries, no string
// argument. The query is returned alongside the refs so the caller
// can seed a KindString context="sql" registry node for downstream
// short-circuit rebuilds without re-parsing source.
func detectGoSQLCall(callExpr *sitter.Node, method string, src []byte) ([]sql.TableRef, []sql.ColumnRef, string, bool) {
	if callExpr == nil {
		return nil, nil, "", false
	}
	if _, hit := goSQLExecMethods[method]; !hit {
		return nil, nil, "", false
	}
	args := callExpr.ChildByFieldName("arguments")
	if args == nil {
		return nil, nil, "", false
	}
	for i, _nc := 0, int(args.NamedChildCount()); i < _nc; i++ {
		c := args.NamedChild(i)
		if c == nil {
			continue
		}
		t := c.Type()
		if t != "interpreted_string_literal" && t != "raw_string_literal" {
			continue
		}
		query := strings.Trim(c.Content(src), "\"`")
		refs := sql.ExtractTables(query)
		if len(refs) == 0 {
			return nil, nil, "", false
		}
		cols := sql.ExtractColumns(query)
		return refs, cols, query, true
	}
	return nil, nil, "", false
}

// goSQLDriverDialects maps Go SQL driver import paths to the
// dialect tag stamped on KindTable nodes when the file uses them.
// The map is intentionally narrow — only widely-used drivers are
// recognised; everything else falls through to the generic
// dialect, matching the spec's "best-effort, default-off" stance
// for SQL extraction. Entries match by exact path or by prefix
// (the imports map carries the literal import path so a `pgx/v5`
// suffix still resolves to postgres via prefix-stripped lookup).
var goSQLDriverDialects = map[string]string{
	// Postgres
	"github.com/lib/pq":        "postgres",
	"github.com/jackc/pgx":     "postgres",
	"github.com/jackc/pgx/v4":  "postgres",
	"github.com/jackc/pgx/v5":  "postgres",
	"github.com/jackc/pgconn":  "postgres",
	"github.com/jackc/pgxpool": "postgres",
	// MySQL / MariaDB
	"github.com/go-sql-driver/mysql":   "mysql",
	"github.com/go-mysql-org/go-mysql": "mysql",
	// SQLite
	"github.com/mattn/go-sqlite3":   "sqlite",
	"github.com/glebarez/go-sqlite": "sqlite",
	"modernc.org/sqlite":            "sqlite",
	// SQL Server
	"github.com/microsoft/go-mssqldb":  "mssql",
	"github.com/denisenkom/go-mssqldb": "mssql",
}

// inferGoSQLDialect picks the most likely SQL dialect for a Go
// file given its import map. Returns "generic" when no driver
// import is recognised. Multiple drivers in one file is rare in
// practice; when it happens we pick the first match in iteration
// order — agents can disambiguate via the import edges if the
// dialect tag matters and is wrong.
func inferGoSQLDialect(imports map[string]string) string {
	for _, path := range imports {
		if dialect, ok := goSQLDriverDialects[path]; ok {
			return dialect
		}
		// Prefix-stripped lookup for major-version suffixes
		// (`pgx/v6`, `pgx/v7` future-proofing). Walk parent
		// directories until a match or the path runs out.
		p := path
		for {
			i := lastSegmentSlash(p)
			if i < 0 {
				break
			}
			p = p[:i]
			if dialect, ok := goSQLDriverDialects[p]; ok {
				return dialect
			}
		}
	}
	return "generic"
}

// lastSegmentSlash returns the index of the last `/` in s, or -1
// when s contains no slash. Cheaper than strings.LastIndex when
// inlined in the dialect-resolution hot loop.
func lastSegmentSlash(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '/' {
			return i
		}
	}
	return -1
}

// emitGoSQLEvents turns deferred SQL records into KindTable nodes
// plus EdgeQueries edges. Tables share IDs across files in a repo
// — the same `users` table referenced from multiple call sites
// produces a single node that every caller links to. The dialect
// argument is inferred per-file from the import map; when two
// files in the same repo use different drivers and both reference
// "users", they produce distinct nodes (db::postgres::users vs
// db::mysql::users) — the right behaviour for repos that span
// dialects.
func emitGoSQLEvents(events []goSQLEvent, dialect string, callerLookup func(line int) string, filePath string, result *parser.ExtractionResult) {
	if len(events) == 0 {
		return
	}
	if dialect == "" {
		dialect = "generic"
	}
	seenNodes := make(map[string]struct{})
	seenStringEdges := make(map[string]struct{})
	for _, e := range events {
		callerID := callerLookup(e.line)
		if callerID == "" {
			continue
		}
		// Anchor a KindString context="sql" registry node for this
		// query alongside the table edges. The node ID hashes the raw
		// query so two call sites issuing the same SQL share a single
		// registry entry, and downstream sql_rebuild can walk these
		// nodes to rederive KindTable / KindColumn without re-parsing
		// source. Empty queries (defensive — should not happen since
		// detectGoSQLCall returns ok=false) are skipped.
		if e.query != "" {
			tableIDs := make([]string, 0, len(e.tables))
			ops := make([]string, 0, len(e.tables))
			seenOp := make(map[string]struct{}, len(e.tables))
			for _, ref := range e.tables {
				tableIDs = append(tableIDs, sql.TableNodeID(dialect, ref.Schema, ref.Table))
				if _, dup := seenOp[ref.Op]; !dup && ref.Op != "" {
					seenOp[ref.Op] = struct{}{}
					ops = append(ops, ref.Op)
				}
			}
			nodeMeta := map[string]any{
				"dialect": dialect,
				"tables":  tableIDs,
			}
			if len(ops) > 0 {
				nodeMeta["ops"] = ops
			}
			edgeMeta := map[string]any{
				"dialect": dialect,
			}
			strID := goStringNodeID(stringCtxSQL, e.query)
			if _, dup := seenNodes[strID]; !dup {
				seenNodes[strID] = struct{}{}
				meta := map[string]any{
					"context": string(stringCtxSQL),
					"value":   e.query,
				}
				maps.Copy(meta, nodeMeta)
				result.Nodes = append(result.Nodes, &graph.Node{
					ID:       strID,
					Kind:     graph.KindString,
					Name:     e.query,
					FilePath: filePath, // first sighting; not authoritative
					Language: "go",
					Meta:     meta,
				})
			}
			edgeKey := callerID + "->" + strID
			if _, dup := seenStringEdges[edgeKey]; !dup {
				seenStringEdges[edgeKey] = struct{}{}
				em := map[string]any{
					"context": string(stringCtxSQL),
					"method":  e.method,
				}
				maps.Copy(em, edgeMeta)
				result.Edges = append(result.Edges, &graph.Edge{
					From:     callerID,
					To:       strID,
					Kind:     graph.EdgeEmits,
					FilePath: filePath,
					Line:     e.line,
					Origin:   graph.OriginASTInferred,
					Meta:     em,
				})
			}
		}
		for _, ref := range e.tables {
			tableID := sql.TableNodeID(dialect, ref.Schema, ref.Table)
			if _, ok := seenNodes[tableID]; !ok {
				seenNodes[tableID] = struct{}{}
				meta := map[string]any{
					"table":   ref.Table,
					"dialect": dialect,
				}
				if ref.Schema != "" {
					meta["schema"] = ref.Schema
				}
				result.Nodes = append(result.Nodes, &graph.Node{
					ID:       tableID,
					Kind:     graph.KindTable,
					Name:     ref.Table,
					FilePath: filePath, // first sighting; not authoritative
					Language: "go",
					Meta:     meta,
				})
			}
			result.Edges = append(result.Edges, &graph.Edge{
				From:     callerID,
				To:       tableID,
				Kind:     graph.EdgeQueries,
				FilePath: filePath,
				Line:     e.line,
				Origin:   graph.OriginTextMatched,
				Meta: map[string]any{
					"op":     ref.Op,
					"method": e.method,
				},
			})
		}
		// Column-level reads/writes — same call, finer granularity.
		// One KindColumn node per (table, column) tuple, one
		// EdgeReadsCol or EdgeWritesCol per call site.
		for _, col := range e.columns {
			colID := sql.ColumnNodeID(dialect, col.Schema, col.Table, col.Column)
			if _, ok := seenNodes[colID]; !ok {
				seenNodes[colID] = struct{}{}
				meta := map[string]any{
					"table":   col.Table,
					"column":  col.Column,
					"dialect": dialect,
				}
				if col.Schema != "" {
					meta["schema"] = col.Schema
				}
				result.Nodes = append(result.Nodes, &graph.Node{
					ID:       colID,
					Kind:     graph.KindColumn,
					Name:     col.Column,
					FilePath: filePath,
					Language: "go",
					Meta:     meta,
				})
			}
			edgeKind := graph.EdgeReadsCol
			if col.Op == "write" {
				edgeKind = graph.EdgeWritesCol
			}
			result.Edges = append(result.Edges, &graph.Edge{
				From:     callerID,
				To:       colID,
				Kind:     edgeKind,
				FilePath: filePath,
				Line:     e.line,
				Origin:   graph.OriginTextMatched,
				Meta: map[string]any{
					"method": e.method,
				},
			})
		}
	}
}
