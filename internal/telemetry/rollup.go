package telemetry

import (
	"regexp"
	"time"
)

// allowedMetrics is the hard allow-list of telemetry counter keys. It is the
// privacy backbone: the aggregator physically cannot record a key that is not
// on this list, so a path, a symbol name, or any arbitrary string can never
// become a metric. Adding a counter is a deliberate edit here and nowhere else.
var allowedMetrics = map[string]bool{
	"mcp_tool_call":  true, // an MCP tool was invoked; dim = tool name
	"cli_command":    true, // a CLI subcommand ran; dim = command name
	"index":          true, // an index pass completed; dim = file-count bucket
	"daemon_session": true, // a daemon session started; dim = backend kind
}

// IsAllowedMetric reports whether key may be recorded.
func IsAllowedMetric(key string) bool { return allowedMetrics[key] }

// dimPattern bounds a metric dimension to a short, non-identifying token —
// bucket labels (`1k-10k`), tool/command names (`search_symbols`), backend
// kinds (`sqlite`). Anything with a path separator, whitespace, or length
// beyond 32 is rejected, so even a caller that passes a path or a symbol name
// as a dimension cannot leak it: the counter falls back to the bare key.
var dimPattern = regexp.MustCompile(`^[A-Za-z0-9_.<>+-]{1,32}$`)

// safeDim returns dim when it is a bounded, non-identifying token, else "".
func safeDim(dim string) string {
	if dimPattern.MatchString(dim) {
		return dim
	}
	return ""
}

// BucketFileCount collapses a file count into a coarse bucket so an exact
// count — which can narrow identification — is never recorded.
func BucketFileCount(n int) string {
	switch {
	case n < 100:
		return "<100"
	case n < 1000:
		return "100-1k"
	case n < 10000:
		return "1k-10k"
	default:
		return "10k+"
	}
}

// BucketDuration collapses an elapsed time into a coarse bucket.
func BucketDuration(d time.Duration) string {
	ms := d.Milliseconds()
	switch {
	case ms < 10_000:
		return "<10s"
	case ms < 60_000:
		return "10-60s"
	case ms < 300_000:
		return "1-5m"
	default:
		return "5m+"
	}
}

// Rollup is one UTC day's aggregated, coarse counts. Counts maps an
// allow-listed metric key (optionally suffixed with a bucketed dimension after
// a colon) to the number of times it occurred that day.
type Rollup struct {
	Day    string         `json:"day"` // YYYY-MM-DD in UTC
	Counts map[string]int `json:"counts"`
}

// DayKey renders the UTC calendar day of t.
func DayKey(t time.Time) string { return t.UTC().Format("2006-01-02") }

// NewRollup creates an empty rollup for the UTC day of t.
func NewRollup(t time.Time) *Rollup {
	return &Rollup{Day: DayKey(t), Counts: map[string]int{}}
}

// Add increments the counter for an allow-listed metric, optionally qualified
// by a dimension (a bucket label or a fixed enum like a tool name). It returns
// whether the event counted: a key not on the allow-list is silently dropped,
// and a dimension that is not a bounded token is discarded (the bare key still
// counts). This is the single mutation point, so the allow-list and dimension
// guard are unavoidable.
func (r *Rollup) Add(key, dim string) bool {
	if !allowedMetrics[key] {
		return false
	}
	if r.Counts == nil {
		r.Counts = map[string]int{}
	}
	name := key
	if d := safeDim(dim); d != "" {
		name = key + ":" + d
	}
	r.Counts[name]++
	return true
}

// Merge folds other's counts into r. Days are assumed equal (the caller groups
// by day); a mismatched day is a programmer error and is ignored rather than
// silently corrupting r's day label.
func (r *Rollup) Merge(other *Rollup) {
	if other == nil || other.Day != r.Day {
		return
	}
	if r.Counts == nil {
		r.Counts = map[string]int{}
	}
	for k, v := range other.Counts {
		r.Counts[k] += v
	}
}

// Total is the sum of every counter — a convenience for "did anything happen
// today" checks before persisting or sending.
func (r *Rollup) Total() int {
	n := 0
	for _, v := range r.Counts {
		n += v
	}
	return n
}
