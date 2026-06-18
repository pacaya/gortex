package telemetry

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Store persists daily rollups as one JSON file per UTC day under a telemetry
// directory (rollup-YYYY-MM-DD.json). Per-day files let a completed day be
// read, sent, and deleted independently of the day still accumulating.
type Store struct {
	dir string
}

// NewStore roots a store at dir; the directory is created on first write.
func NewStore(dir string) *Store { return &Store{dir: dir} }

const (
	rollupFilePrefix = "rollup-"
	rollupFileSuffix = ".json"
)

func (s *Store) pathForDay(day string) string {
	return filepath.Join(s.dir, rollupFilePrefix+day+rollupFileSuffix)
}

// Load reads a day's rollup, returning an empty rollup (not an error) when the
// day has no file yet — the natural starting point for accumulation.
func (s *Store) Load(day string) (*Rollup, error) {
	b, err := os.ReadFile(s.pathForDay(day))
	if errors.Is(err, fs.ErrNotExist) {
		return &Rollup{Day: day, Counts: map[string]int{}}, nil
	}
	if err != nil {
		return nil, err
	}
	r := &Rollup{}
	if err := json.Unmarshal(b, r); err != nil {
		return nil, err
	}
	if r.Counts == nil {
		r.Counts = map[string]int{}
	}
	return r, nil
}

// Save atomically writes a rollup's day file (temp file + rename), creating the
// telemetry directory on demand. Atomicity guarantees a reader (or a crash)
// never sees a half-written rollup.
func (s *Store) Save(r *Rollup) error {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.dir, rollupFilePrefix+r.Day+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op after a successful rename
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, s.pathForDay(r.Day))
}

// Merge loads a day, folds r into it, and saves — the recorder's accumulate
// step, so concurrent processes converge on the same day's totals.
func (s *Store) Merge(r *Rollup) error {
	existing, err := s.Load(r.Day)
	if err != nil {
		return err
	}
	existing.Merge(r)
	return s.Save(existing)
}

// Days returns every persisted day, sorted ascending.
func (s *Store) Days() ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var days []string
	for _, e := range entries {
		n := e.Name()
		if strings.HasPrefix(n, rollupFilePrefix) && strings.HasSuffix(n, rollupFileSuffix) {
			days = append(days, strings.TrimSuffix(strings.TrimPrefix(n, rollupFilePrefix), rollupFileSuffix))
		}
	}
	sort.Strings(days)
	return days, nil
}

// LoadCompleted returns every rollup whose day is strictly before today (UTC).
// Today is still accumulating, so only completed days are safe to send — this
// is how volume scales with active machines, not with tool calls.
func (s *Store) LoadCompleted(today string) ([]*Rollup, error) {
	days, err := s.Days()
	if err != nil {
		return nil, err
	}
	var out []*Rollup
	for _, d := range days {
		if d >= today {
			continue // today or a future-dated file: still open
		}
		r, err := s.Load(d)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// Delete removes a day's file (after a successful send). A missing file is not
// an error — delete is idempotent.
func (s *Store) Delete(day string) error {
	err := os.Remove(s.pathForDay(day))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}
