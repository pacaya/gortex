// Package savings persists cumulative token-savings metrics across server
// restarts. Every source-reading tool call feeds this store through the MCP
// server's tokenStats, so over time the numbers become a credible narrative:
// "Gortex saved N tokens / $X at model rate this month".
//
// Storage format: a single JSON file at ~/.cache/gortex/savings.json (or the
// configured cache dir). Atomic writes via temp-file + rename, with an
// advisory file lock on a sidecar `.lock` file so multiple gortex processes
// (e.g. a daemon and a parallel `gortex mcp`) can write to the same
// savings file without clobbering each other's deltas. Falls back to an
// in-memory-only store when the path isn't writable.
package savings

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/gofrs/flock"
)

const (
	// schemaVersion lets future changes migrate or reject incompatible files.
	schemaVersion = 1
	// flushEvery buffers this many observations before writing to disk.
	flushEvery = 20
)

// Totals is the cumulative record for a single scope (top-level or per-repo).
type Totals struct {
	TokensSaved    int64 `json:"tokens_saved"`
	TokensReturned int64 `json:"tokens_returned"`
	CallsCounted   int64 `json:"calls_counted"`
}

// File is the on-disk schema. Older files without `per_language` load
// cleanly — JSON unmarshal leaves the missing field as a nil map and
// the next write upgrades it.
type File struct {
	Version     int                `json:"version"`
	FirstSeen   time.Time          `json:"first_seen"`
	LastUpdated time.Time          `json:"last_updated"`
	Totals      Totals             `json:"totals"`
	PerRepo     map[string]*Totals `json:"per_repo,omitempty"`
	PerLanguage map[string]*Totals `json:"per_language,omitempty"`
}

// Store holds the cumulative savings state and flushes to disk periodically.
// All operations are safe for concurrent use. When path is empty the store
// still tracks in-memory but never writes to disk.
//
// Concurrency model: in-process callers serialize through s.mu. Cross-process
// safety is achieved on flush by acquiring an advisory flock on a sidecar
// lock file, re-reading the on-disk totals, merging this process's pending
// delta, and writing back. A second process that flushed in between just
// shifts our baseline up; nothing is lost.
type Store struct {
	mu        sync.Mutex
	path      string
	file      File
	delta     Totals             // cumulative this-process contributions since last successful flush
	perDelta  map[string]*Totals // per-repo deltas, same semantics as delta
	langDelta map[string]*Totals // per-language deltas
	pending   int                // observations since last flush

	// eventsPath is the JSONL event log derived from `path`. Each
	// AddObservation appends one line so the `gortex savings` dashboard
	// can compute Today / Last 7 days windows the cumulative file can't.
	// Empty when the store is in-memory only or when disabled.
	eventsPath     string
	eventsDisabled bool

	// stop signals the optional periodic flusher to exit. Nil when no
	// flusher is running.
	stopOnce sync.Once
	stopCh   chan struct{}
}

// DefaultPath returns the canonical savings.json location under the user's
// cache dir. Returns an empty string (i.e. "disable persistence") when the
// cache dir is unavailable.
func DefaultPath() string {
	base, err := os.UserCacheDir()
	if err != nil || base == "" {
		return ""
	}
	return filepath.Join(base, "gortex", "savings.json")
}

// DefaultEventsPath returns the canonical savings.jsonl event-log path next
// to DefaultPath. Empty when the cache dir is unavailable.
func DefaultEventsPath() string {
	p := DefaultPath()
	if p == "" {
		return ""
	}
	return EventsPathFor(p)
}

// EventsPathFor returns the JSONL event-log path that corresponds to a
// cumulative savings JSON path — `<dir>/savings.jsonl` alongside the JSON
// file. Empty when storePath is empty.
func EventsPathFor(storePath string) string {
	if storePath == "" {
		return ""
	}
	dir := filepath.Dir(storePath)
	base := filepath.Base(storePath)
	ext := filepath.Ext(base)
	stem := base
	if ext != "" {
		stem = base[:len(base)-len(ext)]
	}
	return filepath.Join(dir, stem+".jsonl")
}

// Open loads savings from path, or returns an empty Store when the file
// doesn't exist yet. Corrupt or incompatible files are backed up to
// `<path>.corrupt-<ts>` and replaced with a fresh state so a bad write can't
// permanently break metrics.
func Open(path string) (*Store, error) {
	s := &Store{
		path:       path,
		eventsPath: EventsPathFor(path),
		perDelta:   make(map[string]*Totals),
		langDelta:  make(map[string]*Totals),
	}
	s.file.Version = schemaVersion
	s.file.FirstSeen = time.Now().UTC()
	s.file.PerRepo = make(map[string]*Totals)
	s.file.PerLanguage = make(map[string]*Totals)

	if path == "" {
		return s, nil
	}

	loaded, err := readFile(path)
	if err != nil {
		return s, err
	}
	if loaded != nil {
		s.file = *loaded
		// Older files have no per_language section; fill in so callers
		// don't need a nil check.
		if s.file.PerLanguage == nil {
			s.file.PerLanguage = make(map[string]*Totals)
		}
	}
	return s, nil
}

