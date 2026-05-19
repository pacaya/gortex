package mcp

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// notebookEntry is a single repository-local persistent notebook
// record. Stored as a markdown file under .gortex/notebook/<id>.md
// with a YAML frontmatter header for metadata + a markdown body.
//
// The notebook is the third memory axis below session notes
// (per-session, expires) and cross-session memories (workspace-wide,
// in cache dir): notebook entries live in the repo working tree so
// they can be committed to git and reviewed in PRs.
type notebookEntry struct {
	ID         string
	Title      string
	Tags       []string
	Created    time.Time
	Updated    time.Time
	LastUsed   time.Time
	UsedCount  uint64
	Body       string
}

// notebookManager owns the on-disk notebook store. The directory is
// the repo's .gortex/notebook/ tree; an empty dir yields a no-op
// manager so test fixtures and single-shot CLI calls don't fail.
type notebookManager struct {
	mu  sync.Mutex
	dir string
	// ttl applies to LastUsed when set: entries unused for longer
	// than ttl are pruned at save time. 0 disables pruning.
	ttl time.Duration
}

// newNotebookManager returns a manager rooted at <repoPath>/.gortex/
// notebook/. Empty repoPath yields a no-disk manager (the methods
// are still safe to call, they just no-op the persistence).
func newNotebookManager(repoPath string) *notebookManager {
	if repoPath == "" {
		return &notebookManager{}
	}
	return &notebookManager{
		dir: filepath.Join(repoPath, ".gortex", "notebook"),
		ttl: 30 * 24 * time.Hour,
	}
}

// Save persists a notebook entry. Generates an ID when missing.
// Returns the entry as it landed on disk (id + timestamps set).
func (nm *notebookManager) Save(entry notebookEntry) (notebookEntry, error) {
	nm.mu.Lock()
	defer nm.mu.Unlock()

	if entry.ID == "" {
		entry.ID = newNotebookID()
	}
	now := time.Now().UTC()
	if entry.Created.IsZero() {
		entry.Created = now
	}
	entry.Updated = now

	if nm.dir == "" {
		return entry, nil
	}
	if err := os.MkdirAll(nm.dir, 0o755); err != nil {
		return entry, fmt.Errorf("mkdir notebook: %w", err)
	}
	if err := os.WriteFile(nm.entryPath(entry.ID), []byte(notebookMarshal(entry)), 0o644); err != nil {
		return entry, fmt.Errorf("write notebook: %w", err)
	}
	// Best-effort TTL prune. Failures don't fail the save — the
	// next call will retry.
	nm.pruneLocked()
	return entry, nil
}

// Get loads a single entry by id. Returns (entry, true) on hit,
// (zero, false) when the file is missing.
func (nm *notebookManager) Get(id string) (notebookEntry, bool) {
	nm.mu.Lock()
	defer nm.mu.Unlock()
	if nm.dir == "" {
		return notebookEntry{}, false
	}
	body, err := os.ReadFile(nm.entryPath(id))
	if err != nil {
		return notebookEntry{}, false
	}
	entry, err := notebookUnmarshal(string(body))
	if err != nil {
		return notebookEntry{}, false
	}
	entry.ID = id
	return entry, true
}

// Delete removes an entry from disk. Missing files are not errors —
// callers can use Delete unconditionally.
func (nm *notebookManager) Delete(id string) error {
	nm.mu.Lock()
	defer nm.mu.Unlock()
	if nm.dir == "" {
		return nil
	}
	err := os.Remove(nm.entryPath(id))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// List returns every entry on disk sorted by Updated DESC. Cheap
// enough for typical notebook sizes (hundreds of entries); the cap
// at the call site keeps responses bounded.
func (nm *notebookManager) List() []notebookEntry {
	nm.mu.Lock()
	defer nm.mu.Unlock()
	return nm.listLocked()
}

func (nm *notebookManager) listLocked() []notebookEntry {
	if nm.dir == "" {
		return nil
	}
	entries, err := os.ReadDir(nm.dir)
	if err != nil {
		return nil
	}
	out := make([]notebookEntry, 0, len(entries))
	for _, de := range entries {
		if de.IsDir() || !strings.HasSuffix(de.Name(), ".md") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(nm.dir, de.Name()))
		if err != nil {
			continue
		}
		e, err := notebookUnmarshal(string(body))
		if err != nil {
			continue
		}
		e.ID = strings.TrimSuffix(de.Name(), ".md")
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Updated.After(out[j].Updated)
	})
	return out
}

// Find runs a case-insensitive substring scan over Title / Body /
// Tags. Returns matches sorted by Updated DESC. Empty query returns
// every entry (same as List).
func (nm *notebookManager) Find(query string) []notebookEntry {
	nm.mu.Lock()
	defer nm.mu.Unlock()
	all := nm.listLocked()
	if strings.TrimSpace(query) == "" {
		return all
	}
	q := strings.ToLower(query)
	out := make([]notebookEntry, 0, len(all))
	for _, e := range all {
		if strings.Contains(strings.ToLower(e.Title), q) ||
			strings.Contains(strings.ToLower(e.Body), q) {
			out = append(out, e)
			continue
		}
		for _, t := range e.Tags {
			if strings.Contains(strings.ToLower(t), q) {
				out = append(out, e)
				break
			}
		}
	}
	return out
}

