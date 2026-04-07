package search

import (
	"strings"
	"unicode"
)

// Tokenize splits a string into searchable tokens by:
// - camelCase boundaries (getUserById → [get, user, by, id])
// - snake_case boundaries (get_user_by_id → [get, user, by, id])
// - path separators (internal/auth/token.go → [internal, auth, token, go])
// - dot separators (UserService.FindUser → [user, service, find, user])
//
// All tokens are lowercased. Tokens shorter than 2 characters are dropped.
func Tokenize(s string) []string {
	var tokens []string
	var current []rune

	flush := func() {
		if len(current) >= 2 {
			tokens = append(tokens, strings.ToLower(string(current)))
		}
		current = current[:0]
	}

	runes := []rune(s)
	for i, r := range runes {
		switch {
		case r == '_' || r == '/' || r == '.' || r == ':' || r == '-' || r == ' ':
			flush()
		case unicode.IsUpper(r):
			if len(current) > 0 {
				prev := current[len(current)-1]
				// camelCase: lowercase followed by uppercase → split
				if unicode.IsLower(prev) {
					flush()
				}
				// ALLCAPS followed by uppercase+lowercase → split before the last uppercase
				// e.g. HTMLParser: H-T-M-L-P → flush "HTML" before "P"
				if unicode.IsUpper(prev) && i+1 < len(runes) && unicode.IsLower(runes[i+1]) {
					flush()
				}
			}
			current = append(current, r)
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			current = append(current, r)
		default:
			flush()
		}
	}
	flush()

	if tokens == nil {
		tokens = []string{}
	}
	return tokens
}

// TokenizeQuery tokenizes a search query, keeping all tokens (including short ones
// that might be meaningful like "go" or "js").
func TokenizeQuery(s string) []string {
	var tokens []string
	var current []rune

	flush := func() {
		if len(current) > 0 {
			tokens = append(tokens, strings.ToLower(string(current)))
		}
		current = current[:0]
	}

	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' {
			current = append(current, r)
		} else {
			flush()
		}
	}
	flush()
	return tokens
}
