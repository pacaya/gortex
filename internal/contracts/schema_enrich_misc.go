package contracts

import (
	"regexp"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// -----------------------------------------------------------------------------
// Smaller language packs: Ruby / PHP / Elixir / Dart provider
//
// Route detection lives in httpPatterns (see http.go). This file
// holds the request/response/status extractors for the languages
// that didn't warrant a dedicated file. Each extractor is
// deliberately narrow — these ecosystems are mostly dynamically
// typed on the wire and the gains from deeper regex parsing are
// small. The goal is "catch the common idiom and degrade gracefully
// when we can't".
// -----------------------------------------------------------------------------

func init() {
	schemaEnrichers = append(schemaEnrichers,
		schemaEnricher{
			name:      "ruby-rails-provider",
			languages: []string{"ruby"},
			roles:     []Role{RoleProvider},
			detect:    rubyProviderDetect,
		},
		schemaEnricher{
			name:      "php-laravel-provider",
			languages: []string{"php"},
			roles:     []Role{RoleProvider},
			detect:    phpProviderDetect,
		},
		schemaEnricher{
			name:      "elixir-phoenix-provider",
			languages: []string{"elixir"},
			roles:     []Role{RoleProvider},
			detect:    elixirProviderDetect,
		},
		schemaEnricher{
			name:      "dart-shelf-provider",
			languages: []string{"dart"},
			roles:     []Role{RoleProvider},
			detect:    dartProviderDetect,
		},
	)
}

// -----------------------------------------------------------------------------
// Ruby on Rails
//
// Controllers are usually dynamically typed. What we can reliably
// recognise:
//   - `render json: foo, status: :created` — status symbol / code.
//   - `params.require(:user).permit(:a, :b)` — strong-params list.
// -----------------------------------------------------------------------------

var (
	rubyRenderStatusRe = regexp.MustCompile(`render\s+[^,\n]+,\s*status:\s*(?::([a-z_]+)|(\d{3}))`)
	rubyRenderJSONRe   = regexp.MustCompile(`render\s+json:\s*(@?\w+)`)
	rubyPermitRe       = regexp.MustCompile(`\.permit\(([^)]+)\)`)
)

// rubyStatusSymbols maps Rails' status symbols to their HTTP codes.
// Drawn from ActionDispatch::HTTP::StatusCodes.
var rubyStatusSymbols = map[string]int{
	"continue":              100,
	"ok":                    200,
	"created":               201,
	"accepted":              202,
	"no_content":            204,
	"moved_permanently":     301,
	"found":                 302,
	"see_other":             303,
	"not_modified":          304,
	"temporary_redirect":    307,
	"permanent_redirect":    308,
	"bad_request":           400,
	"unauthorized":          401,
	"forbidden":             403,
	"not_found":             404,
	"method_not_allowed":    405,
	"conflict":              409,
	"gone":                  410,
	"precondition_failed":   412,
	"unprocessable_entity":  422,
	"too_many_requests":     429,
	"internal_server_error": 500,
	"bad_gateway":           502,
	"service_unavailable":   503,
}

func rubyProviderDetect(body string, fileNodes []*graph.Node) schemaHints {
	var h schemaHints
	// Status codes from `render json: x, status: :created`.
	for _, m := range rubyRenderStatusRe.FindAllStringSubmatch(body, -1) {
		if m[1] != "" {
			if code, ok := rubyStatusSymbols[m[1]]; ok {
				h.StatusCodes = append(h.StatusCodes, code)
			}
		} else if m[2] != "" {
			if code, ok := parseStatusExpr(m[2]); ok {
				h.StatusCodes = append(h.StatusCodes, code)
			}
		}
	}
	// Response expression from `render json: @user`.
	if m := rubyRenderJSONRe.FindStringSubmatch(body); len(m) > 1 {
		h.ResponseExpr = "render json: " + m[1]
	}
	// Strong params → query param list (heuristic; Rails merges
	// POST body + URL params under the same `params` hash).
	if m := rubyPermitRe.FindStringSubmatch(body); len(m) > 1 {
		h.QueryParams = append(h.QueryParams, splitCommaSymbols(m[1])...)
	}
	_ = fileNodes // reserved for future type-node lookup
	return h
}

// splitCommaSymbols turns `:a, :b, :c` into ["a", "b", "c"].
func splitCommaSymbols(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		p := strings.TrimSpace(part)
		p = strings.TrimPrefix(p, ":")
		p = strings.TrimSuffix(p, ",")
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// -----------------------------------------------------------------------------
// PHP Laravel
//
// `return response()->json($foo, 201);`
// `$request->input('x')`, `$request->validate([ 'name' => 'required' ])`
// FormRequest class-based validation is opt-in; we don't chase it here.
// -----------------------------------------------------------------------------

var (
	// `response()->json($foo)` | `response()->json(['a' => 1])` with
	// optional trailing status code. The first arg is accepted as
	// any non-comma / non-paren expression so array literals like
	// `['id' => 'x']` pass through — then we pick up the status.
	// Paren depth isn't tracked (nested calls would confuse us) but
	// that's uncommon in response()->json().
	phpResponseJSONRe = regexp.MustCompile(`response\(\)->json\([^)]*?,\s*(\d{3})\b`)
	phpStatusCodeRe   = regexp.MustCompile(`->(?:setStatusCode|status)\(\s*(\d{3})\b`)
	phpInputRe        = regexp.MustCompile(`->input\(\s*['"](\w+)['"]`)
)

func phpProviderDetect(body string, fileNodes []*graph.Node) schemaHints {
	var h schemaHints
	for _, m := range phpResponseJSONRe.FindAllStringSubmatch(body, -1) {
		if len(m) > 1 && m[1] != "" {
			if code, ok := parseStatusExpr(m[1]); ok {
				h.StatusCodes = append(h.StatusCodes, code)
			}
		}
	}
	for _, m := range phpStatusCodeRe.FindAllStringSubmatch(body, -1) {
		if code, ok := parseStatusExpr(m[1]); ok {
			h.StatusCodes = append(h.StatusCodes, code)
		}
	}
	for _, m := range phpInputRe.FindAllStringSubmatch(body, -1) {
		h.QueryParams = append(h.QueryParams, m[1])
	}
	_ = fileNodes
	return h
}

// -----------------------------------------------------------------------------
// Elixir Phoenix
//
// `conn |> put_status(:created) |> json(user)` — pipe-chain style.
// -----------------------------------------------------------------------------

var (
	elixirPutStatusRe = regexp.MustCompile(`put_status\(\s*:([a-z_]+)\)`)
	elixirJsonRe      = regexp.MustCompile(`\|>\s*json\(\s*(\w+)\s*\)`)
)

func elixirProviderDetect(body string, fileNodes []*graph.Node) schemaHints {
	var h schemaHints
	for _, m := range elixirPutStatusRe.FindAllStringSubmatch(body, -1) {
		// Phoenix reuses Plug's symbol → code map, same as Rails.
		if code, ok := rubyStatusSymbols[m[1]]; ok {
			h.StatusCodes = append(h.StatusCodes, code)
		}
	}
	if m := elixirJsonRe.FindStringSubmatch(body); len(m) > 1 {
		h.ResponseExpr = "json(" + m[1] + ")"
	}
	_ = fileNodes
	return h
}

// -----------------------------------------------------------------------------
// Dart provider (shelf_router / shelf)
//
// `Response.ok(jsonEncode({'data': payload}));`
// `Response(201, body: jsonEncode(out));`
// -----------------------------------------------------------------------------

var (
	dartResponseOkRe     = regexp.MustCompile(`Response\.ok\(\s*jsonEncode\(\s*(\w+)`)
	dartResponseStatusRe = regexp.MustCompile(`Response\(\s*(\d{3})\b`)
)

func dartProviderDetect(body string, fileNodes []*graph.Node) schemaHints {
	var h schemaHints
	if m := dartResponseOkRe.FindStringSubmatch(body); len(m) > 1 {
		if rt := findDartVarType(body, m[1]); rt != "" {
			h.ResponseType = resolveTypeInFile(rt, fileNodes)
		}
		h.StatusCodes = append(h.StatusCodes, 200)
	}
	for _, m := range dartResponseStatusRe.FindAllStringSubmatch(body, -1) {
		if code, ok := parseStatusExpr(m[1]); ok {
			h.StatusCodes = append(h.StatusCodes, code)
		}
	}
	return h
}
