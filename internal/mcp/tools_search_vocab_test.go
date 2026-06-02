package mcp

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search"
)

// TestAnchorTermsToVocabulary_FiltersOutOfVocab confirms the LLM
// expansion post-filter keeps only terms present in the mined
// vocabulary and drops the rest.
func TestAnchorTermsToVocabulary_FiltersOutOfVocab(t *testing.T) {
	g := graph.New()
	for i, n := range []string{"parsePayload", "decodeToken", "encodeSession"} {
		g.AddNode(&graph.Node{
			ID: "pkg/" + n + ".go::" + n, Kind: graph.KindFunction, Name: n,
			FilePath: "pkg/" + n + ".go", StartLine: i + 1, EndLine: i + 2, Language: "go",
		})
	}
	ac := search.BuildAutoConcepts(g)
	require.Positive(t, ac.VocabularySize())

	// "token" and "session" are in the corpus; "authenticator" and
	// "oauth" are plausible LLM inventions that no symbol uses.
	in := []string{"token", "authenticator", "session", "oauth"}
	got := anchorTermsToVocabulary(in, ac)
	require.ElementsMatch(t, []string{"token", "session"}, got,
		"anchoring should keep in-vocab terms and drop the rest")
}

// TestAnchorTermsToVocabulary_DegradesOnEmptyVocab confirms an empty
// or nil vocabulary returns the input unchanged — anchoring must never
// silence expansion on a repo that hasn't mined a vocabulary yet.
func TestAnchorTermsToVocabulary_DegradesOnEmptyVocab(t *testing.T) {
	in := []string{"token", "authenticator"}

	// Nil AutoConcepts (the pre-RunAnalysis state).
	require.Equal(t, in, anchorTermsToVocabulary(in, nil))

	// Empty graph -> empty vocabulary -> unconstrained passthrough.
	empty := search.BuildAutoConcepts(graph.New())
	require.Equal(t, 0, empty.VocabularySize())
	require.Equal(t, in, anchorTermsToVocabulary(in, empty))

	// Empty input is a clean no-op either way.
	require.Empty(t, anchorTermsToVocabulary(nil, empty))
}

// TestVocabAnchoredExpansionDefault confirms the config default
// resolution: nil pointer -> off, explicit true/false honoured.
func TestVocabAnchoredExpansionDefault(t *testing.T) {
	require.False(t, config.SearchConfig{}.VocabAnchoredExpansionDefault(),
		"a nil VocabAnchoredExpansion pointer must default to off")
	on := true
	require.True(t, config.SearchConfig{VocabAnchoredExpansion: &on}.VocabAnchoredExpansionDefault())
	off := false
	require.False(t, config.SearchConfig{VocabAnchoredExpansion: &off}.VocabAnchoredExpansionDefault())
}

// TestSearchSymbols_VocabAnchoredArgNoLLMNoop is a defensive
// integration check: with NO LLM provider configured the
// vocab_anchored argument is inert — search still works and the arg
// neither errors nor changes the deterministic-channel result.
func TestSearchSymbols_VocabAnchoredArgNoLLMNoop(t *testing.T) {
	srv := equivalenceTestServer(t, []string{"LoginService", "ParseConfig"}, nil)
	// vocab_anchored only gates the LLM channel, which is off here, so
	// the equivalence bridge ("auth" -> LoginService) is unaffected.
	base := searchIDs(t, srv, map[string]any{"query": "auth"})
	anchored := searchIDs(t, srv, map[string]any{"query": "auth", "vocab_anchored": true})
	require.Equal(t, base, anchored,
		"vocab_anchored must be inert when the LLM expansion channel is off")
}
