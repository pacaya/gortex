// Package todos implements the comment-marker scanner described by
// spec-graph-coverage.md §5.12. It produces graph nodes and edges for
// TODO / FIXME / HACK / XXX / NOTE markers found in source comments.
//
// The scanner is intentionally simple: a per-line regex restricted to
// "comment context" lines (preceded only by whitespace and a known
// comment opener). String literals containing the word "TODO" are not
// matched because they have non-whitespace content before the opener.
// This is the v1 tradeoff — once every language parser exposes
// comment AST nodes, the scanner will switch to AST-driven extraction
// (see spec §10.1).
package todos

import (
	"bufio"
	"bytes"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// Finding is one matched marker. Line is 1-based.
type Finding struct {
	Tag      string // TODO / FIXME / HACK / XXX / NOTE
	Assignee string // value inside (parens) immediately after the tag, "" if absent
	Due      string // YYYY-MM-DD value inside [brackets] immediately after the tag, "" if absent
	Ticket   string // first #123 / JIRA-456 / ABC-123 reference found, "" if absent
	Text     string // remaining text after the tag/assignee/due, trimmed and capped at maxText
	Line     int    // 1-based line number in the file
}

// commentLine is the gating regex: line must start with whitespace,
// then a known opener (// or # or -- or /* or *), then whitespace,
// then one of the configured tags, then a word boundary. The tag
// alternation is built dynamically from the user's tag list.
//
// We capture two groups: (1) opener — discarded, kept for clarity;
// (2) the tag itself; (3) the rest of the line (assignee/due/text).
// The assignee/due/text split happens in parseRest so the regex stays
// readable and dialect-flexible.
var ticketRe = regexp.MustCompile(`(?:#\d+|[A-Z][A-Z0-9]+-\d+)`)

// dueRe matches a [YYYY-MM-DD] suffix immediately after the
// (assignee) (or after the tag if no assignee). We accept loose ISO
// dates; full RFC 3339 validation is over-engineering for a TODO
// scanner.
var dueRe = regexp.MustCompile(`^\[(\d{4}-\d{2}-\d{2})\]`)

// assigneeRe matches a (name) suffix immediately after the tag.
// Allowed chars: letters, digits, _, -, @, ., space — covers
// usernames, emails, and "first last".
var assigneeRe = regexp.MustCompile(`^\(([\w@.\-\s]+)\)`)

// Scan walks source line-by-line and returns markers in document
// order. tags is the set of marker tokens to recognise (case-
// sensitive); maxText caps the stored text length.
func Scan(source []byte, tags []string, maxText int) []Finding {
	if len(tags) == 0 || maxText <= 0 || len(source) == 0 {
		return nil
	}

	tagPattern := buildTagPattern(tags)
	if tagPattern == nil {
		return nil
	}

	var findings []Finding
	scanner := bufio.NewScanner(bytes.NewReader(source))
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := scanner.Text()
		match := tagPattern.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		tag := match[2]
		rest := strings.TrimSpace(line[len(match[0]):])
		assignee, due, text := parseRest(rest)
		ticket := ""
		if loc := ticketRe.FindString(rest); loc != "" {
			ticket = loc
		}
		if len(text) > maxText {
			text = text[:maxText]
		}
		findings = append(findings, Finding{
			Tag:      tag,
			Assignee: assignee,
			Due:      due,
			Ticket:   ticket,
			Text:     text,
			Line:     lineNum,
		})
	}
	return findings
}

// buildTagPattern constructs the per-line regex from the configured
// tag list. Tags are matched case-sensitively; this is intentional —
// "Todo" mid-comment ("// I'll Todo this later") should not match.
func buildTagPattern(tags []string) *regexp.Regexp {
	cleaned := make([]string, 0, len(tags))
	for _, t := range tags {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		cleaned = append(cleaned, regexp.QuoteMeta(t))
	}
	if len(cleaned) == 0 {
		return nil
	}
	// Comment openers we recognise:
	//   //   C-family line
	//   #    Python / shell / Ruby / YAML / TOML
	//   --   SQL / Lua / Haskell
	//   /*   C-family block start
	//   *    continuation line inside a /* ... */ block
	pattern := `^\s*(//|#|--|/\*|\*)\s+(` + strings.Join(cleaned, "|") + `)\b[:\s]*`
	return regexp.MustCompile(pattern)
}

