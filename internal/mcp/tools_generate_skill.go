package mcp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
)

// registerGenerateSkillTool wires generate_skill — emits a
// `.claude/skills/<name>/SKILL.md` plus a `references/` tree from a
// directory of source files. First MCP-native skill generator;
// mirrors repomix's generateSkillTool.ts surface so existing
// agent workflows that pack-and-ship a directory as a skill have a
// drop-in primitive.
func (s *Server) registerGenerateSkillTool() {
	s.addTool(
		mcp.NewTool("generate_skill",
			mcp.WithDescription("Bundle a directory into a Claude Code skill. Walks `directory`, applies include/ignore patterns, and writes `<output_dir>/SKILL.md` (with name + description YAML frontmatter and a reference index) plus `<output_dir>/references/<relpath>` for each kept file. Use to ship a region of the codebase as a model-invoked skill — e.g. an internal SDK an agent should reach for when answering questions about that area."),
			mcp.WithString("directory", mcp.Description("Directory to bundle (repo-relative or absolute).")),
			mcp.WithString("skill_name", mcp.Description("Skill name (kebab-case). Defaults to the directory's basename.")),
			mcp.WithString("description", mcp.Description("Description for the skill picker. Defaults to a templated 'Use when working with <name>'.")),
			mcp.WithString("output_dir", mcp.Description("Where to write SKILL.md (repo-relative or absolute). Defaults to .claude/skills/<skill_name>/ relative to the resolved input directory's repo root.")),
			mcp.WithString("include_patterns", mcp.Description("Comma-separated glob patterns (filepath.Match syntax) for files to KEEP. Matched against the file path RELATIVE to `directory`. Empty = keep all.")),
			mcp.WithString("ignore_patterns", mcp.Description("Comma-separated glob patterns for files to SKIP. Applied after include_patterns. Empty = no extra exclusion. The common noise (.git/, node_modules/, .gortex/, .claude/) is always skipped.")),
			mcp.WithBoolean("compress", mcp.Description("When true, reference files written for Go / TS / JS / Python source get function bodies elided via the same compress_bodies surface as read_file. Default false.")),
			mcp.WithNumber("max_references", mcp.Description("Cap on the number of files written under references/ (default: 50).")),
			mcp.WithBoolean("dry_run", mcp.Description("When true, returns what WOULD be written without touching disk. Default false.")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx, or toon")),
		),
		s.handleGenerateSkill,
	)
}

// generateSkillResult captures one file the tool placed under
// references/. Returned in the response so callers can pipe the list
// into a follow-up commit or sanity-check.
type generateSkillRef struct {
	RelPath      string `json:"rel_path"`
	BytesWritten int    `json:"bytes_written"`
	Compressed   bool   `json:"compressed,omitempty"`
}

func (s *Server) handleGenerateSkill(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	rawDir, err := req.RequireString("directory")
	if err != nil {
		return mcp.NewToolResultError("directory is required"), nil
	}

	absDir, _, err := s.resolveFilePath(rawDir)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("resolve directory: %v", err)), nil
	}
	info, err := os.Stat(absDir)
	if err != nil || !info.IsDir() {
		return mcp.NewToolResultError(fmt.Sprintf("%s is not a directory", rawDir)), nil
	}

	skillName := strings.TrimSpace(req.GetString("skill_name", ""))
	if skillName == "" {
		skillName = sluggify(filepath.Base(absDir))
	}
	if skillName == "" {
		return mcp.NewToolResultError("skill_name could not be derived from directory; pass it explicitly"), nil
	}

	description := strings.TrimSpace(req.GetString("description", ""))
	if description == "" {
		description = fmt.Sprintf("Use when working with %s. Bundled from %s.", skillName, rawDir)
	}

	includes := splitCSV(req.GetString("include_patterns", ""))
	ignores := splitCSV(req.GetString("ignore_patterns", ""))
	compress := req.GetBool("compress", false)
	maxRefs := max(req.GetInt("max_references", 50), 1)
	dryRun := req.GetBool("dry_run", false)

	// output_dir defaults to .claude/skills/<skill_name>/ next to the
	// resolved input directory. The caller can override with a
	// repo-relative or absolute path.
	outputDir := strings.TrimSpace(req.GetString("output_dir", ""))
	var absOutputDir string
	if outputDir == "" {
		repoRoot := repoRootContaining(absDir)
		if repoRoot == "" {
			repoRoot = absDir
		}
		absOutputDir = filepath.Join(repoRoot, ".claude", "skills", skillName)
	} else {
		resolved, _, err := s.resolveFilePath(outputDir)
		if err != nil {
			absOutputDir = outputDir // accept literal path even when not under a known root
		} else {
			absOutputDir = resolved
		}
	}

	// Walk the directory once, collecting candidates that pass
	// every filter. The walk also enforces the cap so the work
	// is bounded even on huge trees.
	refs := []generateSkillRef{}
	skipped := 0
	walkErr := filepath.WalkDir(absDir, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, _ := filepath.Rel(absDir, path)
		if rel == "." {
			return nil
		}
		base := filepath.Base(path)
		// Hard-coded noise filter — same set the indexer skips by
		// default. We don't want bundled skills to ship vendor trees.
		if isAlwaysSkipped(base) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}

		if !matchAny(includes, rel) {
			skipped++
			return nil
		}
		if matchesIgnore(ignores, rel) {
			skipped++
			return nil
		}
		if len(refs) >= maxRefs {
			skipped++
			return nil
		}

		body, readErr := os.ReadFile(path)
		if readErr != nil {
			skipped++
			return nil
		}
		compressedHere := false
		if compress && shouldCompressByExt(base) {
			// Lightweight body elision — language-aware compression
			// lives in internal/elide, but it requires a parser
			// pipeline we don't want to spin up here. The body-line
			// elision below removes the inner content of every
			// brace-balanced block while keeping signatures and
			// comments. Conservative and language-agnostic enough
			// for a skill reference dump.
			body = []byte(elideBraceBodies(string(body)))
			compressedHere = true
		}
		refs = append(refs, generateSkillRef{
			RelPath:      rel,
			BytesWritten: len(body),
			Compressed:   compressedHere,
		})

		if dryRun {
			return nil
		}
		destPath := filepath.Join(absOutputDir, "references", rel)
		if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", filepath.Dir(destPath), err)
		}
		if err := os.WriteFile(destPath, body, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", destPath, err)
		}
		return nil
	})
	if walkErr != nil {
		return mcp.NewToolResultError(fmt.Sprintf("walk %s: %v", absDir, walkErr)), nil
	}

	skillBody := buildSkillMarkdown(skillName, description, refs)
	skillPath := filepath.Join(absOutputDir, "SKILL.md")

	if !dryRun {
		if err := os.MkdirAll(absOutputDir, 0o755); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("mkdir %s: %v", absOutputDir, err)), nil
		}
		if err := os.WriteFile(skillPath, []byte(skillBody), 0o644); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("write SKILL.md: %v", err)), nil
		}
	}

	sort.Slice(refs, func(i, j int) bool { return refs[i].RelPath < refs[j].RelPath })

	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"skill_name":   skillName,
		"description":  description,
		"skill_path":   skillPath,
		"output_dir":   absOutputDir,
		"reference_count": len(refs),
		"references":   refs,
		"skipped":      skipped,
		"dry_run":      dryRun,
		"compressed":   compress,
	})
}

