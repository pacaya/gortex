package main

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/progress"
)

// writeIndexTextSummary emits one line per path with file/node/edge totals
// and duration; if the result has errors, they're listed indented below in
// red (or plain text on non-TTY stdout — lipgloss strips colors when the
// output isn't a terminal).
func writeIndexTextSummary(w io.Writer, path string, r *indexer.IndexResult) {
	stats := []string{
		progress.Stat(humanizeInt(r.FileCount), "files", progress.StatNeutral),
		progress.Stat(humanizeInt(r.NodeCount), "nodes", progress.StatNeutral),
		progress.Stat(humanizeInt(r.EdgeCount), "edges", progress.StatNeutral),
		progress.Stat(strconv.FormatInt(r.DurationMs, 10)+"ms", "", progress.StatGood),
	}
	if len(r.Errors) > 0 {
		stats = append(stats, progress.Stat(humanizeInt(len(r.Errors)), "errors", progress.StatBad))
	}

	_, _ = fmt.Fprintf(w, "  %s   %s\n", progress.Row(path, "", 4), progress.StatStrip(stats...))
	writeIndexErrorBreakdown(w, r)
}

// indexBreakdownErrorCap bounds how many distinct error reasons the breakdown
// lists before collapsing the tail into a "+N more" line.
const indexBreakdownErrorCap = 12

// writeIndexErrorBreakdown prints the parse-failure picture for an index pass:
// the crash-isolation / size-cap counts the indexer surfaces as dedicated
// fields, then the parse errors grouped by a normalised reason (so "N files
// failed: unterminated string" reads at a glance instead of N raw lines). A
// clean pass prints nothing.
func writeIndexErrorBreakdown(w io.Writer, r *indexer.IndexResult) {
	var caps []string
	if r.QuarantinedFiles > 0 {
		caps = append(caps, fmt.Sprintf("%s quarantined (parser crash/timeout)", humanizeInt(r.QuarantinedFiles)))
	}
	if r.SkippedFiles > 0 {
		caps = append(caps, fmt.Sprintf("%s skipped (size/timeout cap)", humanizeInt(r.SkippedFiles)))
	}
	if n := len(r.FailedFiles); n > 0 {
		caps = append(caps, fmt.Sprintf("%s failed after retry", humanizeInt(n)))
	}
	if len(caps) > 0 {
		_, _ = fmt.Fprintf(w, "      %s\n", progress.Caption("breakdown: "+strings.Join(caps, " · ")))
	}

	if len(r.Errors) == 0 {
		return
	}
	// Group by a normalised reason so a class of failures collapses to one line.
	counts := make(map[string]int, len(r.Errors))
	for _, e := range r.Errors {
		counts[normalizeIndexErrorReason(e.Error)]++
	}
	reasons := make([]string, 0, len(counts))
	for reason := range counts {
		reasons = append(reasons, reason)
	}
	sort.Slice(reasons, func(i, j int) bool {
		if counts[reasons[i]] != counts[reasons[j]] {
			return counts[reasons[i]] > counts[reasons[j]] // most frequent first
		}
		return reasons[i] < reasons[j]
	})
	shown := reasons
	if len(shown) > indexBreakdownErrorCap {
		shown = shown[:indexBreakdownErrorCap]
	}
	for _, reason := range shown {
		_, _ = fmt.Fprintf(w, "      %s\n", progress.Caption(fmt.Sprintf("%s × %s", humanizeInt(counts[reason]), reason)))
	}
	if extra := len(reasons) - len(shown); extra > 0 {
		_, _ = fmt.Fprintf(w, "      %s\n", progress.Caption(fmt.Sprintf("+%d more error reasons", extra)))
	}
}

// normalizeIndexErrorReason collapses an error message to a coarse, groupable
// reason: the text before the first ':' (which is usually the file/offset
// detail), trimmed and lower-cased, so per-file specifics don't fragment the
// breakdown. Returns "parse error" for an empty message.
func normalizeIndexErrorReason(msg string) string {
	msg = strings.TrimSpace(msg)
	if msg == "" {
		return "parse error"
	}
	if i := strings.Index(msg, ":"); i > 0 {
		msg = msg[:i]
	}
	return strings.ToLower(strings.TrimSpace(msg))
}

// humanizeInt returns the integer with thousands separators ("1234" → "1,234").
// Works for any integer-like value.
func humanizeInt[T int | int32 | int64 | uint32 | uint64](v T) string {
	s := strconv.FormatInt(int64(v), 10)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	if len(s) <= 3 {
		if neg {
			return "-" + s
		}
		return s
	}
	var b strings.Builder
	rem := len(s) % 3
	if rem > 0 {
		b.WriteString(s[:rem])
	}
	for i := rem; i < len(s); i += 3 {
		if b.Len() > 0 {
			b.WriteString(",")
		}
		b.WriteString(s[i : i+3])
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}
