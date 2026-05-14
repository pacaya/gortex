package clones

import "strings"

// universalKeywords is a deliberately broad, language-agnostic set of
// control-flow and declaration keywords. Tokens in this set are kept
// verbatim during normalisation so the structural skeleton of a
// function body survives; every other identifier collapses to the
// placeholder "v". A language whose exotic keyword is missing here
// simply has that keyword normalised to "v" — detection degrades
// gracefully rather than breaking.
var universalKeywords = map[string]struct{}{
	// conditionals / loops
	"if": {}, "else": {}, "elif": {}, "elsif": {}, "unless": {},
	"for": {}, "while": {}, "do": {}, "loop": {}, "foreach": {},
	"switch": {}, "case": {}, "default": {}, "match": {}, "when": {},
	"break": {}, "continue": {}, "goto": {}, "return": {}, "yield": {},
	// declarations
	"func": {}, "function": {}, "fn": {}, "def": {}, "fun": {}, "sub": {}, "proc": {},
	"class": {}, "struct": {}, "interface": {}, "enum": {}, "trait": {},
	"impl": {}, "type": {}, "typedef": {}, "record": {}, "module": {}, "package": {},
	"var": {}, "let": {}, "const": {}, "final": {}, "val": {}, "static": {}, "mut": {},
	"public": {}, "private": {}, "protected": {}, "internal": {}, "abstract": {},
	"export": {}, "import": {}, "namespace": {}, "use": {}, "using": {}, "from": {},
	// objects
	"new": {}, "delete": {}, "this": {}, "self": {}, "super": {}, "extends": {},
	"implements": {}, "override": {}, "virtual": {},
	// errors / exceptions
	"try": {}, "catch": {}, "finally": {}, "throw": {}, "throws": {},
	"raise": {}, "except": {}, "rescue": {}, "ensure": {}, "defer": {}, "panic": {},
	"recover": {},
	// concurrency
	"async": {}, "await": {}, "go": {}, "chan": {}, "select": {}, "spawn": {},
	"synchronized": {}, "volatile": {},
	// boolean / logical
	"true": {}, "false": {}, "nil": {}, "null": {}, "none": {}, "undefined": {},
	"void": {}, "and": {}, "or": {}, "not": {}, "in": {}, "is": {}, "as": {},
	"of": {}, "with": {}, "where": {}, "then": {}, "begin": {}, "end": {},
	"lambda": {}, "where_": {},
}

// operatorRunChars are the punctuation characters that can chain into a
// single multi-character operator token (==, !=, :=, <=, &&, ->, =>,
// ::, ++, <<, etc.). Brackets, braces, parentheses, commas and
// semicolons are intentionally excluded — each is its own single-char
// token so call/block structure stays granular.
const operatorRunChars = "+-*/%=<>!&|^~?:.@"

// Tokenize reduces a source body to a normalised, language-agnostic
// token stream:
//
//   - identifier in universalKeywords  → the lower-cased keyword
//   - any other identifier             → "v"
//   - numeric literal                  → "0"
//   - string / char / raw literal      → "s"
//   - run of operator characters       → the verbatim run (e.g. "==")
//   - single bracket / brace / paren / comma / semicolon → itself
//
// Whitespace is dropped. Comments are not stripped — copy-pasted
// clones carry copy-pasted comments, and the Jaccard threshold absorbs
// the occasional divergence. The result is deterministic and depends
// only on the input bytes.
func Tokenize(body string) []string {
	tokens := make([]string, 0, len(body)/4+1)
	runes := []rune(body)
	n := len(runes)
	i := 0
	for i < n {
		c := runes[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v':
			i++
		case isIdentStart(c):
			j := i + 1
			for j < n && isIdentPart(runes[j]) {
				j++
			}
			word := string(runes[i:j])
			lower := strings.ToLower(word)
			if _, ok := universalKeywords[lower]; ok {
				tokens = append(tokens, lower)
			} else {
				tokens = append(tokens, "v")
			}
			i = j
		case c >= '0' && c <= '9':
			j := i + 1
			for j < n && isNumberPart(runes[j]) {
				j++
			}
			tokens = append(tokens, "0")
			i = j
		case c == '"' || c == '\'' || c == '`':
			i = skipStringLiteral(runes, i)
			tokens = append(tokens, "s")
		case strings.ContainsRune(operatorRunChars, c):
			j := i + 1
			for j < n && strings.ContainsRune(operatorRunChars, runes[j]) {
				j++
			}
			tokens = append(tokens, string(runes[i:j]))
			i = j
		case c == '(' || c == ')' || c == '[' || c == ']' ||
			c == '{' || c == '}' || c == ',' || c == ';':
			tokens = append(tokens, string(c))
			i++
		default:
			// Unknown punctuation / non-ASCII symbol — keep it as a
			// single token so it still contributes to the shape.
			tokens = append(tokens, string(c))
			i++
		}
	}
	return tokens
}

// skipStringLiteral returns the index just past a string, char, or raw
// literal starting at runes[start]. Backslash escapes are honoured for
// "/' quotes; backtick raw strings run to the next backtick. An
// unterminated literal consumes to end-of-input.
func skipStringLiteral(runes []rune, start int) int {
	quote := runes[start]
	i := start + 1
	n := len(runes)
	for i < n {
		c := runes[i]
		if c == '\\' && quote != '`' {
			i += 2
			continue
		}
		if c == quote {
			return i + 1
		}
		i++
	}
	return n
}

func isIdentStart(c rune) bool {
	return c == '_' || c == '$' ||
		(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
		c > 127
}

func isIdentPart(c rune) bool {
	return isIdentStart(c) || (c >= '0' && c <= '9')
}

func isNumberPart(c rune) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') ||
		(c >= 'A' && c <= 'F') || c == '.' || c == '_' ||
		c == 'x' || c == 'X' || c == 'o' || c == 'O' ||
		c == 'b' || c == 'B' || c == 'e' || c == 'E'
}
