package audit

import (
	"regexp"
	"strings"
)

// tokenKind classifies a backticked token as path, symbol, or other (ignored).
type tokenKind int

const (
	tokenOther tokenKind = iota
	tokenSymbol
	tokenPath
)

// extractBackticked returns all `backticked` spans on a line, without the
// enclosing backticks. Triple-backticks (code fences) are skipped.
func extractBackticked(line string) []string {
	// Skip fenced code block markers.
	trim := strings.TrimSpace(line)
	if strings.HasPrefix(trim, "```") {
		return nil
	}

	var out []string
	for {
		start := strings.Index(line, "`")
		if start < 0 {
			return out
		}
		// Don't match triple backticks mid-line.
		if strings.HasPrefix(line[start:], "```") {
			end := strings.Index(line[start+3:], "```")
			if end < 0 {
				return out
			}
			line = line[start+3+end+3:]
			continue
		}
		rest := line[start+1:]
		end := strings.Index(rest, "`")
		if end < 0 {
			return out
		}
		tok := strings.TrimSpace(rest[:end])
		if tok != "" {
			out = append(out, tok)
		}
		line = rest[end+1:]
	}
}

// Path-shaped: contains a path separator OR starts with a dot and has a dot
// further on (e.g. `.gortex.yaml`). URLs are excluded.
var (
	pathExts = map[string]bool{
		".go": true, ".ts": true, ".tsx": true, ".js": true, ".jsx": true,
		".py": true, ".rs": true, ".java": true, ".kt": true, ".rb": true,
		".cs": true, ".cpp": true, ".c": true, ".h": true, ".hpp": true,
		".md": true, ".yaml": true, ".yml": true, ".json": true, ".toml": true,
		".sh": true, ".ps1": true, ".lua": true, ".dart": true, ".swift": true,
		".html": true, ".css": true, ".sql": true, ".proto": true, ".xml": true,
	}

	// Identifier-shaped: CamelCase / PascalCase / snake_case / dotted / `()` suffix.
	// Require either a capital letter somewhere OR an explicit `()` suffix so we
	// don't flag every plain english word.
	identRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(?:[.:][A-Za-z_][A-Za-z0-9_]*)*(?:\(\))?$`)

	// Common shell/tool names and markdown-ish terms that get backticked but
	// are not symbol references.
	skipTokens = map[string]bool{
		"true": true, "false": true, "nil": true, "null": true, "None": true,
		"TODO": true, "FIXME": true, "NOTE": true, "XXX": true,
		"grep": true, "ls": true, "cd": true, "rm": true, "mv": true, "cp": true,
		"cat": true, "echo": true, "sed": true, "awk": true, "find": true,
		"git": true, "go": true, "npm": true, "yarn": true, "make": true,
		"docker": true, "curl": true, "wget": true, "ssh": true,
	}
)

func classifyToken(tok string) tokenKind {
	if tok == "" || len(tok) > 200 {
		return tokenOther
	}
	// Strip common markdown decorations that sometimes end up inside backticks.
	tok = strings.TrimSpace(tok)

	// URL? Ignore.
	if strings.Contains(tok, "://") {
		return tokenOther
	}
	// Whitespace inside a token (e.g. `POST /mcp`, `git status`) — the
	// content is a snippet, not an identifier. Skip before path
	// classification so a slash + space combination doesn't get
	// mistakenly read as a filesystem path.
	if strings.ContainsAny(tok, " \t") {
		return tokenOther
	}
	// Placeholder syntax inside docs (e.g. `<exact-name>`, `<kind>`) —
	// templated examples, not real identifiers.
	if strings.ContainsAny(tok, "<>") {
		return tokenOther
	}
	// Tokens carrying the Go-style `::` qualifier are always symbol
	// candidates even when they also contain a `/`. Without this gate
	// `pkg/foo.go::Bar` would land in tokenPath first and look up a
	// non-existent file rather than a non-existent symbol.
	if !strings.Contains(tok, "::") && isPathLike(tok) {
		return tokenPath
	}

	// Strip trailing `()` for ident classification.
	bare := strings.TrimSuffix(tok, "()")
	if skipTokens[bare] {
		return tokenOther
	}
	if !identRe.MatchString(tok) {
		return tokenOther
	}
	// SCREAMING_SNAKE / SCREAMING-DASH tokens (env var names, build-tag
	// constants, JSON-schema-style ALL_CAPS keys) are not Go symbols
	// in any graph we'd audit — agent configs cite them constantly
	// (ANTHROPIC_API_KEY, AWS_ACCESS_KEY_ID, NODE_TLS_REJECT_UNAUTHORIZED)
	// and the audit must not flag them as stale.
	if isScreamingSnake(bare) {
		return tokenOther
	}
	// Require a strong signal that this is a code symbol the graph
	// would carry: an uppercase letter (Go/Java/TS public ident),
	// the Go-style `::` qualifier, or an explicit `()` call suffix.
	// Pure-lowercase tokens with only `_` or `.` (e.g. `search_symbols`,
	// `meta.signature`, `older_than`) are MCP tool names, framework
	// option keys, and Python-style attribute paths — agent configs
	// reference them constantly but they are NOT Go symbols, so they
	// would otherwise dominate stale-ref reports as false positives.
	hasSignal := strings.HasSuffix(tok, "()") ||
		strings.Contains(bare, "::") ||
		hasUppercase(bare)
	if !hasSignal {
		return tokenOther
	}
	// A bare lowercase-first identifier (e.g. `generateContent`,
	// `responseSchema`, `additionalProperties`) is ambiguous — could
	// be a Go unexported method we own, could be a Google/AWS/JSON-
	// schema API name an agent config legitimately cites. Require a
	// stronger signal: a qualifier (`pkg.Sym` / `pkg::Sym`), an
	// explicit call form (`fn()`), or a capitalized first letter
	// (exported Go / Java / TS, where the docs-author convention is
	// to mean "this exported symbol"). Bare `handleFoo` no longer
	// qualifies — false positives from docs vocabulary used to drown
	// out the real signal.
	firstUpper := bare[0] >= 'A' && bare[0] <= 'Z'
	if !firstUpper && !strings.ContainsAny(bare, ".:") && !strings.HasSuffix(tok, "()") {
		return tokenOther
	}
	// Must be at least 3 chars to reduce false positives on 1-2 letter vars.
	if len(bare) < 3 {
		return tokenOther
	}
	return tokenSymbol
}

// isPathLike detects tokens that look like filesystem paths rather than
// symbols. Returns true only when the token carries a positive path
// signal — a known file extension, a leading `./` / `~` / `/`, or a
// recognised directory leaf. Bare slash-separated identifiers (e.g.
// `notifications/tools/list_changed`, `pkg/foo`) are NOT paths because
// the audit can't pathExists() them without a stat against the repo
// root, and slash-separated identifiers are common in MCP method names,
// JSON pointer fragments, and HTTP routes that the audit should leave
// alone.
func isPathLike(tok string) bool {
	if tok == "" {
		return false
	}
	// Explicit-path prefixes — `./relative`, `/absolute`, `~/home`.
	if strings.HasPrefix(tok, "./") || strings.HasPrefix(tok, "/") || strings.HasPrefix(tok, "~/") || strings.HasPrefix(tok, "~") && len(tok) > 1 && tok[1] == '/' {
		return true
	}
	if strings.Contains(tok, "\\") {
		return true
	}
	// Files with a known extension (e.g. `.gortex.yaml`, `README.md`,
	// `internal/foo.go`).
	if idx := strings.LastIndex(tok, "."); idx > 0 {
		ext := strings.ToLower(tok[idx:])
		if pathExts[ext] {
			return true
		}
	}
	return false
}

// isScreamingSnake reports whether tok is composed entirely of upper-
// case letters, digits, underscores, and dashes — the env-var /
// constant shape that agent configs cite but our symbol graphs don't
// carry as nodes.
func isScreamingSnake(tok string) bool {
	if tok == "" {
		return false
	}
	hasLetter := false
	for _, r := range tok {
		switch {
		case r >= 'A' && r <= 'Z':
			hasLetter = true
		case r >= '0' && r <= '9':
		case r == '_' || r == '-':
		default:
			return false
		}
	}
	return hasLetter
}

func hasUppercase(s string) bool {
	for _, r := range s {
		if r >= 'A' && r <= 'Z' {
			return true
		}
	}
	return false
}
