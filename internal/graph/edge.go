package graph

type EdgeKind string

const (
	EdgeImports      EdgeKind = "imports"
	EdgeDefines      EdgeKind = "defines"
	EdgeCalls        EdgeKind = "calls"
	EdgeInstantiates EdgeKind = "instantiates"
	EdgeImplements   EdgeKind = "implements"
	EdgeExtends      EdgeKind = "extends"
	EdgeReferences   EdgeKind = "references"
	EdgeMemberOf     EdgeKind = "member_of"
	EdgeProvides     EdgeKind = "provides"
	EdgeConsumes     EdgeKind = "consumes"
	// EdgeMatches links a consumer contract node to the provider contract
	// node it resolves to (e.g. consumer http:GET:/v1/tucks → provider
	// http:GET:/v1/tucks, across repos). Traversals bridge service
	// boundaries by hopping Consumer → EdgeConsumes⁻¹ → consumer-contract
	// → EdgeMatches → provider-contract → EdgeProvides⁻¹ → handler.
	EdgeMatches EdgeKind = "matches"
	// EdgeAnnotated links a symbol to a synthetic annotation node
	// representing a decorator / annotation / attribute applied to it
	// (e.g. @Component, @Test, @Deprecated, #[derive(Debug)],
	// [Authorize], @app.route("/x"), @Published). The annotation node's
	// ID follows the convention "annotation::<lang>::<name>"; the edge's
	// Meta["args"] carries the verbatim argument text (truncated) when
	// the annotation has parentheses.
	//
	// Framework dispatch (NestJS @Get, Laravel middleware, Symfony
	// AsEventListener, Spring @Bean, FastAPI @app.route, …) continues
	// to flow through the contracts/dispatch layer with
	// EdgeProvides/EdgeConsumes — EdgeAnnotated runs in parallel as a
	// queryable record of the raw decorator. This lets agents answer
	// "find all @Deprecated" / "find all controllers" with one graph
	// hop without duplicating contract logic.
	EdgeAnnotated EdgeKind = "annotated"
	// EdgeTests links a test function/method to a non-test symbol it
	// exercises. Computed at index time as a post-extraction pass:
	// every call edge whose source is a test function (Meta["is_test"]
	// = true) and whose target is non-test produces an EdgeTests pair
	// alongside the existing EdgeCalls. Lets agents answer
	// "which tests cover X" with a single reverse-edge walk and lets
	// `get_untested_symbols` filter public symbols whose inverse-EdgeTests
	// set is empty.
	//
	// Test detection is by file naming convention plus per-language
	// fn-name conventions (Test*/Benchmark*/Fuzz* in Go, test_* /
	// Test* in Python, *_test.dart, etc.). Override per-repo via
	// .gortex.yaml::test_patterns when the project uses an unusual
	// layout — false positives are an acknowledged tradeoff (see
	// spec-graph-detail.md §4.4 for the heuristic catalogue).
	EdgeTests EdgeKind = "tests"
	// EdgeReads / EdgeWrites split EdgeReferences for value-side uses
	// of variables and fields. LHS of an assignment / op= / ++ / --
	// emits EdgeWrites; every other identifier or selector use emits
	// EdgeReads. EdgeReferences is reserved for type references
	// (`var x SomeType` references the type SomeType) so the resolver
	// can keep distinguishing the two by target node kind.
	//
	// Together with KindField, these let agents ask "which functions
	// write to this field" — impossible with the previous "any use is
	// a reference" model. Implemented per-language as the Go/TS/
	// Python (priority wave) and Rust/Java (second wave) extractors
	// learn to walk assignment AST nodes.
	EdgeReads  EdgeKind = "reads"
	EdgeWrites EdgeKind = "writes"
	// EdgeThrows links a function/method to an error or exception
	// type that can propagate from it. Per language:
	//
	//   go      function returns an error type → edge to that type
	//           (custom *MyError type or external::error sentinel for
	//           the built-in error interface).
	//   python  `raise <Exception>` AST nodes inside the body.
	//   java    method `throws` clause.
	//   swift   `throws` / `rethrows` keyword on the function decl.
	//   rust    return type contains Result<_, E> → edge to E.
	//
	// Lets agents ask "what error types can propagate from here" with
	// a single forward walk and lets `analyze kind: "error_surface"`
	// summarise every public function's error contract without
	// re-deriving it from source.
	EdgeThrows EdgeKind = "throws"
	// Phase 1+ edges added by spec-graph-coverage.md. Each edge is
	// produced only when the relevant index.<domain>.enabled gate is
	// set; the registry is permissive (DefaultOriginFor handles
	// unknown kinds via the confidence-score fallback).

	// EdgeParamOf links a KindParam node to its owning function or
	// method. Distinct from EdgeMemberOf (which is for fields of
	// types). Always ast_resolved by construction.
	EdgeParamOf EdgeKind = "param_of"
	// EdgeReturns links a function/method to a type it returns. Multi-
	// return Go functions emit one edge per result. Confidence reflects
	// the resolver: ast_inferred when the type is named in source,
	// promoted to ast_resolved / lsp_resolved by the semantic layer.
	EdgeReturns EdgeKind = "returns"
	// EdgeTypedAs binds a variable, parameter, field, or constant to
	// its declared type. Lets traversals answer "find all values of
	// type T". Distinct from EdgeReferences, which is broader.
	EdgeTypedAs EdgeKind = "typed_as"
	// EdgeCaptures links a closure node to an outer binding it closes
	// over.
	EdgeCaptures EdgeKind = "captures"
	// EdgeSpawns links a caller to a function it launches
	// asynchronously (goroutine, async/await, Promise, worker pool).
	// Emitted in addition to the corresponding EdgeCalls so synchronous
	// reachability queries can scope by edge kind. Meta["mode"] ∈
	// goroutine|async|promise|worker_pool.
	EdgeSpawns EdgeKind = "spawns"
	// EdgeSends / EdgeRecvs link a function to a channel-typed
	// variable for channel I/O. The channel's element type is reachable
	// via the variable's EdgeTypedAs edge.
	EdgeSends EdgeKind = "sends"
	EdgeRecvs EdgeKind = "recvs"
	// EdgeQueries links a function to a database table it queries
	// against. Default origin text_matched from string-literal SQL;
	// promoted to ast_resolved when an ORM mapping is recognized.
	EdgeQueries EdgeKind = "queries"
	// EdgeReadsCol / EdgeWritesCol provide column-level resolution
	// when the SQL parser can extract it. Falls back to table-level
	// EdgeQueries when columns can't be resolved.
	EdgeReadsCol  EdgeKind = "reads_col"
	EdgeWritesCol EdgeKind = "writes_col"
	// EdgeReadsConfig / EdgeWritesConfig link a function to a config
	// key it reads or writes (env var, viper key, k8s configmap entry,
	// struct-tag binding).
	EdgeReadsConfig  EdgeKind = "reads_config"
	EdgeWritesConfig EdgeKind = "writes_config"
	// EdgeTogglesFlag links a function to a feature flag it checks or
	// toggles. Meta["op"] ∈ read|write|register.
	EdgeTogglesFlag EdgeKind = "toggles_flag"
	// EdgeEmits links a function to a log/metric/trace event it emits.
	// Meta carries level (for logs), unit (for metrics), and label keys.
	EdgeEmits EdgeKind = "emits"
	// EdgeGeneratedBy links a generated file to its schema source
	// (.proto, .graphql, openapi.yaml, etc.). Detected via comment
	// markers (// Code generated …), conventional adjacency, or
	// go:generate directives.
	EdgeGeneratedBy EdgeKind = "generated_by"
	// EdgeDependsOnModule links a file/package/import to a KindModule
	// node. One edge per import statement; aggregable to package-level.
	EdgeDependsOnModule EdgeKind = "depends_on_module"
	// EdgeOwns links a team to a file or directory. Sourced from
	// CODEOWNERS. Directory entries materialize per-file.
	EdgeOwns EdgeKind = "owns"
	// EdgeAuthored links a person/team to a node they last touched.
	// Meta carries commit and timestamp. People are stored as
	// KindTeam nodes with Meta["kind"]="person".
	EdgeAuthored EdgeKind = "authored"
	// EdgeCoveredBy links a function/method to a test that exercises
	// it, with coverage_pct attached in Meta. Directional inverse of
	// EdgeTests, distinguished by carrying the coverage metric.
	EdgeCoveredBy EdgeKind = "covered_by"
	// EdgeAliases links a type alias `type X = Y` to its underlying
	// type. Distinct from EdgeExtends (`type X Y` newtype) — agents
	// distinguish by edge kind to compute correct blast radius.
	EdgeAliases EdgeKind = "aliases"
	// EdgeComposes links a type to an embedded/composed/mixed-in type
	// (Go struct embedding, Rust trait bounds, Python multiple
	// inheritance). Distinct from EdgeExtends (newtype/inheritance/
	// interface extension).
	EdgeComposes EdgeKind = "composes"
	// EdgeLicensedAs links a file to its SPDX license. Sourced from
	// the file's SPDX-License-Identifier header, falling back to the
	// repo-level LICENSE file.
	EdgeLicensedAs EdgeKind = "licensed_as"
)

