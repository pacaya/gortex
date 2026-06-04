package contracts

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"

	"github.com/zzet/gortex/internal/parser"

	"github.com/zzet/gortex/internal/graph"
)

// -----------------------------------------------------------------------------
// Configurable HTTP-client wrapper aliases
// -----------------------------------------------------------------------------
//
// Many TS/JS codebases route every network call through a project-local
// helper — `apiGet('/users')`, `apiPost('/orders', body)`,
// `httpClient.request('GET', '/users')`. The built-in fetch/axios
// consumer patterns never see these because the call target is a
// user-defined function, so the endpoints stay invisible to the
// contract matcher. The `index.http_client_aliases` config lists those
// wrapper names; this pass mints a consumer contract for each call to
// one of them, using the SAME `http::METHOD::/path` ID scheme the
// matcher pairs against providers.
//
// Supported call shapes (alias = a configured name like `apiGet` or
// `client.request`):
//
//	apiGet('/users')                  → http::GET::/users
//	apiPost('/orders', body)          → http::POST::/orders
//	apiDelete(`/users/${id}`)         → http::DELETE::/users/{p1}
//	  (method from the alias name's verb suffix: get/post/put/delete/
//	   patch/head/options, case-insensitive)
//
//	apiCall('GET', '/users')          → http::GET::/users
//	client.request('POST', '/orders') → http::POST::/orders
//	  (alias name carries no verb suffix → the first string literal is
//	   read as the method when it is a bare HTTP verb, and the next
//	   string literal is the path)
//
// When the alias name has no verb suffix and the first literal isn't an
// HTTP verb, the method defaults to GET and the first string literal is
// the path — matching the lenient default the built-in `fetch(url)`
// pattern uses.

// srcMentionsAnyAlias reports whether src textually mentions any
// configured alias name (the bare tail of a dotted alias counts, so
// `client.request` is detected via `request`). A cheap substring gate
// that lets the prefilter keep an alias-only file alive without
// widening the regex scan for everyone else.
func srcMentionsAnyAlias(src []byte, aliases []string) bool {
	for _, a := range aliases {
		a = strings.TrimSpace(a)
		if a == "" {
			continue
		}
		if i := strings.LastIndex(a, "."); i >= 0 {
			a = a[i+1:]
		}
		if a != "" && bytes.Contains(src, []byte(a)) {
			return true
		}
	}
	return false
}

// aliasVerbSuffixRe pulls a trailing HTTP verb off an alias name so
// `apiGet` → GET, `fetchUsersPost` → POST. Anchored to the end and
// case-insensitive; the verb must be the literal tail of the name.
var aliasVerbSuffixRe = regexp.MustCompile(`(?i)(get|post|put|delete|patch|head|options)$`)

// aliasMethodLiteralRe matches a bare HTTP verb string literal used as
// the first argument of a generic alias like apiCall('GET', '/x').
var aliasMethodLiteralRe = regexp.MustCompile(`(?i)^(get|post|put|delete|patch|head|options)$`)

