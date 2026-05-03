package coverage

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestParse_BasicProfile(t *testing.T) {
	profile := []byte(`mode: set
github.com/foo/bar/pkg/a.go:5.13,8.2 2 1
github.com/foo/bar/pkg/a.go:10.13,15.2 4 0
github.com/foo/bar/pkg/b.go:1.13,3.2 1 1
`)
	got := Parse(profile)
	if len(got) != 3 {
		t.Fatalf("expected 3 segments, got %d: %+v", len(got), got)
	}
	if got[0].File != "github.com/foo/bar/pkg/a.go" {
		t.Errorf("file = %q", got[0].File)
	}
	if got[0].StartLine != 5 || got[0].EndLine != 8 {
		t.Errorf("range = %d-%d", got[0].StartLine, got[0].EndLine)
	}
	if got[0].NumStmt != 2 || got[0].Count != 1 {
		t.Errorf("stmt/count = %d/%d", got[0].NumStmt, got[0].Count)
	}
	// Second segment is the uncovered block.
	if got[1].Count != 0 || got[1].NumStmt != 4 {
		t.Errorf("uncovered segment wrong: %+v", got[1])
	}
}

func TestParse_SkipsMalformed(t *testing.T) {
	profile := []byte(`mode: set
github.com/x/y/pkg/a.go:5.13,8.2 2 1
this line is not a segment
github.com/x/y/pkg/a.go:bad 1 1
github.com/x/y/pkg/b.go:1.13,3.2 1 1
`)
	got := Parse(profile)
	if len(got) != 2 {
		t.Errorf("expected 2 valid segments (malformed skipped), got %d", len(got))
	}
}

func TestProjectStats(t *testing.T) {
	segments := []Segment{
		{StartLine: 5, EndLine: 8, NumStmt: 2, Count: 1},  // covered
		{StartLine: 10, EndLine: 15, NumStmt: 4, Count: 0}, // uncovered
		{StartLine: 20, EndLine: 22, NumStmt: 1, Count: 1}, // outside range
	}
	stats := projectStats(segments, 1, 16)
	if stats.NumStmt != 6 {
		t.Errorf("num_stmt = %d, want 6", stats.NumStmt)
	}
	if stats.Hit != 2 {
		t.Errorf("hit = %d, want 2 (only first segment is covered)", stats.Hit)
	}
	if pct := stats.Percent(); pct < 33.32 || pct > 33.34 {
		t.Errorf("percent = %f, want ~33.33", pct)
	}
}

func TestProjectStats_NoCoverage(t *testing.T) {
	stats := projectStats(nil, 1, 100)
	if stats.NumStmt != 0 {
		t.Errorf("empty segments should yield zero stats")
	}
	if stats.Percent() != -1 {
		t.Errorf("no-measurement percent should be -1, got %f", stats.Percent())
	}
}

func TestEnrichGraph_StampsMetaCoveragePct(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{
		ID:        "pkg/a.go::Foo",
		Kind:      graph.KindFunction,
		FilePath:  "pkg/a.go",
		StartLine: 1,
		EndLine:   20,
	})
	g.AddNode(&graph.Node{
		ID:        "pkg/a.go::Bar",
		Kind:      graph.KindFunction,
		FilePath:  "pkg/a.go",
		StartLine: 25,
		EndLine:   30,
	})

	segs := []Segment{
		{File: "github.com/foo/bar/pkg/a.go", StartLine: 5, EndLine: 8, NumStmt: 2, Count: 1},
		{File: "github.com/foo/bar/pkg/a.go", StartLine: 10, EndLine: 15, NumStmt: 4, Count: 0},
		{File: "github.com/foo/bar/pkg/a.go", StartLine: 27, EndLine: 28, NumStmt: 1, Count: 1},
	}
	enriched := EnrichGraph(g, segs, "github.com/foo/bar")
	if enriched != 2 {
		t.Errorf("expected 2 enriched, got %d", enriched)
	}

	foo := g.GetNode("pkg/a.go::Foo")
	pct, _ := foo.Meta["coverage_pct"].(float64)
	if pct < 33.32 || pct > 33.34 {
		t.Errorf("Foo pct = %v, want ~33.33", pct)
	}
	bar := g.GetNode("pkg/a.go::Bar")
	pct, _ = bar.Meta["coverage_pct"].(float64)
	if pct != 100 {
		t.Errorf("Bar pct = %v, want 100", pct)
	}
}

func TestEnrichGraph_SkipsNonExecutable(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{
		ID: "pkg/a.go::T", Kind: graph.KindType,
		FilePath: "pkg/a.go", StartLine: 1, EndLine: 10,
	})
	segs := []Segment{
		{File: "pkg/a.go", StartLine: 5, EndLine: 8, NumStmt: 2, Count: 1},
	}
	if got := EnrichGraph(g, segs, ""); got != 0 {
		t.Errorf("KindType should not be enriched, got %d", got)
	}
}

func TestEnrichGraph_HandlesUnprefixedPaths(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{
		ID:        "pkg/a.go::Foo",
		Kind:      graph.KindFunction,
		FilePath:  "pkg/a.go",
		StartLine: 1,
		EndLine:   10,
	})
	// Profile without a module prefix.
	segs := []Segment{
		{File: "pkg/a.go", StartLine: 5, EndLine: 8, NumStmt: 2, Count: 1},
	}
	if got := EnrichGraph(g, segs, ""); got != 1 {
		t.Errorf("expected 1 enriched (no prefix-strip path), got %d", got)
	}
}

func TestStripModulePrefix(t *testing.T) {
	cases := []struct {
		file, mod, want string
	}{
		{"github.com/foo/bar/pkg/a.go", "github.com/foo/bar", "pkg/a.go"},
		{"pkg/a.go", "", "pkg/a.go"},
		{"./pkg/a.go", "", "pkg/a.go"},
		{"github.com/x/y/a.go", "github.com/foo/bar", "github.com/x/y/a.go"},
	}
	for _, c := range cases {
		got := stripModulePrefix(c.file, c.mod)
		if got != c.want {
			t.Errorf("stripModulePrefix(%q,%q) = %q, want %q", c.file, c.mod, got, c.want)
		}
	}
}

func TestReadModulePath(t *testing.T) {
	dir := t.TempDir()
	if err := writeFile(filepath.Join(dir, "go.mod"), "module github.com/foo/bar\n\ngo 1.22\n"); err != nil {
		t.Fatal(err)
	}
	got := ReadModulePath(dir)
	if got != "github.com/foo/bar" {
		t.Errorf("got %q", got)
	}
}

func TestReadModulePath_NoFile(t *testing.T) {
	if got := ReadModulePath(t.TempDir()); got != "" {
		t.Errorf("expected empty string for missing go.mod, got %q", got)
	}
}

func TestRoundTwo(t *testing.T) {
	cases := []struct{ in, want float64 }{
		{33.3333, 33.33},
		{99.999, 100},
		{0, 0},
		{-1, -1},
	}
	for _, c := range cases {
		if got := roundTwo(c.in); got != c.want {
			t.Errorf("roundTwo(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

// helpers

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
}
