package indexer

// shlexSplit tokenizes a POSIX-shell-style command line into arguments,
// honoring single quotes (fully literal), double quotes (with `\"` / `\\`
// escapes), and bare backslash escapes; unquoted whitespace separates tokens.
// It is intentionally minimal — enough to recover `-I"a b/inc"` and
// `-isystem /x` from a compile_commands.json `command` string when the
// structured `arguments` array is absent.
func shlexSplit(s string) []string {
	const (
		none = iota
		single
		double
	)
	var out []string
	var cur []rune
	inTok := false
	quote := none
	rs := []rune(s)
	flush := func() {
		if inTok {
			out = append(out, string(cur))
			cur = cur[:0]
			inTok = false
		}
	}
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		switch quote {
		case single:
			if c == '\'' {
				quote = none
			} else {
				cur = append(cur, c)
			}
			continue
		case double:
			switch {
			case c == '\\' && i+1 < len(rs) && (rs[i+1] == '"' || rs[i+1] == '\\'):
				i++
				cur = append(cur, rs[i])
			case c == '"':
				quote = none
			default:
				cur = append(cur, c)
			}
			continue
		}
		switch c {
		case ' ', '\t', '\n', '\r':
			flush()
		case '\'':
			quote = single
			inTok = true
		case '"':
			quote = double
			inTok = true
		case '\\':
			if i+1 < len(rs) {
				i++
				cur = append(cur, rs[i])
			}
			inTok = true
		default:
			cur = append(cur, c)
			inTok = true
		}
	}
	flush()
	return out
}