// detectClientAliasConsumers scans src for calls to any configured
// client-alias function and returns one consumer contract per call.
// The path is the first path-shaped string literal in the argument
// list; the method comes from the alias name's verb suffix or, for
// suffix-less aliases, a leading bare-verb literal argument.
func (h *HTTPExtractor) detectClientAliasConsumers(
	filePath string,
	text string,
	lines []string,
	fileNodes []*graph.Node,
	lang string,
	tree *parser.ParseTree,
) []Contract {
	var out []Contract
	seen := make(map[string]bool) // de-dupe (contractID, line) within the file

	for _, alias := range h.ClientAliases {
		alias = strings.TrimSpace(alias)
		if alias == "" {
			continue
		}
		re := aliasCallRegex(alias)
		if re == nil {
			continue
		}
		// The verb suffix is derived from the LAST dotted segment of
		// the alias so `client.get` → GET while `client.request` →
		// (none, generic two-arg form).
		nameForVerb := alias
		if i := strings.LastIndex(alias, "."); i >= 0 {
			nameForVerb = alias[i+1:]
		}
		method := ""
		if m := aliasVerbSuffixRe.FindString(nameForVerb); m != "" {
			method = strings.ToUpper(m)
		}

		for _, m := range re.FindAllStringSubmatchIndex(text, -1) {
			// m[2]:m[3] is the captured argument-list head (everything
			// from the opening paren to a reasonable bound).
			if len(m) < 4 || m[2] < 0 {
				continue
			}
			args := text[m[2]:m[3]]
			method, path, ok := aliasMethodAndPath(method, args)
			if !ok {
				continue
			}
			normPath, origNames := NormalizeHTTPPathWithParams(path)
			contractID := fmt.Sprintf("http::%s::%s", method, normPath)

			lineNum := lineAtOffset(lines, m[0])
			dedupeKey := fmt.Sprintf("%s@%d", contractID, lineNum)
			if seen[dedupeKey] {
				continue
			}
			seen[dedupeKey] = true

			meta := map[string]any{
				"method":    method,
				"path":      normPath,
				"framework": "client-alias",
				"alias":     alias,
			}
			if len(origNames) > 0 {
				meta["path_param_names"] = origNames
			}

			c := Contract{
				ID:         contractID,
				Type:       ContractHTTP,
				Role:       RoleConsumer,
				SymbolID:   findEnclosingSymbol(fileNodes, lineNum),
				FilePath:   filePath,
				Line:       lineNum,
				Meta:       meta,
				Confidence: 0.85,
			}
			// Enrich request/response types from the call-site window
			// just like a regex-detected consumer would.
			EnrichHTTPContractWithTree(&c, lines, fileNodes, lang, tree)

			out = append(out, c)
		}
	}
	return out
}

// aliasCallRegex compiles a matcher for a single alias name. The
// capture group is the argument-list head: from the char after the
// opening paren up to (but not including) the matching close paren or
// the end of the line, bounded so a missing close paren can't run
// away. Dotted aliases (`client.get`) match the literal receiver.method
// form; bare aliases match a free-standing call, optionally preceded by
// a non-identifier char so `myApiGet(` doesn't match alias `apiGet`.
func aliasCallRegex(alias string) *regexp.Regexp {
	// Escape each dotted segment, join with an escaped dot.
	segs := strings.Split(alias, ".")
	for i, s := range segs {
		segs[i] = regexp.QuoteMeta(s)
	}
	pat := strings.Join(segs, `\.`)
	// `(?:^|[^\w$.])` ensures we match a whole identifier — not a
	// suffix of a longer name (apiGet ≠ myApiGet) and not a property
	// access chain we didn't ask for. The capture stops at the first
	// `)` or newline; nested parens inside an argument (rare for a
	// path literal) would truncate the capture, which only loses the
	// trailing args we don't read anyway.
	full := `(?:^|[^\w$.])` + pat + `\(([^)\n]*)`
	re, err := regexp.Compile(full)
	if err != nil {
		return nil
	}
	return re
}

// aliasStringLitRe captures consecutive string literals (single, double,
// or backtick) from an argument-list head, in source order.
var aliasStringLitRe = regexp.MustCompile("[\"'`]([^\"'`]*)[\"'`]")

// aliasMethodAndPath resolves the (method, path) pair for one alias
// call. nameMethod is the verb derived from the alias name (empty when
// the name has no verb suffix). args is the captured argument-list head.
//
//   - nameMethod set: the first string literal is the path.
//   - nameMethod empty: if the first literal is a bare HTTP verb it is
//     the method and the second literal is the path
//     (apiCall('GET', '/x')); otherwise method defaults to GET and the
//     first literal is the path.
//
// Returns ok=false when no usable path literal is present (e.g. the
// argument is a runtime variable, not a string literal).
func aliasMethodAndPath(nameMethod, args string) (method, path string, ok bool) {
	lits := aliasStringLitRe.FindAllStringSubmatch(args, -1)
	if len(lits) == 0 {
		return "", "", false
	}
	if nameMethod != "" {
		return nameMethod, lits[0][1], true
	}
	// Suffix-less alias: try the (method, path) two-arg form.
	if len(lits) >= 2 && aliasMethodLiteralRe.MatchString(strings.TrimSpace(lits[0][1])) {
		return strings.ToUpper(strings.TrimSpace(lits[0][1])), lits[1][1], true
	}
	// Lenient default — first literal is the path, method GET.
	return "GET", lits[0][1], true
}