// MarkUsed bumps UsedCount + LastUsed for the named entry and
// persists the change. The semantic is "the agent just consulted
// this entry"; that signal also resets the TTL pruner's clock so
// load-bearing entries don't age out.
func (nm *notebookManager) MarkUsed(id string) (notebookEntry, error) {
	nm.mu.Lock()
	defer nm.mu.Unlock()
	if nm.dir == "" {
		return notebookEntry{}, fmt.Errorf("notebook is not initialised")
	}
	body, err := os.ReadFile(nm.entryPath(id))
	if err != nil {
		return notebookEntry{}, err
	}
	entry, err := notebookUnmarshal(string(body))
	if err != nil {
		return notebookEntry{}, err
	}
	entry.ID = id
	entry.UsedCount++
	entry.LastUsed = time.Now().UTC()
	if err := os.WriteFile(nm.entryPath(id), []byte(notebookMarshal(entry)), 0o644); err != nil {
		return notebookEntry{}, err
	}
	return entry, nil
}

// pruneLocked removes entries whose LastUsed (or Updated, when never
// used) is older than the TTL. Best-effort — silent on individual
// errors so a permission glitch on one file doesn't poison the
// rest of the call.
func (nm *notebookManager) pruneLocked() {
	if nm.dir == "" || nm.ttl <= 0 {
		return
	}
	cutoff := time.Now().UTC().Add(-nm.ttl)
	for _, e := range nm.listLocked() {
		ref := e.LastUsed
		if ref.IsZero() {
			ref = e.Updated
		}
		if ref.Before(cutoff) {
			_ = os.Remove(nm.entryPath(e.ID))
		}
	}
}

func (nm *notebookManager) entryPath(id string) string {
	return filepath.Join(nm.dir, id+".md")
}

// newNotebookID returns a short random hex string suitable for a
// file basename. 16 chars = 8 bytes = ample collision resistance
// for a per-repo notebook.
func newNotebookID() string {
	var buf [8]byte
	_, _ = rand.Read(buf[:])
	return "nb" + hex.EncodeToString(buf[:])
}

// notebookFrontmatterRe matches the YAML-ish frontmatter block at
// the top of a notebook file. The block opens with --- on its own
// line and closes with --- on its own line; everything after is the
// body.
var notebookFrontmatterRe = regexp.MustCompile(`(?s)^---\n(.*?)\n---\n?`)

// notebookMarshal renders an entry as a markdown file: YAML
// frontmatter for metadata + the verbatim body below. Field order
// is stable so re-saves produce byte-identical output when nothing
// material changed.
func notebookMarshal(e notebookEntry) string {
	var b strings.Builder
	b.WriteString("---\n")
	if e.Title != "" {
		fmt.Fprintf(&b, "title: %s\n", yamlEscapeOneLine(e.Title))
	}
	if len(e.Tags) > 0 {
		fmt.Fprintf(&b, "tags: [%s]\n", strings.Join(e.Tags, ", "))
	}
	if !e.Created.IsZero() {
		fmt.Fprintf(&b, "created: %s\n", e.Created.UTC().Format(time.RFC3339Nano))
	}
	if !e.Updated.IsZero() {
		fmt.Fprintf(&b, "updated: %s\n", e.Updated.UTC().Format(time.RFC3339Nano))
	}
	if !e.LastUsed.IsZero() {
		fmt.Fprintf(&b, "last_used: %s\n", e.LastUsed.UTC().Format(time.RFC3339Nano))
	}
	if e.UsedCount > 0 {
		fmt.Fprintf(&b, "used_count: %d\n", e.UsedCount)
	}
	b.WriteString("---\n\n")
	b.WriteString(e.Body)
	if !strings.HasSuffix(e.Body, "\n") {
		b.WriteString("\n")
	}
	return b.String()
}

// notebookUnmarshal parses the frontmatter + body shape written by
// notebookMarshal. Unknown fields are ignored; malformed dates are
// silently skipped (zero-valued in the result).
func notebookUnmarshal(s string) (notebookEntry, error) {
	m := notebookFrontmatterRe.FindStringSubmatchIndex(s)
	if m == nil {
		// No frontmatter — treat entire content as Body.
		return notebookEntry{Body: s}, nil
	}
	header := s[m[2]:m[3]]
	body := s[m[1]:]
	body = strings.TrimLeft(body, "\n")

	entry := notebookEntry{Body: body}
	for _, line := range strings.Split(header, "\n") {
		line = strings.TrimRight(line, "\r")
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		switch strings.TrimSpace(k) {
		case "title":
			entry.Title = yamlUnescapeOneLine(v)
		case "tags":
			entry.Tags = parseYAMLInlineList(v)
		case "created":
			if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
				entry.Created = t
			}
		case "updated":
			if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
				entry.Updated = t
			}
		case "last_used":
			if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
				entry.LastUsed = t
			}
		case "used_count":
			var n uint64
			fmt.Sscanf(v, "%d", &n)
			entry.UsedCount = n
		}
	}
	return entry, nil
}

// yamlEscapeOneLine quotes a value when it contains characters that
// would break the simple `key: value` shape on read.
func yamlEscapeOneLine(s string) string {
	if strings.ContainsAny(s, ":#\"\n") {
		return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
	}
	return s
}

func yamlUnescapeOneLine(s string) string {
	if strings.HasPrefix(s, `"`) && strings.HasSuffix(s, `"`) && len(s) >= 2 {
		s = s[1 : len(s)-1]
		return strings.ReplaceAll(s, `\"`, `"`)
	}
	return s
}

// parseYAMLInlineList parses the `[a, b, c]` form we emit for tags.
// Tolerates surrounding spaces, missing brackets, and empty strings.
func parseYAMLInlineList(s string) []string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "[")
	s = strings.TrimSuffix(s, "]")
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		t := strings.TrimSpace(p)
		if t != "" {
			out = append(out, t)
		}
	}
	return out
}
