package contracts

import (
	"regexp"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// -----------------------------------------------------------------------------
// C# ASP.NET — controllers + minimal APIs
// -----------------------------------------------------------------------------
//
// ASP.NET has two routing styles, both covered:
//
//   Classic MVC:
//	 [HttpPost("/users")]
//	 public async Task<ActionResult<UserResp>> Create([FromBody] CreateUserReq req)
//
//   Minimal APIs (6.0+):
//	 app.MapPost("/users", async ([FromBody] CreateUserReq req) => Results.Ok(new UserResp()));
//
// Both produce the same underlying wire contract; extraction picks
// up `[FromBody]` / `[FromQuery]` / `[FromRoute]` + the handler's
// return type.

func init() {
	schemaEnrichers = append(schemaEnrichers,
		schemaEnricher{
			name:      "csharp-aspnet-provider",
			languages: []string{"csharp"},
			roles:     []Role{RoleProvider},
			detect:    csharpProviderDetect,
		},
		schemaEnricher{
			name:      "csharp-httpclient-consumer",
			languages: []string{"csharp"},
			roles:     []Role{RoleConsumer},
			detect:    csharpConsumerDetect,
		},
	)
}

var (
	// [FromBody] CreateReq body  |  [FromBody] CreateReq? body
	csharpFromBodyRe = regexp.MustCompile(`\[FromBody\](?:\([^)]*\))?\s+([A-Za-z_][\w<>.?]*)\s+\w+`)
	// [FromQuery] string foo  |  [FromQuery(Name = "x")] string foo
	csharpFromQueryRe = regexp.MustCompile(`\[FromQuery(?:\(Name\s*=\s*"([^"]+)"\))?\]\s+[A-Za-z_][\w<>.?]*\s+(\w+)`)
	// Method return type: `Task<ActionResult<T>>`, `ActionResult<T>`,
	// `Task<IActionResult>` — we unwrap envelopes.
	csharpReturnRe = regexp.MustCompile(
		`(?:public|private|protected|internal)\s+(?:async\s+)?([A-Za-z_][\w<>.,?\s]*)\s+\w+\s*\(`,
	)
	// [ProducesResponseType(StatusCodes.Status201Created)] etc.
	csharpStatusAttrRe = regexp.MustCompile(`StatusCodes\.Status(\d{3})\w*`)
	// return StatusCode(404, ...), return BadRequest()
	csharpStatusCallRe = regexp.MustCompile(`\bStatusCode\(\s*(\d{3})\b`)
	// return Ok(...), BadRequest(...), NotFound(...), etc.
	csharpResultHelperRe = regexp.MustCompile(`\breturn\s+(Ok|Created|Accepted|NoContent|BadRequest|Unauthorized|Forbid|NotFound|Conflict|UnprocessableEntity|InternalServerError)\b`)
	// Consumer: `_client.GetAsync("/x")` / `PostAsJsonAsync("/x", payload)`.
	csharpPostJsonRe   = regexp.MustCompile(`\bPostAsJsonAsync\(\s*"([^"]+)"\s*,\s*(\w+)`)
	csharpReadAsJSONRe = regexp.MustCompile(`\.ReadFromJsonAsync<\s*([A-Za-z_][\w.]*)\s*>`)
)

// csharpResultToCode maps the common ASP.NET result-helper names to
// their HTTP status codes. Same coverage as the attribute-driven
// StatusCodes class; these fire on idiomatic `return Ok(...)` etc.
var csharpResultToCode = map[string]int{
	"Ok":                  200,
	"Created":             201,
	"Accepted":            202,
	"NoContent":           204,
	"BadRequest":          400,
	"Unauthorized":        401,
	"Forbid":              403,
	"NotFound":            404,
	"Conflict":            409,
	"UnprocessableEntity": 422,
	"InternalServerError": 500,
}

func csharpProviderDetect(body string, fileNodes []*graph.Node) schemaHints {
	var h schemaHints

	if m := csharpFromBodyRe.FindStringSubmatch(body); len(m) > 1 {
		t := stripCSharpOptional(stripGenericsCSharp(m[1]))
		h.RequestType = resolveTypeInFile(t, fileNodes)
	}
	for _, m := range csharpFromQueryRe.FindAllStringSubmatch(body, -1) {
		wire := m[1]
		if wire == "" {
			wire = m[2]
		}
		if wire != "" {
			h.QueryParams = append(h.QueryParams, wire)
		}
	}
	// Response type from method signature. Unwrap ActionResult<T>,
	// Task<T>, Task<ActionResult<T>>, IActionResult (no inner type).
	if m := csharpReturnRe.FindStringSubmatch(body); len(m) > 1 {
		ret := unwrapCSharpEnvelopes(strings.TrimSpace(m[1]))
		if ret != "" && ret != "void" && ret != "IActionResult" && ret != "ActionResult" {
			h.ResponseType = resolveTypeInFile(stripCSharpOptional(stripGenericsCSharp(ret)), fileNodes)
		}
	}
	// Status codes.
	for _, m := range csharpStatusAttrRe.FindAllStringSubmatch(body, -1) {
		if code, ok := parseStatusExpr(m[1]); ok {
			h.StatusCodes = append(h.StatusCodes, code)
		}
	}
	for _, m := range csharpStatusCallRe.FindAllStringSubmatch(body, -1) {
		if code, ok := parseStatusExpr(m[1]); ok {
			h.StatusCodes = append(h.StatusCodes, code)
		}
	}
	for _, m := range csharpResultHelperRe.FindAllStringSubmatch(body, -1) {
		if code, ok := csharpResultToCode[m[1]]; ok {
			h.StatusCodes = append(h.StatusCodes, code)
		}
	}
	return h
}

func csharpConsumerDetect(body string, fileNodes []*graph.Node) schemaHints {
	var h schemaHints
	if m := csharpPostJsonRe.FindStringSubmatch(body); len(m) > 2 {
		if rt := findCSharpVarType(body, m[2]); rt != "" {
			h.RequestType = resolveTypeInFile(rt, fileNodes)
		}
	}
	if m := csharpReadAsJSONRe.FindStringSubmatch(body); len(m) > 1 {
		h.ResponseType = resolveTypeInFile(stripGenericsCSharp(m[1]), fileNodes)
	}
	return h
}

// unwrapCSharpEnvelopes peels `Task<>`, `ActionResult<>`, `Result<>`,
// and `ValueTask<>` so a return type like `Task<ActionResult<UserResp>>`
// collapses to `UserResp`.
func unwrapCSharpEnvelopes(t string) string {
	t = strings.TrimSpace(t)
	wrappers := []string{"Task", "ValueTask", "ActionResult", "Result", "Response", "ResponseMessage"}
	for {
		changed := false
		for _, w := range wrappers {
			pfx := w + "<"
			if strings.HasPrefix(t, pfx) && strings.HasSuffix(t, ">") {
				t = strings.TrimSpace(t[len(pfx) : len(t)-1])
				changed = true
				break
			}
		}
		if !changed {
			return t
		}
	}
}

// stripGenericsCSharp trims a trailing `<...>` so `List<User>` → `List`.
func stripGenericsCSharp(t string) string {
	t = strings.TrimSpace(t)
	if i := strings.Index(t, "<"); i >= 0 {
		return strings.TrimSpace(t[:i])
	}
	return t
}

// stripCSharpOptional removes a trailing `?` nullable-reference-type
// marker so `string?` → `string`.
func stripCSharpOptional(t string) string {
	return strings.TrimSuffix(strings.TrimSpace(t), "?")
}

// findCSharpVarType searches the body for a `var name = new Type(...)`
// or `Type name = ...` declaration and returns the type.
func findCSharpVarType(body, varName string) string {
	if varName == "" {
		return ""
	}
	v := regexp.QuoteMeta(varName)
	if m := regexp.MustCompile(`\bvar\s+` + v + `\s*=\s*new\s+([A-Za-z_][\w.]*)`).FindStringSubmatch(body); len(m) > 1 {
		return stripGenericsCSharp(m[1])
	}
	if m := regexp.MustCompile(`\b([A-Za-z_][\w.<>,\s]*)\s+` + v + `\s*=`).FindStringSubmatch(body); len(m) > 1 {
		return stripGenericsCSharp(m[1])
	}
	return ""
}