// parseRest splits "(assignee)[2026-05-01] rest of text" into its
// pieces. Either prefix is optional; whichever appears first is
// consumed first. The fragment we keep as Text is the trimmed
// remainder.
func parseRest(rest string) (assignee, due, text string) {
	rest = strings.TrimSpace(rest)
	if m := assigneeRe.FindStringSubmatch(rest); m != nil {
		assignee = strings.TrimSpace(m[1])
		rest = strings.TrimSpace(rest[len(m[0]):])
	}
	if m := dueRe.FindStringSubmatch(rest); m != nil {
		due = m[1]
		rest = strings.TrimSpace(rest[len(m[0]):])
	}
	// Drop a leading colon left by patterns like "TODO(zzet): text".
	rest = strings.TrimLeft(rest, ":")
	rest = strings.TrimSpace(rest)
	text = rest
	return
}

// BuildGraphArtifacts converts findings for a single file into the
// node/edge pairs the indexer will append. The owning file node is
// linked via EdgeAnnotated so the existing decorator-walk machinery
// surfaces todos alongside @Deprecated, @Test, etc.
//
// IDs follow the existing graph convention `<file>::<name>` so the
// indexer's applyRepoPrefix pass can prepend the repo prefix
// uniformly without special-casing this kind. Within a file, todos
// are named `todo:<line>`. Different TODOs on the same line are
// disambiguated by appending #<seq> — a rare case but keeps IDs
// unique.
//
// filePath is the unprefixed (per-file extractor) path; the indexer
// adds the repo prefix downstream via applyRepoPrefix.
func BuildGraphArtifacts(filePath string, findings []Finding, language string) ([]*graph.Node, []*graph.Edge) {
	if len(findings) == 0 {
		return nil, nil
	}

	filePath = filepath.ToSlash(filePath)
	fileID := filePath
	nodes := make([]*graph.Node, 0, len(findings))
	edges := make([]*graph.Edge, 0, len(findings))
	seen := make(map[string]int)
	for _, f := range findings {
		baseID := todoNodeID(filePath, f.Line)
		id := baseID
		if n := seen[baseID]; n > 0 {
			id = baseID + "#" + intToString(n)
		}
		seen[baseID] = seen[baseID] + 1

		meta := map[string]any{
			"tag":  f.Tag,
			"text": f.Text,
		}
		if f.Assignee != "" {
			meta["assignee"] = f.Assignee
		}
		if f.Due != "" {
			meta["due"] = f.Due
		}
		if f.Ticket != "" {
			meta["ticket"] = f.Ticket
		}
		nodes = append(nodes, &graph.Node{
			ID:        id,
			Kind:      graph.KindTodo,
			Name:      "todo:" + intToString(f.Line),
			FilePath:  filePath,
			StartLine: f.Line,
			EndLine:   f.Line,
			Language:  language,
			Meta:      meta,
		})
		edges = append(edges, &graph.Edge{
			From:     fileID,
			To:       id,
			Kind:     graph.EdgeAnnotated,
			FilePath: filePath,
			Line:     f.Line,
			Origin:   graph.OriginASTResolved,
		})
	}
	return nodes, edges
}

// todoNodeID builds the canonical node ID for a TODO at (path, line).
// Format `<file>::todo:<line>` slots cleanly into applyRepoPrefix
// (which prepends "<repo>/" to the whole ID) and into nodeShort
// (which returns whatever follows the last "::"). We intentionally
// do not include the tag — the same line cannot hold two markers in
// practice, and including the tag would mean a rename from FIXME →
// TODO produces a new node ID rather than an in-place update.
func todoNodeID(filePath string, line int) string {
	return filePath + "::todo:" + intToString(line)
}

// intToString avoids pulling in strconv just for one int conversion.
// Allocation cost dominates over the conversion cost; this is in the
// hot path during full re-index.
func intToString(n int) string {
	if n == 0 {
		return "0"
	}
	neg := false
	if n < 0 {
		neg = true
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
