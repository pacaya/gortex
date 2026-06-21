package progress

import (
	"fmt"
	"io"
	"sync"
	"time"
)

// TimingReporter records the first-seen timestamp of each stage and,
// when printed, emits a per-stage duration breakdown. Shows where
// wall-clock time is being spent during a full index pass.
//
// Stage transitions are detected by the reporter seeing a *new* stage
// label arrive — subsequent ticks for the same stage (progress updates
// like "parsing 3000/5000") don't create a new entry. This matches the
// indexer's usage where it calls Report once at stage entry and then
// again with counter updates.
type TimingReporter struct {
	mu     sync.Mutex
	start  time.Time
	stages []stageEntry
	seen   map[string]int // stage → index in stages (also suppresses duplicates)
}

type stageEntry struct {
	name  string
	seen  time.Time
	ticks int
}

// NewTimingReporter returns a reporter with its clock anchored at now.
func NewTimingReporter() *TimingReporter {
	return &TimingReporter{
		start: time.Now(),
		seen:  make(map[string]int),
	}
}

// Report records a stage tick. The first tick for a given stage name
// is treated as the stage's start timestamp.
func (r *TimingReporter) Report(stage string, _, _ int) {
	if stage == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if idx, ok := r.seen[stage]; ok {
		r.stages[idx].ticks++
		return
	}
	r.seen[stage] = len(r.stages)
	r.stages = append(r.stages, stageEntry{name: stage, seen: time.Now(), ticks: 1})
}

// WriteReport prints a two-column breakdown: per-stage duration and
// cumulative time since the reporter was created. end defaults to
// time.Now() when zero; pass an explicit value when the caller
// finishes before a final "indexing complete" stage tick is emitted.
func (r *TimingReporter) WriteReport(w io.Writer, end time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if end.IsZero() {
		end = time.Now()
	}
	if len(r.stages) == 0 {
		fmt.Fprintln(w, "no stages recorded")
		return
	}

	fmt.Fprintf(w, "%-32s %12s %12s\n", "stage", "duration", "cumulative")
	fmt.Fprintf(w, "%-32s %12s %12s\n",
		"--------------------------------", "------------", "------------")
	for i, s := range r.stages {
		var nextStart time.Time
		if i+1 < len(r.stages) {
			nextStart = r.stages[i+1].seen
		} else {
			nextStart = end
		}
		duration := nextStart.Sub(s.seen)
		cumulative := nextStart.Sub(r.start)
		fmt.Fprintf(w, "%-32s %12s %12s\n",
			s.name, formatMs(duration), formatMs(cumulative))
	}
}

// formatMs formats a duration as "123ms" / "4.56s" / "1m23s" depending
// on magnitude. Small-enough to be readable in a CLI dump.
func formatMs(d time.Duration) string {
	switch {
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	case d < time.Minute:
		return fmt.Sprintf("%.2fs", d.Seconds())
	default:
		m := int(d / time.Minute)
		s := int((d % time.Minute) / time.Second)
		return fmt.Sprintf("%dm%02ds", m, s)
	}
}
