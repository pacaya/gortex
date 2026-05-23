// Package audit scans agent configuration files (CLAUDE.md, AGENTS.md,
// Cursor/Copilot/Windsurf/Antigravity rules) for stale symbol references,
// dead file paths, and bloat — producing a structured report backed by the
// authoritative Gortex symbol graph.
package audit

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// GraphLookup is the subset of *graph.Graph needed for symbol-ref checks.
// Kept as an interface so tests can substitute a fake.
type GraphLookup interface {
	GetNode(id string) *graph.Node
	GetNodeByQualName(qualName string) *graph.Node
	FindNodesByName(name string) []*graph.Node
	GetFileNodes(filePath string) []*graph.Node
}

// StaleRef describes a backticked identifier that no longer matches any
// symbol in the graph.
type StaleRef struct {
	File  string `json:"file"`
	Line  int    `json:"line"`
	Token string `json:"token"`
}

// DeadPath describes a path-shaped token whose target does not exist on disk.
type DeadPath struct {
	File string `json:"file"`
	Line int    `json:"line"`
	Path string `json:"path"`
}

// BloatMetrics captures per-file structural bloat signals.
type BloatMetrics struct {
	Lines      int `json:"lines"`
	Bullets    int `json:"bullets"`
	LongLines  int `json:"long_lines"`
	Duplicates int `json:"duplicate_bullets"`
	CodeBlocks int `json:"code_blocks"`
	MaxDepth   int `json:"max_list_depth"`
	Score      int `json:"score"`
}

// FileReport is the per-file rollup.
type FileReport struct {
	File      string       `json:"file"`
	StaleRefs []StaleRef   `json:"stale_refs,omitempty"`
	DeadPaths []DeadPath   `json:"dead_paths,omitempty"`
	Bloat     BloatMetrics `json:"bloat"`
}

// Report is the full audit output.
type Report struct {
	Root         string       `json:"root,omitempty"`
	FilesScanned int          `json:"files_scanned"`
	StaleRefs    []StaleRef   `json:"stale_refs,omitempty"`
	DeadPaths    []DeadPath   `json:"dead_paths,omitempty"`
	BloatScore   int          `json:"bloat_score"`
	Files        []FileReport `json:"files,omitempty"`
	Suggestions  []string     `json:"suggestions,omitempty"`
}

// Audit runs all checks against the given config files (paths relative to root
// or absolute). Non-existent files are silently skipped.
func Audit(g GraphLookup, root string, files []string) *Report {
	rep := &Report{Root: root}

	for _, f := range files {
		abs := f
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(root, f)
		}
		data, err := os.ReadFile(abs)
		if err != nil {
			continue
		}
		rel, _ := filepath.Rel(root, abs)
		if rel == "" || strings.HasPrefix(rel, "..") {
			rel = f
		}

		fr := scanFile(g, root, rel, string(data))
		rep.Files = append(rep.Files, fr)
		rep.StaleRefs = append(rep.StaleRefs, fr.StaleRefs...)
		rep.DeadPaths = append(rep.DeadPaths, fr.DeadPaths...)
		rep.FilesScanned++
	}

	rep.BloatScore = aggregateBloat(rep.Files)
	rep.Suggestions = buildSuggestions(rep)

	// Deterministic ordering.
	sort.SliceStable(rep.StaleRefs, func(i, j int) bool {
		a, b := rep.StaleRefs[i], rep.StaleRefs[j]
		if a.File != b.File {
			return a.File < b.File
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		return a.Token < b.Token
	})
	sort.SliceStable(rep.DeadPaths, func(i, j int) bool {
		a, b := rep.DeadPaths[i], rep.DeadPaths[j]
		if a.File != b.File {
			return a.File < b.File
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		return a.Path < b.Path
	})

	return rep
}

// scanFile runs all three scans on a single file's contents.
func scanFile(g GraphLookup, root, rel, content string) FileReport {
	fr := FileReport{File: rel}
	lines := strings.Split(content, "\n")

	for i, line := range lines {
		for _, tok := range extractBackticked(line) {
			switch classifyToken(tok) {
			case tokenPath:
				if !pathExists(root, tok) {
					fr.DeadPaths = append(fr.DeadPaths, DeadPath{
						File: rel, Line: i + 1, Path: tok,
					})
				}
			case tokenSymbol:
				if g != nil && !symbolExists(g, tok) {
					fr.StaleRefs = append(fr.StaleRefs, StaleRef{
						File: rel, Line: i + 1, Token: tok,
					})
				}
			}
		}
	}

	fr.Bloat = scoreBloat(lines)
	return fr
}

// symbolExists returns true when the token matches any node in the graph,
// checked as (1) full ID, (2) qualified name, (3) short name of the last segment.
func symbolExists(g GraphLookup, tok string) bool {
	if n := g.GetNode(tok); n != nil {
		return true
	}
	if n := g.GetNodeByQualName(tok); n != nil {
		return true
	}
	short := tok
	// Strip trailing `()` and receiver prefixes like `Server.` / `pkg::`.
	short = strings.TrimSuffix(short, "()")
	if idx := strings.LastIndexAny(short, ".:"); idx >= 0 && idx < len(short)-1 {
		short = short[idx+1:]
	}
	if nodes := g.FindNodesByName(short); len(nodes) > 0 {
		return true
	}
	return false
}

// pathExists checks whether the token resolves to an existing filesystem path
// under root. Leading `./` is stripped; a leading `~` expands to the user's
// home directory so config docs that cite `~/.claude/CLAUDE.md` aren't
// reported as dead.
func pathExists(root, p string) bool {
	p = strings.TrimPrefix(p, "./")
	if strings.HasPrefix(p, "~") {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			p = home + strings.TrimPrefix(p, "~")
		}
	}
	if filepath.IsAbs(p) {
		_, err := os.Stat(p)
		return err == nil
	}
	_, err := os.Stat(filepath.Join(root, p))
	return err == nil
}

// buildSuggestions returns high-level remediation hints based on the report.
func buildSuggestions(rep *Report) []string {
	var out []string
	if len(rep.StaleRefs) > 0 {
		out = append(out, "Remove or rename stale symbol references — the graph no longer has them.")
	}
	if len(rep.DeadPaths) > 0 {
		out = append(out, "Update dead file paths — the referenced files don't exist.")
	}
	if rep.BloatScore >= 60 {
		out = append(out, "Config files are bloated (score >=60). Split long sections, dedupe bullets, trim >200-char lines.")
	}
	if len(out) == 0 && rep.FilesScanned > 0 {
		out = append(out, "Config looks clean.")
	}
	return out
}
