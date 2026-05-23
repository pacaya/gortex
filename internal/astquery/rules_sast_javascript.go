package astquery

// JavaScript / TypeScript SAST rules. Patterns are written once and
// shipped to both grammars (the JS tree-sitter grammar covers the
// subset shared with TS; the TS grammar handles the type-suffixed
// constructs separately). Each rule's Pat carries both keys.

func init() {
	registerJSEvalFunction()
	registerJSDangerousSinks()
	registerJSReactDangerouslySetHTML()
	registerJSChildProcessExec()
	registerJSRequireWithVariable()
	registerJSCookieFlags()
	registerJSCORSWildcard()
	registerJSPostMessageWildcard()
	registerJSWeakCryptoSubtle()
	registerJSMathRandomForToken()
	registerJSNoTLSReject()
	registerJSJWTNone()
	registerJSDocumentWrite()
	registerJSTargetBlankNoopener()
	registerJSLocationAssignUserInput()
}

// 1. eval / new Function — CWE-95
func registerJSEvalFunction() {
	pat := `((call_expression function: (identifier) @fn) @match (#match? @fn "^(eval)$"))
            ((new_expression constructor: (identifier) @fn) @match (#eq? @fn "Function"))`
	mustRegisterSAST(sastRule{
		Name:        "js-eval-use",
		Description: "`eval(...)` / `new Function(...)` — executes an arbitrary string as JS. Any caller-controlled input is RCE.",
		Severity:    "error",
		CWE:         "CWE-95",
		OWASP:       "A03:2021-Injection",
		Tags:        []string{"injection", "code-injection"},
		Pat:         map[string]string{"javascript": pat, "typescript": pat},
	})
}

// 2. dangerous DOM sinks — CWE-79
func registerJSDangerousSinks() {
	pat := `((assignment_expression
              left: (member_expression property: (property_identifier) @prop)) @match
            (#match? @prop "^(innerHTML|outerHTML|insertAdjacentHTML)$"))`
	mustRegisterSAST(sastRule{
		Name:        "js-dom-innerhtml-assignment",
		Description: "Direct assignment to `.innerHTML` / `.outerHTML` / `.insertAdjacentHTML` — bypasses the browser's auto-escaping. Use `textContent` or a sanitizer (DOMPurify).",
		Severity:    "warning",
		CWE:         "CWE-79",
		OWASP:       "A03:2021-Injection",
		Tags:        []string{"xss", "dom"},
		Pat:         map[string]string{"javascript": pat, "typescript": pat},
	})
}

func registerJSDocumentWrite() {
	pat := `((call_expression
              function: (member_expression object: (identifier) @obj property: (property_identifier) @prop)) @match
            (#eq? @obj "document") (#match? @prop "^(write|writeln)$"))`
	mustRegisterSAST(sastRule{
		Name:        "js-document-write",
		Description: "`document.write(...)` / `document.writeln(...)` — writes raw HTML into the DOM, identical XSS risk as `innerHTML=`. Blocked by Trusted Types when enabled.",
		Severity:    "error",
		CWE:         "CWE-79",
		Tags:        []string{"xss", "dom", "deprecated"},
		Pat:         map[string]string{"javascript": pat, "typescript": pat},
	})
}

// 3. React dangerouslySetInnerHTML
func registerJSReactDangerouslySetHTML() {
	mustRegisterSAST(sastRule{
		Name:        "js-react-dangerously-set-inner-html",
		Description: "JSX `dangerouslySetInnerHTML={{__html: x}}` — React's deliberate XSS escape hatch. Any caller-controlled `x` is XSS. Use DOMPurify or render as text.",
		Severity:    "error",
		CWE:         "CWE-79",
		OWASP:       "A03:2021-Injection",
		Tags:        []string{"xss", "react"},
		Pat: map[string]string{
			"javascript": `((jsx_attribute (property_identifier) @attr) @match (#eq? @attr "dangerouslySetInnerHTML"))`,
			// tsx (not typescript) — jsx_attribute only exists in the
			// JSX-aware grammar. .tsx targets are retagged as "tsx"
			// upstream so this query compiles cleanly.
			"tsx": `((jsx_attribute (property_identifier) @attr) @match (#eq? @attr "dangerouslySetInnerHTML"))`,
		},
	})
}

