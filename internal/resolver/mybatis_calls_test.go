package resolver

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// myBatisTestGraph builds the minimal shape ResolveMyBatisCalls consumes:
// a SQL-statement node with a mybatis.mapper placeholder call edge, plus a
// Java DAO interface method node it should land on.
type myBatisTestGraph struct {
	g graph.Store
}

func newMyBatisTestGraph() *myBatisTestGraph { return &myBatisTestGraph{g: graph.New()} }

// addStatement adds the mapper-XML statement node and its placeholder call
// edge, exactly as the MyBatis extractor would emit them.
func (b *myBatisTestGraph) addStatement(namespace, stmtID, filePath string) (*graph.Node, *graph.Edge) {
	nodeID := namespace + "::" + stmtID
	n := &graph.Node{
		ID: nodeID, Kind: graph.KindMethod, Name: stmtID,
		FilePath: filePath, Language: "mybatis",
		Meta: map[string]any{
			"mybatis_namespace": namespace,
			"mybatis_statement": stmtID,
			"mybatis_sql_kind":  "select",
		},
	}
	b.g.AddNode(n)
	e := &graph.Edge{
		From: nodeID, To: myBatisStubPlaceholder(namespace, stmtID),
		Kind: graph.EdgeCalls, FilePath: filePath, Line: 4,
		Meta: map[string]any{
			"via":               "mybatis.mapper",
			"mybatis_namespace": namespace,
			"mybatis_statement": stmtID,
		},
	}
	b.g.AddEdge(e)
	return n, e
}

// addJavaMethod adds a Java DAO/Mapper interface method node shaped like
// the Java extractor's output: ID `<filePath>::<Class>.<method>` with a
// `receiver` Meta.
func (b *myBatisTestGraph) addJavaMethod(filePath, class, method, repo string) *graph.Node {
	id := filePath + "::" + class + "." + method
	n := &graph.Node{
		ID: id, Kind: graph.KindMethod, Name: method,
		FilePath: filePath, RepoPrefix: repo, Language: "java",
		Meta: map[string]any{"receiver": class},
	}
	b.g.AddNode(n)
	return n
}

func TestResolveMyBatisCalls_LandsOnJavaMethod(t *testing.T) {
	b := newMyBatisTestGraph()
	_, stubEdge := b.addStatement("com.app.UserMapper", "findUser", "mappers/UserMapper.xml")
	javaMethod := b.addJavaMethod("src/com/app/UserMapper.java", "UserMapper", "findUser", "")
	// A same-named method on an unrelated class must not be matched.
	b.addJavaMethod("src/com/app/OrderMapper.java", "OrderMapper", "findUser", "")

	resolved := ResolveMyBatisCalls(b.g)
	require.Equal(t, 1, resolved)

	require.Equal(t, javaMethod.ID, stubEdge.To, "stub edge should land on the Java DAO method")
	require.Equal(t, graph.OriginASTInferred, stubEdge.Origin)
	require.Equal(t, "mybatis", stubEdge.Meta["synthesized_by"])
	require.Equal(t, "heuristic", stubEdge.Meta["provenance"])
}

func TestResolveMyBatisCalls_Idempotent(t *testing.T) {
	b := newMyBatisTestGraph()
	_, stubEdge := b.addStatement("com.app.UserMapper", "findUser", "mappers/UserMapper.xml")
	javaMethod := b.addJavaMethod("src/com/app/UserMapper.java", "UserMapper", "findUser", "")

	require.Equal(t, 1, ResolveMyBatisCalls(b.g))
	require.Equal(t, 1, ResolveMyBatisCalls(b.g)) // stable re-run
	require.Equal(t, javaMethod.ID, stubEdge.To)
}

func TestResolveMyBatisCalls_NoMatchStaysPlaceholder(t *testing.T) {
	b := newMyBatisTestGraph()
	_, stubEdge := b.addStatement("com.app.UserMapper", "findUser", "mappers/UserMapper.xml")
	// No matching Java method present.

	require.Equal(t, 0, ResolveMyBatisCalls(b.g))
	require.Equal(t, myBatisStubPlaceholder("com.app.UserMapper", "findUser"), stubEdge.To)
	require.Empty(t, stubEdge.Origin)
	require.Nil(t, stubEdge.Meta["synthesized_by"])
}

func TestResolveMyBatisCalls_PrefersSameRepo(t *testing.T) {
	b := newMyBatisTestGraph()
	stmt, stubEdge := b.addStatement("com.app.UserMapper", "findUser", "mappers/UserMapper.xml")
	// Place the statement's caller (the From node) in repo "svc-a".
	stmt.RepoPrefix = "svc-a"
	b.g.AddNode(stmt)

	want := b.addJavaMethod("a/com/app/UserMapper.java", "UserMapper", "findUser", "svc-a")
	b.addJavaMethod("b/com/app/UserMapper.java", "UserMapper", "findUser", "svc-b")

	require.Equal(t, 1, ResolveMyBatisCalls(b.g))
	require.Equal(t, want.ID, stubEdge.To)
}
