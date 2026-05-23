package audit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// fakeGraph satisfies GraphLookup with an in-memory set of known names.
type fakeGraph struct {
	byName map[string][]*graph.Node
	byQual map[string]*graph.Node
	byID   map[string]*graph.Node
}

func newFakeGraph(names ...string) *fakeGraph {
	fg := &fakeGraph{
		byName: make(map[string][]*graph.Node),
		byQual: make(map[string]*graph.Node),
		byID:   make(map[string]*graph.Node),
	}
	for _, n := range names {
		node := &graph.Node{ID: n, Name: n}
		fg.byName[n] = append(fg.byName[n], node)
		fg.byQual[n] = node
		fg.byID[n] = node
	}
	return fg
}

func (fg *fakeGraph) GetNode(id string) *graph.Node          { return fg.byID[id] }
func (fg *fakeGraph) GetNodeByQualName(q string) *graph.Node { return fg.byQual[q] }
func (fg *fakeGraph) FindNodesByName(n string) []*graph.Node { return fg.byName[n] }
func (fg *fakeGraph) GetFileNodes(p string) []*graph.Node    { return nil }

func TestClassifyToken(t *testing.T) {
	cases := []struct {
		in   string
		want tokenKind
	}{
		// Bare lowercase-first identifiers used to land in tokenSymbol;
		// they're now tokenOther because docs vocabulary
		// (`additionalProperties`, `generateContent`, `responseSchema`)
		// is camelCase and indistinguishable from internal symbols.
		// A qualified or call-form variant still classifies.
		{"handleAuditAgentConfig", tokenOther},
		{"Server.handleAuditAgentConfig", tokenSymbol},
		{"register_tools()", tokenSymbol},
		{"Parser", tokenSymbol}, // capital, 6 chars
		{"go", tokenOther},      // skip list
		{"true", tokenOther},
		{"https://example.com", tokenOther},
		{"internal/audit/audit.go", tokenPath},
		{"CLAUDE.md", tokenPath},
		{".gortex.yaml", tokenPath},
		{"foo", tokenOther}, // lowercase, no signal
		{"", tokenOther},
		// Regressions from the post-fix audit_agent_config report:
		// env vars, JSON-schema keys, placeholder paths, HTTP routes,
		// MCP method names, JSON pointer syntax, and ~paths must all
		// stay out of stale / dead refs.
		{"ANTHROPIC_API_KEY", tokenOther},
		{"AWS_ACCESS_KEY_ID", tokenOther},
		{"GORTEX_LLM_PROVIDER", tokenOther},
		{"additionalProperties", tokenOther},
		{"generateContent", tokenOther},
		{"responseSchema", tokenOther},
		{"<exact-name>", tokenOther},
		{"POST /mcp", tokenOther},
		{"git status", tokenOther},
		{"notifications/tools/list_changed", tokenOther},
		// `pkg/foo.go::Bar` is the canonical docs placeholder shape;
		// it never resolves to a real graph node or a real file. The
		// classifier returns tokenOther because the embedded `::`
		// disqualifies it from path classification AND the embedded
		// `/` disqualifies it from the symbol identifier regexp.
		{"pkg/foo.go::Bar", tokenOther},
		{"~/.claude/CLAUDE.md", tokenPath}, // ~ + extension
	}
	for _, c := range cases {
		got := classifyToken(c.in)
		if got != c.want {
			t.Errorf("classifyToken(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestExtractBackticked(t *testing.T) {
	cases := []struct {
		line string
		want []string
	}{
		{"Use `foo` and `bar.Baz()` here.", []string{"foo", "bar.Baz()"}},
		{"No backticks.", nil},
		{"```go", nil}, // fenced
		{"mix `a` then ``` code ``` then `b`", []string{"a", "b"}},
		{"single ` unmatched", nil},
	}
	for _, c := range cases {
		got := extractBackticked(c.line)
		if !equalStringSlices(got, c.want) {
			t.Errorf("extractBackticked(%q) = %v, want %v", c.line, got, c.want)
		}
	}
}

func TestAuditDetectsStaleAndDead(t *testing.T) {
	tmp := t.TempDir()

	// Create a stub file referenced in config.
	realPath := filepath.Join(tmp, "internal/real.go")
	if err := os.MkdirAll(filepath.Dir(realPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(realPath, []byte("package real\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Use capitalized symbol names so they pass the post-fix
	// classifier — bare lowercase-first camelCase is no longer
	// considered a stale-symbol candidate (it's indistinguishable
	// from docs vocabulary like additionalProperties).
	claudeMd := `# CLAUDE.md

## Tools
- Use ` + "`HandleLive`" + ` for live ops.
- Removed: ` + "`HandleStale`" + ` (gone in v2).

## Paths
- See ` + "`internal/real.go`" + ` (exists).
- Old: ` + "`internal/ghost.go`" + ` (deleted).
`
	mdPath := filepath.Join(tmp, "CLAUDE.md")
	if err := os.WriteFile(mdPath, []byte(claudeMd), 0o644); err != nil {
		t.Fatal(err)
	}

	fg := newFakeGraph("HandleLive")
	rep := Audit(fg, tmp, []string{"CLAUDE.md"})

	if rep.FilesScanned != 1 {
		t.Fatalf("FilesScanned = %d, want 1", rep.FilesScanned)
	}

	if !containsToken(rep.StaleRefs, "HandleStale") {
		t.Errorf("expected HandleStale in stale refs, got %+v", rep.StaleRefs)
	}
	if containsToken(rep.StaleRefs, "HandleLive") {
		t.Errorf("HandleLive should NOT be stale — it's in the graph")
	}

	if !containsPath(rep.DeadPaths, "internal/ghost.go") {
		t.Errorf("expected internal/ghost.go in dead paths, got %+v", rep.DeadPaths)
	}
	if containsPath(rep.DeadPaths, "internal/real.go") {
		t.Errorf("internal/real.go should NOT be dead — it exists on disk")
	}

	if len(rep.Suggestions) == 0 {
		t.Errorf("expected suggestions when stale/dead refs present")
	}
}

func TestAuditBloatScore(t *testing.T) {
	tmp := t.TempDir()

	var sb strings.Builder
	sb.WriteString("# Big\n\n")
	// 2000 bullet lines with duplicates and long text.
	long := strings.Repeat("x", 250)
	for i := 0; i < 2000; i++ {
		sb.WriteString("- duplicated bullet " + long + "\n")
	}
	mdPath := filepath.Join(tmp, "CLAUDE.md")
	if err := os.WriteFile(mdPath, []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	rep := Audit(newFakeGraph(), tmp, []string{"CLAUDE.md"})
	if rep.BloatScore < 60 {
		t.Errorf("BloatScore = %d, want >= 60 for a massive duplicate-bullet file", rep.BloatScore)
	}
}

func TestAuditCleanConfig(t *testing.T) {
	tmp := t.TempDir()
	clean := "# Clean\n\nShort doc with no stale refs.\n"
	if err := os.WriteFile(filepath.Join(tmp, "CLAUDE.md"), []byte(clean), 0o644); err != nil {
		t.Fatal(err)
	}

	rep := Audit(newFakeGraph(), tmp, []string{"CLAUDE.md"})
	if len(rep.StaleRefs) != 0 {
		t.Errorf("expected no stale refs, got %+v", rep.StaleRefs)
	}
	if len(rep.DeadPaths) != 0 {
		t.Errorf("expected no dead paths, got %+v", rep.DeadPaths)
	}
	if rep.BloatScore != 0 {
		t.Errorf("expected bloat score 0, got %d", rep.BloatScore)
	}
}

func TestDiscoverConfigFiles(t *testing.T) {
	tmp := t.TempDir()
	_ = os.WriteFile(filepath.Join(tmp, "CLAUDE.md"), []byte("x"), 0o644)
	_ = os.WriteFile(filepath.Join(tmp, "AGENTS.md"), []byte("x"), 0o644)
	_ = os.MkdirAll(filepath.Join(tmp, ".cursor/rules"), 0o755)
	_ = os.WriteFile(filepath.Join(tmp, ".cursor/rules/foo.md"), []byte("x"), 0o644)

	files := DiscoverConfigFiles(tmp)
	if len(files) != 3 {
		t.Fatalf("expected 3 files, got %d: %v", len(files), files)
	}
	want := map[string]bool{
		"CLAUDE.md":            true,
		"AGENTS.md":            true,
		".cursor/rules/foo.md": true,
	}
	for _, f := range files {
		if !want[f] {
			t.Errorf("unexpected file: %s", f)
		}
	}
}

// ---- helpers ----

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func containsToken(refs []StaleRef, tok string) bool {
	for _, r := range refs {
		if r.Token == tok {
			return true
		}
	}
	return false
}

func containsPath(paths []DeadPath, p string) bool {
	for _, d := range paths {
		if d.Path == p {
			return true
		}
	}
	return false
}