// readFile loads the savings file at path. Returns (nil, nil) when the file
// doesn't exist or is corrupt (in which case it's renamed to a .corrupt-N
// sidecar so future opens get a clean slate).
func readFile(path string) (*File, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read savings: %w", err)
	}

	var loaded File
	if jerr := json.Unmarshal(data, &loaded); jerr != nil || loaded.Version != schemaVersion {
		backup := fmt.Sprintf("%s.corrupt-%d", path, time.Now().Unix())
		_ = os.Rename(path, backup)
		return nil, nil
	}
	if loaded.PerRepo == nil {
		loaded.PerRepo = make(map[string]*Totals)
	}
	if loaded.PerLanguage == nil {
		loaded.PerLanguage = make(map[string]*Totals)
	}
	return &loaded, nil
}

// AddObservation increments the store by one source-reading tool call.
// repoPath and language, when non-empty, also aggregate under per-repo
// and per-language buckets respectively. tool, when non-empty, is recorded
// in the JSONL event log so the dashboard can build a per-tool breakdown
// in --verbose mode. Writes to disk every flushEvery observations.
func (s *Store) AddObservation(repoPath, language, tool string, returned, saved int64) {
	if s == nil {
		return
	}
	now := time.Now().UTC()

	s.mu.Lock()

	if saved < 0 {
		saved = 0
	}

	s.file.Totals.TokensSaved += saved
	s.file.Totals.TokensReturned += returned
	s.file.Totals.CallsCounted++
	s.file.LastUpdated = now

	s.delta.TokensSaved += saved
	s.delta.TokensReturned += returned
	s.delta.CallsCounted++

	addBucket := func(bucket map[string]*Totals, deltaBucket map[string]*Totals, key string) {
		if key == "" {
			return
		}
		t := bucket[key]
		if t == nil {
			t = &Totals{}
			bucket[key] = t
		}
		t.TokensSaved += saved
		t.TokensReturned += returned
		t.CallsCounted++

		dt := deltaBucket[key]
		if dt == nil {
			dt = &Totals{}
			deltaBucket[key] = dt
		}
		dt.TokensSaved += saved
		dt.TokensReturned += returned
		dt.CallsCounted++
	}
	addBucket(s.file.PerRepo, s.perDelta, repoPath)
	addBucket(s.file.PerLanguage, s.langDelta, language)

	s.pending++
	flushCumulative := s.pending >= flushEvery
	eventsPath := s.eventsPath
	eventsDisabled := s.eventsDisabled
	s.mu.Unlock()

	// Best-effort JSONL append outside the store mutex — the file
	// handle is opened with O_APPEND so concurrent writers from this
	// or other gortex processes interleave at line boundaries.
	if !eventsDisabled && eventsPath != "" {
		_ = appendEvent(eventsPath, Event{
			TS:       now,
			Repo:     repoPath,
			Language: language,
			Tool:     tool,
			Returned: returned,
			Saved:    saved,
		})
	}

	if flushCumulative {
		s.mu.Lock()
		_ = s.flushLocked()
		s.mu.Unlock()
	}
}

// Snapshot returns a deep copy of the current totals (safe for reads outside
// the mutex). Used by graph_stats and the CLI.
func (s *Store) Snapshot() File {
	if s == nil {
		return File{Version: schemaVersion, PerRepo: map[string]*Totals{}}
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	cp := s.file
	cp.PerRepo = make(map[string]*Totals, len(s.file.PerRepo))
	for k, v := range s.file.PerRepo {
		t := *v
		cp.PerRepo[k] = &t
	}
	return cp
}

// Flush writes pending observations to disk. Safe to call when no path is
// configured (no-op).
func (s *Store) Flush() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.flushLocked()
}

// Pending reports whether the store has unflushed observations. Lets a
// background ticker skip the flock+IO when nothing has happened.
func (s *Store) Pending() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pending > 0
}