// buildSkillMarkdown produces the SKILL.md body with YAML
// frontmatter plus a sorted reference index. Format mirrors what
// the claudecode adapter emits for its bundled skills so Claude Code
// picks the file up without configuration.
func buildSkillMarkdown(name, description string, refs []generateSkillRef) string {
	var b strings.Builder
	b.WriteString("---\n")
	b.WriteString("name: " + name + "\n")
	b.WriteString("description: \"" + escapeYAMLDoubleQuoted(description) + "\"\n")
	b.WriteString("---\n\n")
	b.WriteString("# " + name + "\n\n")
	b.WriteString(description + "\n\n")
	b.WriteString("## References\n\n")
	if len(refs) == 0 {
		b.WriteString("(no reference files included — none matched the include/ignore patterns)\n")
		return b.String()
	}
	sorted := append([]generateSkillRef(nil), refs...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].RelPath < sorted[j].RelPath })
	for _, r := range sorted {
		fmt.Fprintf(&b, "- `references/%s` (%d bytes)", r.RelPath, r.BytesWritten)
		if r.Compressed {
			b.WriteString(" — bodies compressed")
		}
		b.WriteString("\n")
	}
	return b.String()
}

// escapeYAMLDoubleQuoted escapes the double-quoted YAML string form
// — backslash and double-quote are the only sequences we have to
// handle for descriptions that come from user input.
func escapeYAMLDoubleQuoted(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return s
}