// 4. child_process.exec / execSync / spawn shell:true
func registerJSChildProcessExec() {
	pat := `((call_expression
              function: (member_expression object: (identifier) @obj property: (property_identifier) @fn)) @match
            (#match? @obj "^(child_process|cp)$") (#match? @fn "^(exec|execSync)$"))`
	mustRegisterSAST(sastRule{
		Name:        "js-child-process-exec",
		Description: "`child_process.exec(cmd)` / `execSync(cmd)` — runs `cmd` through `/bin/sh -c` (or `cmd.exe /s /c`). Any caller-controlled portion is command injection. Use `execFile` with an argv array.",
		Severity:    "error",
		CWE:         "CWE-78",
		OWASP:       "A03:2021-Injection",
		Tags:        []string{"command-injection", "node"},
		Pat:         map[string]string{"javascript": pat, "typescript": pat},
	})
}

// 5. require with a variable — module-load hijack
func registerJSRequireWithVariable() {
	pat := `((call_expression function: (identifier) @fn
              arguments: (arguments (identifier) @arg)) @match
            (#eq? @fn "require"))`
	mustRegisterSAST(sastRule{
		Name:        "js-require-with-variable",
		Description: "`require(varName)` — module path comes from a variable. Caller-controlled values are arbitrary file load / require.cache poisoning. Prefer static `require('./fixed-path')`.",
		Severity:    "warning",
		CWE:         "CWE-915",
		Tags:        []string{"dynamic-import", "node"},
		Pat:         map[string]string{"javascript": pat, "typescript": pat},
	})
}

// 6. Cookie flags — secure=false / httpOnly=false
func registerJSCookieFlags() {
	pat := `((object (pair key: (property_identifier) @key value: (false))) @match
            (#match? @key "^(secure|httpOnly|httpolnly)$"))`
	mustRegisterSAST(sastRule{
		Name:        "js-cookie-no-secure-or-httponly",
		Description: "Cookie option `{ secure: false }` or `{ httpOnly: false }` — secure=false transmits the cookie over plaintext HTTP; httpOnly=false exposes it to `document.cookie`-reading XSS. Flip to true unless a JS reader is genuinely required.",
		Severity:    "warning",
		CWE:         "CWE-614",
		Tags:        []string{"cookies"},
		Pat:         map[string]string{"javascript": pat, "typescript": pat},
	})
}

// 7. CORS allow-all + credentials true
func registerJSCORSWildcard() {
	pat := `((call_expression
              function: (identifier) @fn
              arguments: (arguments (object) @opts)) @match
            (#match? @fn "^(cors|Cors)$"))`
	mustRegisterSAST(sastRule{
		Name:        "js-cors-wildcard-with-credentials",
		Description: "`cors({ origin: '*', credentials: true })` — invalid per the CORS spec, and many libraries fall back to reflecting the request `Origin` header. Use an explicit allow-list.",
		Severity:    "info",
		CWE:         "CWE-942",
		Tags:        []string{"cors", "audit-hook"},
		Pat:         map[string]string{"javascript": pat, "typescript": pat},
	})
}

// 8. postMessage with target="*"
func registerJSPostMessageWildcard() {
	pat := `((call_expression
              function: (member_expression property: (property_identifier) @fn)
              arguments: (arguments . (_) (string) @target)) @match
            (#eq? @fn "postMessage") (#match? @target "\"\\*\""))`
	mustRegisterSAST(sastRule{
		Name:        "js-postmessage-wildcard-target",
		Description: "`window.postMessage(data, '*')` — any iframe can receive the message. Use an explicit origin (`'https://example.com'`).",
		Severity:    "warning",
		CWE:         "CWE-346",
		Tags:        []string{"postmessage"},
		Pat:         map[string]string{"javascript": pat, "typescript": pat},
	})
}

// 9. crypto.createHash('md5'|'sha1')
func registerJSWeakCryptoSubtle() {
	pat := `((call_expression
              function: (member_expression object: (identifier) @obj property: (property_identifier) @fn)
              arguments: (arguments (string) @algo)) @match
            (#eq? @obj "crypto") (#eq? @fn "createHash") (#match? @algo "[\"'](md5|sha1)[\"']"))`
	mustRegisterSAST(sastRule{
		Name:        "js-crypto-weak-hash",
		Description: "`crypto.createHash('md5')` / `crypto.createHash('sha1')` — cryptographically broken hashes. Use `'sha256'` or `'sha3-256'`.",
		Severity:    "error",
		CWE:         "CWE-327",
		Tags:        []string{"crypto", "weak-hash"},
		Pat:         map[string]string{"javascript": pat, "typescript": pat},
	})
}

