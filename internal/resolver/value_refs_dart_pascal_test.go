package resolver

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
)

// loadExtract runs an extractor's output into a fresh graph so a resolver pass
// can act on the same nodes/edges the indexer would settle.
func loadExtract(t *testing.T, g graph.Store, res *parser.ExtractionResult) {
	t.Helper()
	for _, n := range res.Nodes {
		g.AddNode(n)
	}
	for _, e := range res.Edges {
		g.AddEdge(e)
	}
}

// TestValueRefDart_EndToEnd extracts a Dart file whose function reads a
// distinctive top-level constant, then confirms ResolveValueRefs binds the
// captured candidate into a tiered EdgeReads from the reader to the constant —
// so the reader lands in the constant's blast radius.
func TestValueRefDart_EndToEnd(t *testing.T) {
	src := `const MAX_RETRIES = 3;

void useIt() {
  print(MAX_RETRIES);
}
`
	res, err := languages.NewDartExtractor().Extract("cfg.dart", []byte(src))
	require.NoError(t, err)

	g := graph.New()
	loadExtract(t, g, res)

	n := ResolveValueRefs(g)
	assert.GreaterOrEqual(t, n, 1, "the distinctive Dart constant read must bind")

	e := readsEdge(g, "cfg.dart::useIt", "cfg.dart::MAX_RETRIES")
	require.NotNil(t, e, "useIt should gain a value-ref EdgeReads to MAX_RETRIES")
	assert.Equal(t, graph.OriginASTResolved, e.Origin, "the read must ride a provenance tier")

	// Impact-radius property: the reader is among the constant's incoming
	// (non-Defines/MemberOf) edges, which blast-radius analysis walks.
	var inRadius bool
	for _, in := range g.GetInEdges("cfg.dart::MAX_RETRIES") {
		if in.From == "cfg.dart::useIt" && in.Kind != graph.EdgeDefines && in.Kind != graph.EdgeMemberOf {
			inRadius = true
		}
	}
	assert.True(t, inRadius, "useIt must appear in MAX_RETRIES' impact radius")
}

// TestValueRefPascal_EndToEnd does the same for a Pascal unit-level constant.
func TestValueRefPascal_EndToEnd(t *testing.T) {
	src := `unit Foo;
interface
const
  MAX_RETRIES = 3;
implementation
procedure UseIt;
begin
  WriteLn(MAX_RETRIES);
end;
end.
`
	res, err := languages.NewPascalExtractor().Extract("foo.pas", []byte(src))
	require.NoError(t, err)

	g := graph.New()
	loadExtract(t, g, res)

	n := ResolveValueRefs(g)
	assert.GreaterOrEqual(t, n, 1, "the distinctive Pascal constant read must bind")

	e := readsEdge(g, "foo.pas::UseIt", "foo.pas::MAX_RETRIES")
	require.NotNil(t, e, "UseIt should gain a value-ref EdgeReads to MAX_RETRIES")
	assert.Equal(t, graph.OriginASTResolved, e.Origin, "the read must ride a provenance tier")

	var inRadius bool
	for _, in := range g.GetInEdges("foo.pas::MAX_RETRIES") {
		if in.From == "foo.pas::UseIt" && in.Kind != graph.EdgeDefines && in.Kind != graph.EdgeMemberOf {
			inRadius = true
		}
	}
	assert.True(t, inRadius, "UseIt must appear in MAX_RETRIES' impact radius")
}