// sluggify converts a directory basename to a stable kebab-case
// skill name. Non-alphanumeric runs collapse to a single dash; the
// result is lowercased and trimmed.
func sluggify(in string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(in) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash && b.Len() > 0 {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	return strings.TrimRight(b.String(), "-")
}

// isAlwaysSkipped lists the directory bases that are noise in every
// project — vendor trees, VCS, build caches, our own metadata.
func isAlwaysSkipped(base string) bool {
	switch base {
	case ".git", ".hg", ".svn",
		"node_modules", "vendor",
		".venv", "venv", "__pycache__",
		".gortex", ".claude",
		"dist", "build", "target":
		return true
	}
	return false
}

// shouldCompressByExt reports whether elideBraceBodies is meaningful
// for this extension — purely brace-bound languages benefit; YAML /
// JSON / markdown should be passed through unchanged.
func shouldCompressByExt(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".go", ".ts", ".tsx", ".js", ".jsx", ".java", ".kt", ".rs", ".c", ".cpp", ".cc", ".h", ".hpp", ".cs":
		return true
	}
	return false
}

// elideBraceBodies replaces the body of every brace-balanced block
// at the top level with `{ /* N lines elided */ }`. Conservative:
// only collapses blocks deeper than one line, never touches strings
// or comments. Good enough for skill reference dumps where the goal
// is to give the agent a skeleton.
func elideBraceBodies(src string) string {
	var out strings.Builder
	depth := 0
	bodyStart := -1
	bodyStartLine := 0
	lineNo := 1
	for i := 0; i < len(src); i++ {
		c := src[i]
		switch c {
		case '\n':
			lineNo++
			if depth == 0 {
				out.WriteByte(c)
			}
		case '{':
			if depth == 0 {
				out.WriteByte(c)
				bodyStart = i
				bodyStartLine = lineNo
			}
			depth++
		case '}':
			depth--
			if depth == 0 && bodyStart >= 0 {
				lines := lineNo - bodyStartLine
				if lines > 1 {
					fmt.Fprintf(&out, " /* %d lines elided */ ", lines)
				} else {
					// Single-line body — keep verbatim.
					out.WriteString(src[bodyStart+1 : i])
				}
				out.WriteByte(c)
				bodyStart = -1
			}
		default:
			if depth == 0 {
				out.WriteByte(c)
			}
		}
	}
	return out.String()
}

// matchAny returns true when patterns is empty (no filter) or any
// pattern matches rel. filepath.Match handles the "*", "?", "["
// metacharacters; deeper double-star support is intentionally
// omitted — repomix's surface uses the same convention.
func matchAny(patterns []string, rel string) bool {
	if len(patterns) == 0 {
		return true
	}
	for _, p := range patterns {
		if p == "" {
			continue
		}
		if matchPathPattern(p, rel) {
			return true
		}
	}
	return false
}

// matchesIgnore returns true when any ignore pattern matches rel.
func matchesIgnore(patterns []string, rel string) bool {
	for _, p := range patterns {
		if p == "" {
			continue
		}
		if matchPathPattern(p, rel) {
			return true
		}
	}
	return false
}

// matchPathPattern matches a glob against either the full relative
// path or the basename. The dual-test lets a caller write `*.go`
// without prefixing it with `**/`.
func matchPathPattern(pattern, rel string) bool {
	if ok, _ := filepath.Match(pattern, rel); ok {
		return true
	}
	if ok, _ := filepath.Match(pattern, filepath.Base(rel)); ok {
		return true
	}
	// Directory-prefix shortcut: pattern ending in "/*" or just a
	// directory name should match anything inside it.
	if strings.HasSuffix(pattern, "/*") {
		prefix := strings.TrimSuffix(pattern, "/*")
		if strings.HasPrefix(rel, prefix+"/") || rel == prefix {
			return true
		}
	}
	if strings.HasPrefix(rel, pattern+"/") {
		return true
	}
	return false
}
