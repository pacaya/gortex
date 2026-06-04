package languages

import (
	"regexp"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// SQL function call-site extraction. A SQL CREATE FUNCTION is already a
// first-class KindFunction node (sql.go); this pass captures the
// cross-language call sites that invoke one so the resolver / SQL-callsite
// synthesizer can link the calling code to the SQL function. Two dominant
// patterns are recognised across JS/TS/Python:
//
//   - PostgREST / Supabase RPC: `client.rpc('fn_name', {...})` — the
//     first string-literal argument is the SQL function name.
//   - SQLAlchemy generative func namespace (Python): `func.fn_name(...)`.
//
// Each emits a placeholder EdgeCalls to `unresolved::sqlfn::<name>` with
// Meta["via"]="sql.callsite" that ResolveSQLCallsites lands on the SQL
// function node.
const sqlCallsiteVia = "sql.callsite"

var (
	sqlRPCRe       = regexp.MustCompile(`\.rpc\s*\(\s*['"` + "`" + `]([A-Za-z_][\w]*)['"` + "`" + `]`)
	sqlAlchemyFnRe = regexp.MustCompile(`\bfunc\.([A-Za-z_]\w*)\s*\(`)
)

// sqlCallPlaceholder is the unresolved target a SQL function call is
// emitted onto for ResolveSQLCallsites to land on the SQL function node.
func sqlCallPlaceholder(fn string) string { return "unresolved::sqlfn::" + fn }

// emitSQLCallsiteEdges scans src for SQL function call sites and emits a
// placeholder call edge from each call's enclosing function. The
// SQLAlchemy `func.<name>` form is Python-only; the `.rpc('name')` form is
// recognised in every language. callerFor maps a 1-based line to the
// enclosing function ID.
func emitSQLCallsiteEdges(src []byte, language string, callerFor func(line int) string, filePath string, result *parser.ExtractionResult) {
	type site struct {
		fn   string
		line int
	}
	var sites []site
	for _, m := range sqlRPCRe.FindAllSubmatchIndex(src, -1) {
		sites = append(sites, site{fn: string(src[m[2]:m[3]]), line: lineAt(src, m[0])})
	}
	if language == "python" {
		for _, m := range sqlAlchemyFnRe.FindAllSubmatchIndex(src, -1) {
			sites = append(sites, site{fn: string(src[m[2]:m[3]]), line: lineAt(src, m[0])})
		}
	}
	for _, s := range sites {
		if s.fn == "" {
			continue
		}
		caller := callerFor(s.line)
		if caller == "" {
			continue
		}
		result.Edges = append(result.Edges, &graph.Edge{
			From: caller, To: sqlCallPlaceholder(s.fn), Kind: graph.EdgeCalls,
			FilePath: filePath, Line: s.line, Origin: graph.OriginASTInferred,
			Meta: map[string]any{"via": sqlCallsiteVia, "sql_function": s.fn},
		})
	}
}
