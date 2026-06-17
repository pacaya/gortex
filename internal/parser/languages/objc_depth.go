package languages

import (
	"regexp"
	"strings"
)

var (
	// objcPropertyRe captures the declared name of an @property (the last
	// identifier before the terminating semicolon).
	objcPropertyRe = regexp.MustCompile(`(?m)^\s*@property\b[^;\n]*?\b([A-Za-z_]\w*)\s*;`)
	// objcTypedefRe captures the type name of a typedef NS_ENUM / NS_OPTIONS.
	objcTypedefRe = regexp.MustCompile(`(?m)^\s*typedef\s+NS_(?:ENUM|OPTIONS)\s*\(\s*\w+\s*,\s*([A-Za-z_]\w*)\s*\)`)

	objcMsgKeywordRe = regexp.MustCompile(`([A-Za-z_]\w*)\s*:`)
	objcMsgUnaryRe   = regexp.MustCompile(`^\s*([A-Za-z_]\w*)\s*$`)
)

// objcSpan is an id with a 1-based inclusive line range.
type objcSpan struct {
	id    string
	start int
	end   int
}

// objcEnclosing returns the id of the smallest span containing line, or "".
func objcEnclosing(spans []objcSpan, line int) string {
	best := ""
	bestSize := 1 << 30
	for _, s := range spans {
		if line >= s.start && line <= s.end {
			if sz := s.end - s.start; sz < bestSize {
				bestSize = sz
				best = s.id
			}
		}
	}
	return best
}

// objcMethodRanges returns the line span of every Objective-C method (keyed by
// its file-qualified selector id), used to attribute message-sends.
func objcMethodRanges(src []byte, lines []string, filePath string) []objcSpan {
	var spans []objcSpan
	for _, m := range objcMethodRe.FindAllSubmatchIndex(src, -1) {
		if m[4] < 0 {
			continue
		}
		sel := objcBuildSelector(string(src[m[4]:m[5]]))
		if sel == "" {
			continue
		}
		line := lineAt(src, m[0])
		spans = append(spans, objcSpan{id: filePath + "::" + sel, start: line, end: findBlockEnd(lines, line)})
	}
	return spans
}

// objcClassRanges returns the line span of every @interface / @implementation
// block, used to attribute @property fields to their owning class.
func objcClassRanges(src []byte, lines []string, filePath string) []objcSpan {
	var spans []objcSpan
	for _, re := range []*regexp.Regexp{objcInterfaceRe, objcImplRe} {
		for _, m := range re.FindAllSubmatchIndex(src, -1) {
			name := string(src[m[2]:m[3]])
			line := lineAt(src, m[0])
			spans = append(spans, objcSpan{id: filePath + "::" + name, start: line, end: findKeywordBlockEnd(lines, line, "@end")})
		}
	}
	return spans
}

// objcMsgSend is one reconstructed message-send: a receiver, its canonical
// selector, and the 1-based line it appears on.
type objcMsgSend struct {
	receiver string
	selector string
	line     int
}

// objcMessageSends scans src for [receiver selector ...] message-sends and
// reconstructs each canonical selector (colon-joined keyword labels for keyword
// messages, the bare name for unary messages). Nested sends are captured because
// the scan does not skip past a bracket's contents.
func objcMessageSends(src []byte) []objcMsgSend {
	s := string(src)
	var sends []objcMsgSend
	for i := 0; i < len(s); i++ {
		if s[i] != '[' {
			continue
		}
		end := objcMatchBracket(s, i)
		if end < 0 {
			continue
		}
		if recv, sel := objcParseMessage(s[i+1 : end]); recv != "" && sel != "" {
			sends = append(sends, objcMsgSend{receiver: recv, selector: sel, line: strings.Count(s[:i], "\n") + 1})
		}
	}
	return sends
}

// objcReturnType extracts the method return type from the text between the
// +/- sign and the selector (e.g. "- (nullable NSArray<User *> *)") — the type
// inside the parentheses with nullability/ARC qualifiers, pointer stars, and
// generic arguments stripped.
func objcReturnType(prefix []byte) string {
	s := string(prefix)
	open := strings.IndexByte(s, '(')
	if open < 0 {
		return ""
	}
	rel := strings.IndexByte(s[open:], ')')
	if rel < 0 {
		return ""
	}
	rt := s[open+1 : open+rel]
	for _, q := range []string{"nullable", "_Nonnull", "_Nullable", "__kindof", "const", "oneway", "bycopy", "byref"} {
		rt = strings.ReplaceAll(rt, q, " ")
	}
	rt = strings.ReplaceAll(rt, "*", " ")
	if i := strings.IndexByte(rt, '<'); i >= 0 {
		rt = rt[:i]
	}
	return strings.TrimSpace(strings.Join(strings.Fields(rt), " "))
}

// objcMatchBracket returns the index of the ']' that closes the '[' at open.
func objcMatchBracket(s string, open int) int {
	depth := 0
	for i := open; i < len(s); i++ {
		switch s[i] {
		case '[':
			depth++
		case ']':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// objcParseMessage splits a bracket body into its receiver and canonical
// selector. Returns ("","") when it is not a message-send (e.g. an array
// subscript or collection literal element).
func objcParseMessage(inner string) (receiver, selector string) {
	inner = strings.TrimSpace(inner)
	sp := strings.IndexAny(inner, " \t\n")
	if sp < 0 {
		return "", ""
	}
	receiver = strings.TrimSpace(inner[:sp])
	rest := inner[sp:]
	if labels := objcMsgKeywordRe.FindAllStringSubmatch(rest, -1); len(labels) > 0 {
		var b strings.Builder
		for _, l := range labels {
			b.WriteString(l[1])
			b.WriteByte(':')
		}
		return receiver, b.String()
	}
	if m := objcMsgUnaryRe.FindStringSubmatch(rest); m != nil {
		return receiver, m[1]
	}
	return "", ""
}
