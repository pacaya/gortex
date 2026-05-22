package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/parser"
)

func TestDefaultGrammarSymbol(t *testing.T) {
	cases := map[string]string{
		"rust":     "tree_sitter_rust",
		"c-sharp":  "tree_sitter_c_sharp",
		"Foo Bar":  "tree_sitter_foo_bar",
		"my.lang3": "tree_sitter_my_lang3",
	}
	for in, want := range cases {
		assert.Equal(t, want, defaultGrammarSymbol(in), in)
	}
}

func TestNormalizeGrammarExtensions(t *testing.T) {
	got := normalizeGrammarExtensions([]string{"foo", ".bar", "  .baz  ", "", "  "})
	assert.Equal(t, []string{".foo", ".bar", ".baz"}, got)
}

func TestRegisterCustomGrammars_SkipsInvalid(t *testing.T) {
	reg := parser.NewRegistry()
	RegisterCustomGrammars(reg, []config.GrammarSpec{
		{Language: "", Library: "x.so", Extensions: []string{".x"}},        // no language
		{Language: "x", Library: "", Extensions: []string{".x"}},           // no library
		{Language: "x", Library: "x.so", Extensions: nil},                  // no extensions
	}, nil)
	// Nothing valid — registry stays empty.
	assert.Empty(t, reg.SupportedLanguages())
}

func TestRegisterCustomGrammars_SkipsCollisions(t *testing.T) {
	reg := parser.NewRegistry()
	reg.Register(NewGoExtractor()) // claims "go" and ".go"

	// Language collision: a custom "go" grammar must be skipped before
	// any load is attempted.
	RegisterCustomGrammars(reg, []config.GrammarSpec{
		{Language: "go", Library: "/nope/go.so", Extensions: []string{".golang"}},
	}, nil)
	// Extension collision: a custom grammar claiming ".go" is skipped.
	RegisterCustomGrammars(reg, []config.GrammarSpec{
		{Language: "mygo", Library: "/nope/mygo.so", Extensions: []string{".go"}},
	}, nil)

	_, hasMygo := reg.GetByLanguage("mygo")
	assert.False(t, hasMygo)
	// The built-in Go extractor is untouched.
	e, ok := reg.GetByLanguage("go")
	assert.True(t, ok)
	assert.Equal(t, "go", e.Language())
}

func TestRegisterCustomGrammars_MissingLibraryIsSkipped(t *testing.T) {
	reg := parser.NewRegistry()
	// A well-formed spec whose library does not exist must be skipped
	// cleanly — a logged warning, no panic, no registration.
	RegisterCustomGrammars(reg, []config.GrammarSpec{
		{Language: "fictional", Library: "/nonexistent/libtree-sitter-fictional.so", Extensions: []string{".fic"}},
	}, nil)
	_, ok := reg.GetByLanguage("fictional")
	assert.False(t, ok)
}
