package contracts

import (
	"regexp"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// -----------------------------------------------------------------------------
// Rust — Axum / Actix / Rocket providers, reqwest consumer
// -----------------------------------------------------------------------------
//
// All three major Rust frameworks share the same extraction patterns:
//
//   Request body:  Json(payload) as a handler parameter, json: Json<Payload>
//   Query params:  Query(q): Query<Q>
//   Path params:   Path(id): Path<String>
//   Response:      -> Json<Resp>, -> (StatusCode, Json<Resp>), -> impl IntoResponse
//   Status:        StatusCode::OK, 200.into_response()
//
// The regexes are deliberately agnostic — Axum and Actix both use
// `Json<T>` / `Query<T>` / `Path<T>` extractors, Rocket's form is
// close enough that the same patterns catch its common cases.

func init() {
	schemaEnrichers = append(schemaEnrichers,
		schemaEnricher{
			name:      "rust-provider",
			languages: []string{"rust"},
			roles:     []Role{RoleProvider},
			detect:    rustProviderDetect,
		},
		schemaEnricher{
			name:      "rust-consumer",
			languages: []string{"rust"},
			roles:     []Role{RoleConsumer},
			detect:    rustConsumerDetect,
		},
	)
}

var (
	// Json<Payload> extractor in parameter list. Captures the type
	// wrapped by `Json<...>`. Non-greedy inside `<>` so nested
	// generics don't over-capture.
	rustJsonExtractRe = regexp.MustCompile(`Json\s*<\s*([A-Za-z_][\w:]*(?:<[^>]*>)?)\s*>`)
	// Query<QueryParams>
	rustQueryExtractRe = regexp.MustCompile(`Query\s*<\s*([A-Za-z_][\w:]*)\s*>`)
	// Return type: `-> Json<T>` or `-> (StatusCode, Json<T>)` or
	// `-> Result<Json<T>, E>`. The Json<T> inside is what matters.
	rustReturnJsonRe = regexp.MustCompile(`->\s*[^{]*?Json\s*<\s*([A-Za-z_][\w:]*(?:<[^>]*>)?)\s*>`)
	// StatusCode::OK, StatusCode::CREATED, etc.
	rustStatusRe = regexp.MustCompile(`StatusCode::(\w+)`)
	// Bare HTTP status literal passed to Response builders:
	// `.status(201)`, `StatusCode::from_u16(404)`.
	rustStatusLitRe = regexp.MustCompile(`\bstatus\s*\(\s*(\d{3})\s*\)`)
	// Consumer-side: `.json(&payload)` attaches a body.
	rustJsonBodyRe = regexp.MustCompile(`\.json\(\s*&?(\w+)\s*\)`)
	// Consumer-side decode: `.json::<ResponseType>().await`.
	rustJsonDecodeRe = regexp.MustCompile(`\.json::<\s*([A-Za-z_][\w:]*)\s*>\(\)`)
)

// rustStatusConstants maps Rust's StatusCode variant names to their
// numeric codes. Covers the common cases — the reqwest / axum crates
// use the same names as Go's net/http but in SCREAMING_SNAKE_CASE.
var rustStatusConstants = map[string]int{
	"CONTINUE":                      100,
	"SWITCHING_PROTOCOLS":           101,
	"OK":                            200,
	"CREATED":                       201,
	"ACCEPTED":                      202,
	"NO_CONTENT":                    204,
	"MOVED_PERMANENTLY":             301,
	"FOUND":                         302,
	"SEE_OTHER":                     303,
	"NOT_MODIFIED":                  304,
	"TEMPORARY_REDIRECT":            307,
	"PERMANENT_REDIRECT":            308,
	"BAD_REQUEST":                   400,
	"UNAUTHORIZED":                  401,
	"FORBIDDEN":                     403,
	"NOT_FOUND":                     404,
	"METHOD_NOT_ALLOWED":            405,
	"CONFLICT":                      409,
	"GONE":                          410,
	"PRECONDITION_FAILED":           412,
	"UNPROCESSABLE_ENTITY":          422,
	"TOO_MANY_REQUESTS":             429,
	"INTERNAL_SERVER_ERROR":         500,
	"NOT_IMPLEMENTED":               501,
	"BAD_GATEWAY":                   502,
	"SERVICE_UNAVAILABLE":           503,
	"GATEWAY_TIMEOUT":               504,
}

func rustProviderDetect(body string, fileNodes []*graph.Node) schemaHints {
	var h schemaHints

	// Request: first Json<T> extractor in the signature is the body.
	if m := rustJsonExtractRe.FindStringSubmatch(body); len(m) > 1 {
		t := stripRustPath(stripGenericsRust(m[1]))
		if t != "" {
			h.RequestType = resolveTypeInFile(t, fileNodes)
		}
	}

	// Response: return-type Json<T>.
	if m := rustReturnJsonRe.FindStringSubmatch(body); len(m) > 1 {
		t := stripRustPath(stripGenericsRust(m[1]))
		if t != "" {
			h.ResponseType = resolveTypeInFile(t, fileNodes)
		}
	}

	// Query params — the Query<T> type is itself a struct. We can't
	// enumerate its fields without looking up T, so just record that
	// a query extractor is present by its type name — downstream
	// shape extraction picks up the field list.
	for _, m := range rustQueryExtractRe.FindAllStringSubmatch(body, -1) {
		_ = m
		// Nothing to record inline; the Query<T> struct's fields
		// become the query params via shape extraction on T.
	}

	// Status codes: StatusCode::CREATED, .status(201).
	for _, m := range rustStatusRe.FindAllStringSubmatch(body, -1) {
		if code, ok := rustStatusConstants[m[1]]; ok {
			h.StatusCodes = append(h.StatusCodes, code)
		}
	}
	for _, m := range rustStatusLitRe.FindAllStringSubmatch(body, -1) {
		if code, ok := parseStatusExpr(m[1]); ok {
			h.StatusCodes = append(h.StatusCodes, code)
		}
	}
	return h
}

func rustConsumerDetect(body string, fileNodes []*graph.Node) schemaHints {
	var h schemaHints

	// Outbound payload: `.json(&payload)`.
	if m := rustJsonBodyRe.FindStringSubmatch(body); len(m) > 1 {
		if rt := findRustVarType(body, m[1]); rt != "" {
			h.RequestType = resolveTypeInFile(rt, fileNodes)
		}
	}
	// Response decode: `.json::<User>().await`.
	if m := rustJsonDecodeRe.FindStringSubmatch(body); len(m) > 1 {
		t := stripRustPath(m[1])
		if t != "" {
			h.ResponseType = resolveTypeInFile(t, fileNodes)
		}
	}
	return h
}

// findRustVarType scans for a Rust local binding of `name` and
// returns its type annotation when present. Covers:
//
//	let name: Type = ...;
//	let name = Type { ... };
//	let name = Type::new(...);
//	fn name(param: Type, ...) handler parameter
func findRustVarType(body, varName string) string {
	if varName == "" {
		return ""
	}
	v := regexp.QuoteMeta(varName)
	// let name: Type = ...
	if m := regexp.MustCompile(`\blet\s+(?:mut\s+)?` + v + `\s*:\s*([A-Za-z_][\w:]*(?:<[^>]*>)?)`).FindStringSubmatch(body); len(m) > 1 {
		return stripRustPath(stripGenericsRust(m[1]))
	}
	// let name = Type { ... }  |  let name = Type::new(...)
	if m := regexp.MustCompile(`\blet\s+(?:mut\s+)?` + v + `\s*=\s*([A-Za-z_][\w:]*)\s*(?:\{|::)`).FindStringSubmatch(body); len(m) > 1 {
		return stripRustPath(m[1])
	}
	// fn signature parameter: `name: Type`.
	if m := regexp.MustCompile(`\b` + v + `\s*:\s*([A-Za-z_][\w:]*(?:<[^>]*>)?)`).FindStringSubmatch(body); len(m) > 1 {
		return stripRustPath(stripGenericsRust(m[1]))
	}
	return ""
}

// stripRustPath drops the `module::path::` prefix from a Rust type
// expression so `models::User` / `crate::domain::User` both collapse
// to `User`. The graph's Rust extractor indexes type nodes by bare
// name; the cross-repo post-pass upgrades bare names to symbol IDs.
func stripRustPath(t string) string {
	t = strings.TrimSpace(t)
	t = strings.TrimPrefix(t, "&")
	t = strings.TrimPrefix(t, "mut ")
	if i := strings.LastIndex(t, "::"); i >= 0 {
		t = t[i+2:]
	}
	return t
}

// stripGenericsRust trims any `<...>` generic suffix on a Rust type
// expression. `Vec<User>` → `Vec`, `Option<Box<T>>` → `Option`. The
// caller is responsible for unwrapping container types further —
// canonicalType in validate.go handles `Vec<T>` as a slice
// equivalence, so just drop the parameterisation here.
func stripGenericsRust(t string) string {
	t = strings.TrimSpace(t)
	if i := strings.Index(t, "<"); i >= 0 {
		return strings.TrimSpace(t[:i])
	}
	return t
}
