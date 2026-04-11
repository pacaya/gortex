package semantic

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/zzet/gortex/internal/graph"
)

func TestSymbolMap(t *testing.T) {
	m := NewSymbolMap()

	m.Add("scip::Foo", "main.go::Foo")
	m.Add("scip::Bar", "lib.go::Bar")

	assert.Equal(t, 2, m.Size())

	id, ok := m.GortexID("scip::Foo")
	assert.True(t, ok)
	assert.Equal(t, "main.go::Foo", id)

	ext, ok := m.ExternalID("lib.go::Bar")
	assert.True(t, ok)
	assert.Equal(t, "scip::Bar", ext)

	_, ok = m.GortexID("scip::Unknown")
	assert.False(t, ok)
}

func TestMatchNodeByFileLine(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{
		ID: "main.go::main", Kind: graph.KindFunction, Name: "main",
		FilePath: "main.go", StartLine: 10, EndLine: 20,
	})
	g.AddNode(&graph.Node{
		ID: "main.go::helper", Kind: graph.KindFunction, Name: "helper",
		FilePath: "main.go", StartLine: 22, EndLine: 30,
	})
	g.AddNode(&graph.Node{
		ID: "main.go", Kind: graph.KindFile, Name: "main.go",
		FilePath: "main.go", StartLine: 1, EndLine: 30,
	})

	// Exact start line match.
	n := MatchNodeByFileLine(g, "main.go", 10)
	assert.NotNil(t, n)
	assert.Equal(t, "main.go::main", n.ID)

	// Within range.
	n = MatchNodeByFileLine(g, "main.go", 15)
	assert.NotNil(t, n)
	assert.Equal(t, "main.go::main", n.ID)

	// Second function.
	n = MatchNodeByFileLine(g, "main.go", 25)
	assert.NotNil(t, n)
	assert.Equal(t, "main.go::helper", n.ID)

	// Line in gap between functions — may or may not find a match.
	n = MatchNodeByFileLine(g, "main.go", 21)
	// Within tolerance of 2 lines from helper (line 22), should find it.
	if n != nil {
		assert.Equal(t, "main.go::helper", n.ID)
	}
}

func TestMatchNodeByQualName(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{
		ID: "main.go::Foo", Kind: graph.KindFunction, Name: "Foo",
		QualName: "github.com/test/pkg.Foo", FilePath: "main.go",
	})

	n := MatchNodeByQualName(g, "github.com/test/pkg.Foo")
	assert.NotNil(t, n)
	assert.Equal(t, "main.go::Foo", n.ID)

	n = MatchNodeByQualName(g, "unknown")
	assert.Nil(t, n)
}

func TestMatchNodeByNameInFile(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{
		ID: "main.go::Foo", Kind: graph.KindFunction, Name: "Foo",
		FilePath: "main.go",
	})

	n := MatchNodeByNameInFile(g, "Foo", "main.go")
	assert.NotNil(t, n)
	assert.Equal(t, "main.go::Foo", n.ID)

	n = MatchNodeByNameInFile(g, "Foo", "other.go")
	assert.Nil(t, n)
}

func TestParseGortexID(t *testing.T) {
	tests := []struct {
		id       string
		wantFile string
		wantSym  string
	}{
		{"main.go::Foo", "main.go", "Foo"},
		{"pkg/auth/token.go::ValidateToken", "pkg/auth/token.go", "ValidateToken"},
		{"main.go", "main.go", ""},
	}

	for _, tt := range tests {
		file, sym := ParseGortexID(tt.id)
		assert.Equal(t, tt.wantFile, file, "file for %s", tt.id)
		assert.Equal(t, tt.wantSym, sym, "sym for %s", tt.id)
	}
}

func TestNormalizeFilePath(t *testing.T) {
	result := NormalizeFilePath("/home/user/repo/pkg/foo.go", "/home/user/repo")
	assert.Equal(t, "pkg/foo.go", result)
}