// 10. Math.random for tokens / IDs
func registerJSMathRandomForToken() {
	pat := `((call_expression
              function: (member_expression object: (identifier) @obj property: (property_identifier) @fn)) @match
            (#eq? @obj "Math") (#eq? @fn "random"))`
	mustRegisterSAST(sastRule{
		Name:        "js-math-random-for-token",
		Description: "`Math.random()` — not cryptographically secure. Audit hook for any site that produces an ID / token / nonce. Use `crypto.randomUUID()` / `crypto.getRandomValues()`.",
		Severity:    "info",
		CWE:         "CWE-338",
		Tags:        []string{"crypto", "audit-hook"},
		Pat:         map[string]string{"javascript": pat, "typescript": pat},
	})
}

// 11. NODE_TLS_REJECT_UNAUTHORIZED=0
func registerJSNoTLSReject() {
	pat := `((property_identifier) @match (#eq? @match "NODE_TLS_REJECT_UNAUTHORIZED"))
            ((pair key: (property_identifier) @key value: (false)) @match (#eq? @key "rejectUnauthorized"))`
	mustRegisterSAST(sastRule{
		Name:        "js-no-tls-reject-unauthorized",
		Description: "`process.env.NODE_TLS_REJECT_UNAUTHORIZED = '0'` / `rejectUnauthorized: false` — disables Node.js TLS verification globally. Anything beyond a dev script is shipping a backdoor.",
		Severity:    "error",
		CWE:         "CWE-295",
		Tags:        []string{"tls", "no-verify"},
		Pat:         map[string]string{"javascript": pat, "typescript": pat},
	})
}

func registerJSTargetBlankNoopener() {
	pat := `((jsx_attribute (property_identifier) @attr (string) @val) @match
            (#eq? @attr "target") (#match? @val "[\"']_blank[\"']"))`
	mustRegisterSAST(sastRule{
		Name:        "js-target-blank-no-noopener",
		Description: "`<a target=\"_blank\">` without `rel=\"noopener noreferrer\"` lets the opened page navigate the opener via `window.opener`. Add the rel attribute or use `rel=\"noreferrer\"`.",
		Severity:    "info",
		CWE:         "CWE-1022",
		Tags:        []string{"phishing"},
		// tsx (not typescript) — jsx_attribute only exists in the
		// JSX-aware grammar. .tsx targets are retagged as "tsx"
		// upstream so this query compiles cleanly.
		Pat: map[string]string{"javascript": pat, "tsx": pat},
	})
}

func registerJSLocationAssignUserInput() {
	pat := `((assignment_expression
              left: (member_expression object: (identifier) @loc property: (property_identifier) @prop)) @match
            (#eq? @loc "location") (#match? @prop "^(href|search|pathname)$"))
            ((assignment_expression
              left: (member_expression
                object: (member_expression object: (identifier) @win property: (property_identifier) @loc)
                property: (property_identifier) @prop)) @match
            (#eq? @win "window") (#eq? @loc "location") (#match? @prop "^(href|search|pathname)$"))`
	mustRegisterSAST(sastRule{
		Name:        "js-location-href-assignment",
		Description: "`location.href = x` / `window.location.href = x` — if `x` comes from URL params it can be an `javascript:` URL → XSS. Validate the scheme allow-list.",
		Severity:    "info",
		CWE:         "CWE-79",
		Tags:        []string{"xss", "open-redirect"},
		Pat:         map[string]string{"javascript": pat, "typescript": pat},
	})
}

// 12. JWT alg=none / verify:false
func registerJSJWTNone() {
	pat := `((object (pair key: (property_identifier) @key value: (string) @val)) @match
            (#eq? @key "algorithm") (#match? @val "[\"']none[\"']"))`
	mustRegisterSAST(sastRule{
		Name:        "js-jwt-none-alg",
		Description: "`{ algorithm: 'none' }` — produces / accepts unsigned JWTs. Trivial impersonation; reject any non-`HS256` / `RS256` / `EdDSA` algorithm explicitly.",
		Severity:    "error",
		CWE:         "CWE-347",
		Tags:        []string{"jwt", "broken-auth"},
		Pat:         map[string]string{"javascript": pat, "typescript": pat},
	})
}
