package contracts

import (
	"regexp"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// -----------------------------------------------------------------------------
// TypeScript / JavaScript enrichers
// -----------------------------------------------------------------------------
//
// NestJS is the gold case: decorators carry request / query / param
// types explicitly, and the handler's return-type annotation pins the
// response. Express is essentially untyped at runtime so we fall back
// to expression capture for most things, but `req.body as Foo` and
// `res.json(named)` still give us a handle. Fetch / axios consumers
// use generics or payload variables — we resolve both when we can.

func init() {
	schemaEnrichers = append(schemaEnrichers,
		schemaEnricher{
			name:      "ts-nestjs-provider",
			languages: []string{"typescript", "javascript"},
			roles:     []Role{RoleProvider},
			detect:    tsNestJSDetect,
		},
		schemaEnricher{
			name:      "ts-express-provider",
			languages: []string{"typescript", "javascript"},
			roles:     []Role{RoleProvider},
			detect:    tsExpressDetect,
		},
		schemaEnricher{
			name:      "ts-axios-consumer",
			languages: []string{"typescript", "javascript"},
			roles:     []Role{RoleConsumer},
			detect:    tsAxiosDetect,
		},
		schemaEnricher{
			name:      "ts-fetch-consumer",
			languages: []string{"typescript", "javascript"},
			roles:     []Role{RoleConsumer},
			detect:    tsFetchDetect,
		},
		schemaEnricher{
			name:      "ts-wrapper-consumer",
			languages: []string{"typescript", "javascript"},
			roles:     []Role{RoleConsumer},
			detect:    tsWrapperConsumerDetect,
		},
	)
}

// -----------------------------------------------------------------------------
// NestJS provider
//
// Captures:
//	@Body() foo: SomeDto
//	@Body() foo: SomeDto              → request_type
//	@Query('x') x: string             → query param
//	@Param('id') id: number           → path param (already known)
//	@HttpCode(201)                    → status code
//	createUser(...): Promise<UserDto> → response_type (unwraps Promise / Observable / Response)
// -----------------------------------------------------------------------------

var (
	nestBodyParamRe = regexp.MustCompile(`@Body\(\s*(?:['"](\w+)['"])?\s*\)\s*(?:@\w+\([^)]*\)\s*)*\w+\s*:\s*([A-Za-z_$][\w$]*(?:<[^>]+>)?)`)
	nestQueryRe     = regexp.MustCompile(`@Query\(\s*(?:['"](\w+)['"])?\s*\)`)
	nestHttpCodeRe  = regexp.MustCompile(`@HttpCode\(\s*(?:HttpStatus\.(\w+)|(\d+))\s*\)`)
	// Method signature: `  foo(args): ReturnType {`. Unwrap Promise<T>,
	// Observable<T>, Response<T>. The match is anchored on a `(...) : Type`
	// followed by `{` on the same line to avoid eating interface decls.
	nestReturnRe = regexp.MustCompile(`\)\s*:\s*(?:Promise|Observable|Response)?<?\s*([A-Za-z_$][\w$.]*)\s*>?\s*\{`)
)

func tsNestJSDetect(body string, fileNodes []*graph.Node) schemaHints {
	var h schemaHints

	if m := nestBodyParamRe.FindStringSubmatch(body); len(m) > 2 {
		h.RequestType = resolveTypeInFile(stripGenerics(m[2]), fileNodes)
	}

	for _, m := range nestQueryRe.FindAllStringSubmatch(body, -1) {
		if len(m) > 1 && m[1] != "" {
			h.QueryParams = append(h.QueryParams, m[1])
		}
	}

	for _, m := range nestHttpCodeRe.FindAllStringSubmatch(body, -1) {
		if m[1] != "" {
			if code, ok := parseStatusExpr(m[1]); ok {
				h.StatusCodes = append(h.StatusCodes, code)
			}
		} else if m[2] != "" {
			if code, ok := parseStatusExpr(m[2]); ok {
				h.StatusCodes = append(h.StatusCodes, code)
			}
		}
	}

	if m := nestReturnRe.FindStringSubmatch(body); len(m) > 1 {
		if rt := stripGenerics(m[1]); rt != "" && rt != "void" && rt != "any" && rt != "unknown" {
			h.ResponseType = resolveTypeInFile(rt, fileNodes)
		}
	}

	return h
}

// -----------------------------------------------------------------------------
// Express provider
//
// Mostly untyped. We still try:
//	req.body as SomeDto               → request_type
//	const foo: SomeDto = req.body     → request_type (less common)
//	res.status(201).json(result)      → status + response (if `result` is typed)
//	res.json(result)                  → response
//	res.sendStatus(204)               → status
//	req.query.<name>, req.params.<name> → enumerate names
// -----------------------------------------------------------------------------

var (
	exprReqBodyAsRe  = regexp.MustCompile(`req\.body\s+as\s+([A-Za-z_$][\w$.]*)`)
	exprReqBodyAnnRe = regexp.MustCompile(`const\s+\w+\s*:\s*([A-Za-z_$][\w$.]*)\s*=\s*req\.body`)
	exprResJSONRe    = regexp.MustCompile(`res\.(?:status\(\s*(\d+)\s*\)\s*\.)?json\(\s*([A-Za-z_$][\w$]*)\s*\)`)
	exprResStatusRe  = regexp.MustCompile(`res\.(?:status|sendStatus)\(\s*(\d+)\s*\)`)
	exprQueryRe      = regexp.MustCompile(`req\.query\.(\w+)`)
	exprHeaderRe     = regexp.MustCompile(`req\.headers\.(\w+)`)
)

func tsExpressDetect(body string, fileNodes []*graph.Node) schemaHints {
	var h schemaHints

	if m := exprReqBodyAsRe.FindStringSubmatch(body); len(m) > 1 {
		h.RequestType = resolveTypeInFile(m[1], fileNodes)
	} else if m := exprReqBodyAnnRe.FindStringSubmatch(body); len(m) > 1 {
		h.RequestType = resolveTypeInFile(m[1], fileNodes)
	}

	for _, m := range exprResJSONRe.FindAllStringSubmatch(body, -1) {
		if m[1] != "" {
			if code, ok := parseStatusExpr(m[1]); ok {
				h.StatusCodes = append(h.StatusCodes, code)
			}
		}
		if rt := findTSVarType(body, m[2]); rt != "" {
			h.ResponseType = resolveTypeInFile(rt, fileNodes)
		} else if h.ResponseExpr == "" {
			h.ResponseExpr = "res.json(" + m[2] + ")"
		}
	}
	for _, m := range exprResStatusRe.FindAllStringSubmatch(body, -1) {
		if code, ok := parseStatusExpr(m[1]); ok {
			h.StatusCodes = append(h.StatusCodes, code)
		}
	}
	h.QueryParams = append(h.QueryParams, allSubmatches(body, exprQueryRe, 1)...)
	// Header names are not surfaced yet, but we capture them here so a
	// later pass can add the key without re-scanning.
	_ = exprHeaderRe

	return h
}

// -----------------------------------------------------------------------------
// Axios consumer
//
// Captures:
//	axios.get<UserResp>(url)                 → response via generic
//	axios.post<UserResp, UserReq>(url, pay)  → both
//	axios.post(url, payload)                 → request via payload var
//	axios.post(url, payload as UserReq)      → request via cast
// -----------------------------------------------------------------------------

var (
	axiosGenericRe = regexp.MustCompile(`axios\.(?:get|post|put|delete|patch|head|options)<\s*([A-Za-z_$][\w$.]*)\s*(?:,\s*([A-Za-z_$][\w$.]*)\s*)?>\(`)
	axiosCallRe    = regexp.MustCompile(`axios\.(?:post|put|patch)\(\s*(?:[^,]+),\s*([A-Za-z_$][\w$]*)\s*(?:as\s+([A-Za-z_$][\w$.]*))?\s*[),]`)
)

func tsAxiosDetect(body string, fileNodes []*graph.Node) schemaHints {
	var h schemaHints

	if m := axiosGenericRe.FindStringSubmatch(body); len(m) > 1 && m[1] != "" {
		h.ResponseType = resolveTypeInFile(m[1], fileNodes)
		if len(m) > 2 && m[2] != "" {
			h.RequestType = resolveTypeInFile(m[2], fileNodes)
		}
	}
	if m := axiosCallRe.FindStringSubmatch(body); len(m) > 1 {
		if len(m) > 2 && m[2] != "" {
			h.RequestType = resolveTypeInFile(m[2], fileNodes)
		} else if rt := findTSVarType(body, m[1]); rt != "" {
			if h.RequestType == "" {
				h.RequestType = resolveTypeInFile(rt, fileNodes)
			}
		}
	}
	return h
}

// -----------------------------------------------------------------------------
// Fetch consumer
//
// Captures:
//	fetch(url, { method, body: JSON.stringify(payload) })
//	const data = (await resp.json()) as SomeResp
//	const data: SomeResp = await resp.json()
// -----------------------------------------------------------------------------

var (
	fetchJSONStringifyRe = regexp.MustCompile(`JSON\.stringify\(\s*([A-Za-z_$][\w$]*)\s*\)`)
	fetchRespCastRe      = regexp.MustCompile(`\(\s*await\s+[\w.]+\.json\(\)\s*\)\s*as\s+([A-Za-z_$][\w$.]*)`)
	fetchRespAnnRe       = regexp.MustCompile(`const\s+\w+\s*:\s*([A-Za-z_$][\w$.]*)\s*=\s*await\s+[\w.]+\.json\(\)`)
)

func tsFetchDetect(body string, fileNodes []*graph.Node) schemaHints {
	var h schemaHints
	if m := fetchJSONStringifyRe.FindStringSubmatch(body); len(m) > 1 {
		if rt := findTSVarType(body, m[1]); rt != "" {
			h.RequestType = resolveTypeInFile(rt, fileNodes)
		} else if h.RequestExpr == "" {
			h.RequestExpr = "JSON.stringify(" + m[1] + ")"
		}
	}
	if m := fetchRespCastRe.FindStringSubmatch(body); len(m) > 1 {
		h.ResponseType = resolveTypeInFile(m[1], fileNodes)
	} else if m := fetchRespAnnRe.FindStringSubmatch(body); len(m) > 1 {
		h.ResponseType = resolveTypeInFile(m[1], fileNodes)
	}
	return h
}

// -----------------------------------------------------------------------------
// Custom wrapper consumer
//
// Many TS/JS codebases wrap `fetch` / `axios` in a project-specific
// helper so individual call sites look like:
//
//	export async function blockEmailSource(
//	  getToken: TokenGetter,
//	  id: string,
//	): Promise<void> {
//	  return request<void>(`/v1/email-sources/${id}/block`, getToken, {
//	    method: 'POST',
//	  });
//	}
//
//	const resp = await request<UserResp>('/users', t, { method: 'POST', body: JSON.stringify(payload) });
//
// Neither the axios nor the fetch enricher matches because the
// network call goes through a user-defined function. But the
// generic type parameter (`request<UserResp>`) or the enclosing
// function's return annotation (`: Promise<UserResp>`) give us the
// response type directly. This detector picks up both signals.
// -----------------------------------------------------------------------------

var (
	// Generic type parameter on a call. Covers three idioms:
	//   request<UserResp>(             — plain wrapper
	//   api.get<User>(                 — namespaced method
	//   createClient(cfg).get<T>(path) — curried-then-called
	// The `(?:\.[A-Za-z_$][\w$]*)*` chain tolerates any number of
	// method hops before the generic call.
	tsGenericCallRe = regexp.MustCompile(
		`[A-Za-z_$][\w$]*(?:\.[A-Za-z_$][\w$]*)*<\s*([A-Za-z_$][\w$.|\s<>[\],]*?)\s*>\s*\(`,
	)
	// React Query / SWR style (`useQuery<UserResp>(...)`) is already
	// handled by the generic regex above — `useQuery` has the same
	// syntactic shape as any other generic call.
	// Function return annotation: `): Promise<UserResp> {`. Used
	// when the generic-call form above wasn't found (e.g. the
	// wrapper is untyped but the outer function annotates its
	// return). Requires `Promise<...>` to avoid matching arbitrary
	// `: Type` annotations elsewhere.
	tsPromiseReturnRe = regexp.MustCompile(
		`\)\s*:\s*Promise\s*<\s*([A-Za-z_$][\w$.|\s<>[\],]*?)\s*>\s*[{=]`,
	)
	// Body option carrying a JSON-stringified payload through a
	// wrapper call: `body: JSON.stringify(payload)` or
	// `{ body: payload }` in the options object. Reuses
	// fetchJSONStringifyRe via tsFetchDetect, so we only handle the
	// typed-payload case here.
	tsWrapperBodyArgRe = regexp.MustCompile(`body\s*:\s*([A-Za-z_$][\w$]*)\b`)
)

func tsWrapperConsumerDetect(body string, fileNodes []*graph.Node) schemaHints {
	var h schemaHints

	// Response type via generic call. First match wins — call sites
	// usually have at most one HTTP wrapper invocation.
	if m := tsGenericCallRe.FindStringSubmatch(body); len(m) > 1 {
		t := cleanTSTypeExpr(m[1])
		if t != "" && t != "void" && t != "unknown" && t != "any" {
			h.ResponseType = resolveTypeInFile(stripGenerics(t), fileNodes)
		}
	}
	if h.ResponseType == "" {
		if m := tsPromiseReturnRe.FindStringSubmatch(body); len(m) > 1 {
			t := cleanTSTypeExpr(m[1])
			if t != "" && t != "void" && t != "unknown" && t != "any" {
				h.ResponseType = resolveTypeInFile(stripGenerics(t), fileNodes)
			}
		}
	}

	// Request type via a body argument in the options object.
	if m := tsWrapperBodyArgRe.FindStringSubmatch(body); len(m) > 1 {
		if rt := findTSVarType(body, m[1]); rt != "" {
			h.RequestType = resolveTypeInFile(rt, fileNodes)
		}
	}

	return h
}

// cleanTSTypeExpr trims whitespace, drops nullable-union suffixes
// (`| null` / `| undefined`), and strips pointer/optional markers.
// Keeps generic-parameter and union structure intact so
// `stripGenerics` can act on the result.
func cleanTSTypeExpr(t string) string {
	t = strings.TrimSpace(t)
	t = strings.TrimSuffix(t, "?")
	// Trim trailing `| null` / `| undefined`.
	lower := strings.ReplaceAll(t, " ", "")
	for _, suffix := range []string{"|null", "|undefined"} {
		if strings.HasSuffix(lower, suffix) {
			cut := len(t) - len(suffix)
			// Preserve original whitespace form by cutting at the
			// rightmost `|` instead of trusting the squashed length.
			if idx := strings.LastIndex(t, "|"); idx >= 0 {
				t = strings.TrimSpace(t[:idx])
			} else {
				t = strings.TrimSpace(t[:cut])
			}
		}
	}
	return strings.TrimSpace(t)
}

// -----------------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------------

// findTSVarType mirrors findVarType but targets TypeScript/JavaScript
// declaration forms. Covers:
//
//	const name: Type = ...
//	let name: Type = ...
//	var name: Type = ...
//	const name = new Type(...)
//	const name = { ... } as Type
//	function arg: Type
func findTSVarType(body, varName string) string {
	if varName == "" {
		return ""
	}
	v := regexp.QuoteMeta(varName)

	// const/let/var foo: Type = ...
	if m := regexp.MustCompile(`\b(?:const|let|var)\s+` + v + `\s*:\s*([A-Za-z_$][\w$.]*)`).FindStringSubmatch(body); len(m) > 1 {
		return m[1]
	}
	// const foo = new Type(...)
	if m := regexp.MustCompile(`\b(?:const|let|var)\s+` + v + `\s*=\s*new\s+([A-Za-z_$][\w$.]*)`).FindStringSubmatch(body); len(m) > 1 {
		return m[1]
	}
	// const foo = <expr> as Type
	if m := regexp.MustCompile(`\b(?:const|let|var)\s+` + v + `\s*=\s*[^;]+?\s+as\s+([A-Za-z_$][\w$.]*)`).FindStringSubmatch(body); len(m) > 1 {
		return m[1]
	}
	// function/arrow param: (..., foo: Type, ...)
	if m := regexp.MustCompile(`\b` + v + `\s*:\s*([A-Za-z_$][\w$.]*)(?:\s*[,)])`).FindStringSubmatch(body); len(m) > 1 {
		return m[1]
	}
	return ""
}

// stripGenerics drops a trailing `<...>` from a type expression so
// `ListResponse<User>` collapses to `ListResponse`. The generic parent
// is what the graph indexes as a type node; the parameterisation is
// a lookup detail we don't handle at this pass.
func stripGenerics(s string) string {
	s = strings.TrimSpace(s)
	if idx := strings.Index(s, "<"); idx >= 0 {
		return strings.TrimSpace(s[:idx])
	}
	return s
}
