package resolver

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

func sqlFnNode(g graph.Store, id, name string) {
	g.AddNode(&graph.Node{ID: id, Kind: graph.KindFunction, Name: name, FilePath: "schema.sql", Language: "sql"})
}

func sqlCaller(g graph.Store, callerID, fn string) *graph.Edge {
	g.AddNode(&graph.Node{ID: callerID, Kind: graph.KindFunction, Name: lastSeg(callerID), FilePath: "app.ts", Language: "typescript"})
	e := &graph.Edge{
		From: callerID, To: "unresolved::sqlfn::" + fn, Kind: graph.EdgeCalls,
		FilePath: "app.ts", Line: 2, Meta: map[string]any{"via": sqlCallsiteVia, "sql_function": fn},
	}
	g.AddEdge(e)
	return e
}

func TestResolveSQLCallsites_Lands(t *testing.T) {
	g := graph.New()
	e := sqlCaller(g, "app.ts::load", "get_user_stats")
	sqlFnNode(g, "schema.sql::get_user_stats", "get_user_stats")

	n := ResolveSQLCallsites(g)
	assert.Equal(t, 1, n)
	assert.Equal(t, "schema.sql::get_user_stats", e.To)
	assert.Equal(t, graph.OriginASTInferred, e.Origin)
	assert.Equal(t, SynthSQLCallsite, e.Meta[MetaSynthesizedBy])
	require.Len(t, g.GetInEdges("schema.sql::get_user_stats"), 1)
}

func TestResolveSQLCallsites_AmbiguousStaysPlaceholder(t *testing.T) {
	g := graph.New()
	e := sqlCaller(g, "app.ts::load", "calc")
	sqlFnNode(g, "a.sql::calc", "calc")
	sqlFnNode(g, "b.sql::calc", "calc")
	assert.Equal(t, 0, ResolveSQLCallsites(g))
	assert.Equal(t, "unresolved::sqlfn::calc", e.To, "ambiguous SQL function stays unresolved")
}

func TestResolveSQLCallsites_NoSQLFunction(t *testing.T) {
	g := graph.New()
	sqlCaller(g, "app.ts::load", "ghost")
	assert.Equal(t, 0, ResolveSQLCallsites(g))
}

func TestResolveSQLCallsites_Idempotent(t *testing.T) {
	g := graph.New()
	sqlCaller(g, "app.ts::load", "fn")
	sqlFnNode(g, "schema.sql::fn", "fn")
	first := ResolveSQLCallsites(g)
	second := ResolveSQLCallsites(g)
	assert.Equal(t, first, second)
	assert.Equal(t, 1, second)
}