type Edge struct {
	From     string   `json:"from"`
	To       string   `json:"to"`
	Kind     EdgeKind `json:"kind"`
	FilePath string   `json:"file_path"`
	Line     int      `json:"line"`
	// Confidence is the numeric score (0..1). Kept on the in-memory
	// struct for internal filtering (min_tier, etc.) but excluded from
	// JSON — agents act on ConfidenceLabel, and the float adds ~15
	// chars to every edge in large graph responses.
	Confidence      float64 `json:"-"`
	ConfidenceLabel string  `json:"confidence_label,omitempty"`
	Origin          string  `json:"origin,omitempty"`
	CrossRepo       bool    `json:"cross_repo,omitempty"`
	// Meta is intentionally excluded from JSON. It holds internal
	// instrumentation (semantic_source, provider hints, etc.) that agents
	// don't consume but that adds measurable bytes to every edge in
	// responses returning hundreds of call-graph edges. Internal callers
	// can still read/write the field; external MCP consumers don't see it.
	Meta map[string]any `json:"-"`
}

// Edge.Origin values — call-graph confidence tiers, highest → lowest. Use
// MeetsMinTier / OriginRank to compare.
//
//   - lsp_resolved: Compiler-grade. LSP, go/types, or SCIP confirms that this
//     edge's target is the precise symbol being referenced. Safe to rely on
//     for refactors.
//   - lsp_dispatch: Interface → implementation dispatch resolved by a
//     semantic provider. One step less direct than a literal target match.
//   - ast_resolved: Tree-sitter / AST extraction found a unique target in
//     the same compilation unit. No type system involved, but structurally
//     unambiguous.
//   - ast_inferred: Heuristic resolution using type info we extracted from
//     the AST. Not compiler-verified.
//   - text_matched: Name-only match. The weakest tier — could be a false
//     positive.
const (
	OriginLSPResolved = "lsp_resolved"
	OriginLSPDispatch = "lsp_dispatch"
	OriginASTResolved = "ast_resolved"
	OriginASTInferred = "ast_inferred"
	OriginTextMatched = "text_matched"
)