// StartPeriodicFlush kicks off a goroutine that flushes the store every
// interval if there are pending observations. Returns a stop function the
// caller should invoke at shutdown to terminate the ticker. Calling
// StartPeriodicFlush more than once on the same Store is a no-op for the
// extra calls (returns a no-op stopper).
//
// The point of the periodic flusher is to bound on-crash data loss to
// roughly `interval` worth of observations even when the call rate is too
// low to trip the every-N-observations flush.
func (s *Store) StartPeriodicFlush(interval time.Duration) func() {
	if s == nil || interval <= 0 {
		return func() {}
	}

	s.mu.Lock()
	if s.stopCh != nil {
		s.mu.Unlock()
		return func() {}
	}
	stop := make(chan struct{})
	s.stopCh = stop
	s.mu.Unlock()

	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				if s.Pending() {
					_ = s.Flush()
				}
			}
		}
	}()

	return func() {
		s.stopOnce.Do(func() { close(stop) })
	}
}

// Reset wipes all cumulative data and removes the persisted file. Used by
// `gortex savings --reset`.
func (s *Store) Reset() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	s.file = File{
		Version:     schemaVersion,
		FirstSeen:   time.Now().UTC(),
		PerRepo:     make(map[string]*Totals),
		PerLanguage: make(map[string]*Totals),
	}
	s.delta = Totals{}
	s.perDelta = make(map[string]*Totals)
	s.langDelta = make(map[string]*Totals)
	s.pending = 0

	if s.path == "" {
		return nil
	}
	if err := os.Remove(s.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if s.eventsPath != "" {
		if err := os.Remove(s.eventsPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// flushLocked must be called with s.mu held.
//
// The flush is cross-process safe: an advisory flock on `<path>.lock`
// serializes with other gortex processes writing the same file. Inside the
// critical section we re-read the on-disk file, add this process's pending
// deltas to it, and write back. That way two parallel writers each get
// their contributions persisted instead of last-flusher-wins.
func (s *Store) flushLocked() error {
	if s.path == "" {
		s.pending = 0
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}

	lockPath := s.path + ".lock"
	fl := flock.New(lockPath)
	if err := fl.Lock(); err != nil {
		return fmt.Errorf("acquire savings lock: %w", err)
	}
	defer func() { _ = fl.Unlock() }()

	// Re-read whatever's on disk now — another process may have flushed
	// since we last loaded. Merge our deltas onto that baseline.
	merged, err := readFile(s.path)
	if err != nil {
		return err
	}
	if merged == nil {
		// File missing (or was just backed up as corrupt). Start fresh
		// from our in-memory baseline — s.file already includes both
		// any value loaded at Open time and everything observed in this
		// process, so don't re-add the delta on top. Deep-copy maps so
		// the merged copy doesn't alias s.file's maps.
		fresh := s.file
		fresh.PerRepo = copyTotalsMap(s.file.PerRepo)
		fresh.PerLanguage = copyTotalsMap(s.file.PerLanguage)
		merged = &fresh
	} else {
		mergeTotals(&merged.Totals, &s.delta)
		if merged.PerRepo == nil {
			merged.PerRepo = make(map[string]*Totals)
		}
		if merged.PerLanguage == nil {
			merged.PerLanguage = make(map[string]*Totals)
		}
		mergeBucketDeltas(merged.PerRepo, s.perDelta)
		mergeBucketDeltas(merged.PerLanguage, s.langDelta)
		if merged.FirstSeen.IsZero() || s.file.FirstSeen.Before(merged.FirstSeen) {
			merged.FirstSeen = s.file.FirstSeen
		}
		merged.LastUpdated = time.Now().UTC()
		merged.Version = schemaVersion
	}

	// Atomic write: temp file in the same dir, then rename.
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".savings-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()

	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	if err := enc.Encode(merged); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		_ = os.Remove(tmpName)
		return err
	}

	// Adopt the merged view as our authoritative in-memory state and
	// clear the delta — anything new arriving after this point will be
	// what we commit on the next flush.
	s.file = *merged
	s.delta = Totals{}
	s.perDelta = make(map[string]*Totals)
	s.langDelta = make(map[string]*Totals)
	s.pending = 0
	return nil
}

// copyTotalsMap returns a deep copy of a name → Totals map.
func copyTotalsMap(src map[string]*Totals) map[string]*Totals {
	if src == nil {
		return make(map[string]*Totals)
	}
	dst := make(map[string]*Totals, len(src))
	for k, v := range src {
		cp := *v
		dst[k] = &cp
	}
	return dst
}

// mergeBucketDeltas folds the per-process delta map into the merged map
// (which represents the on-disk baseline + this process's contributions).
func mergeBucketDeltas(merged, deltas map[string]*Totals) {
	for k, dt := range deltas {
		t := merged[k]
		if t == nil {
			t = &Totals{}
			merged[k] = t
		}
		mergeTotals(t, dt)
	}
}

func mergeTotals(dst, src *Totals) {
	dst.TokensSaved += src.TokensSaved
	dst.TokensReturned += src.TokensReturned
	dst.CallsCounted += src.CallsCounted
}