// OriginRank returns a numeric rank for origin comparison. Higher = more
// confident. Unknown or empty origin returns 0 so it sorts below all known
// tiers; filters treat it as "untagged" and fall back to legacy inference.
func OriginRank(origin string) int {
	switch origin {
	case OriginLSPResolved:
		return 5
	case OriginLSPDispatch:
		return 4
	case OriginASTResolved:
		return 3
	case OriginASTInferred:
		return 2
	case OriginTextMatched:
		return 1
	}
	return 0
}

// MeetsMinTier returns true when origin is at least as confident as minTier.
// Empty minTier always passes (no filter). Empty origin fails any non-empty
// filter — callers wanting legacy fallback should first backfill via
// DefaultOriginFor.
func MeetsMinTier(origin, minTier string) bool {
	if minTier == "" {
		return true
	}
	return OriginRank(origin) >= OriginRank(minTier)
}

// DefaultOriginFor derives an origin tier for edges that don't have Origin
// set yet (edges from providers not updated to set Origin directly, or from
// indexes produced before this field existed). Uses edge kind, confidence
// score, and semantic_source meta as fallback signals. Never returns empty.
func DefaultOriginFor(kind EdgeKind, confidence float64, semanticSource string) string {
	if semanticSource != "" {
		if kind == EdgeImplements {
			return OriginLSPDispatch
		}
		return OriginLSPResolved
	}
	// Structural AST edges are unambiguous by construction.
	switch kind {
	case EdgeDefines, EdgeImports, EdgeExtends, EdgeMemberOf,
		EdgeImplements, EdgeProvides, EdgeConsumes, EdgeMatches,
		// spec-graph-coverage.md additions: structural edges where the
		// extractor produces an unambiguous source→target binding.
		EdgeParamOf, EdgeAliases, EdgeComposes, EdgeLicensedAs,
		EdgeOwns, EdgeAuthored, EdgeGeneratedBy, EdgeDependsOnModule,
		EdgeCaptures:
		return OriginASTResolved
	}
	// Resolution-derived edges fall back to confidence score.
	switch {
	case confidence >= 0.9:
		return OriginASTResolved
	case confidence >= 0.5:
		return OriginASTInferred
	}
	return OriginTextMatched
}

// ConfidenceLabelFor returns EXTRACTED, INFERRED, or AMBIGUOUS for an edge
// based on its kind and confidence value.
//
// Kept for back-compat; new code should prefer Origin tiers (OriginRank /
// MeetsMinTier) which distinguish LSP-grade from AST-grade evidence.
func ConfidenceLabelFor(kind EdgeKind, confidence float64) string {
	// Structural edges from AST are always extracted.
	switch kind {
	case EdgeDefines, EdgeImports, EdgeExtends, EdgeMemberOf, EdgeImplements,
		EdgeProvides, EdgeConsumes, EdgeMatches,
		EdgeParamOf, EdgeAliases, EdgeComposes, EdgeLicensedAs,
		EdgeOwns, EdgeAuthored, EdgeGeneratedBy, EdgeDependsOnModule,
		EdgeCaptures:
		return "EXTRACTED"
	}
	// Resolution-derived edges: classify by confidence score.
	switch {
	case confidence >= 0.9:
		return "EXTRACTED"
	case confidence >= 0.5:
		return "INFERRED"
	case confidence > 0:
		return "AMBIGUOUS"
	default:
		// confidence == 0 means resolved without type info.
		return "INFERRED"
	}
}
