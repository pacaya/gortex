package entrypoints

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

func TestDetect_Alembic(t *testing.T) {
	nodes := []*graph.Node{
		{ID: "alembic/versions/abc.py", Kind: graph.KindFile, FilePath: "alembic/versions/abc.py"},
		{ID: "f::upgrade", Kind: graph.KindFunction, Name: "upgrade"},
		{ID: "f::downgrade", Kind: graph.KindFunction, Name: "downgrade"},
		{ID: "f::revision", Kind: graph.KindVariable, Name: "revision"},
	}
	require.Equal(t, 3, Detect("alembic/versions/abc.py", "python", nodes, nil))
	require.Equal(t, true, nodes[0].Meta[MetaEntryPoint])
	require.Equal(t, "alembic:migration", nodes[0].Meta[MetaEntryKind])
	require.Equal(t, true, nodes[1].Meta[MetaEntryPoint]) // upgrade
	require.Equal(t, true, nodes[2].Meta[MetaEntryPoint]) // downgrade
	require.Nil(t, nodes[3].Meta)                         // revision var not stamped
}

func TestDetect_AlembicRequiresFullSignature(t *testing.T) {
	// upgrade() alone — no downgrade, no revision — is not Alembic.
	nodes := []*graph.Node{
		{ID: "f.py", Kind: graph.KindFile, FilePath: "f.py"},
		{ID: "f::upgrade", Kind: graph.KindFunction, Name: "upgrade"},
	}
	require.Equal(t, 0, Detect("f.py", "python", nodes, nil))
}

func TestDetect_NextJSPagesRouter(t *testing.T) {
	nodes := []*graph.Node{
		{ID: "pages/users.tsx", Kind: graph.KindFile, FilePath: "pages/users.tsx"},
		{ID: "f::getServerSideProps", Kind: graph.KindFunction, Name: "getServerSideProps"},
	}
	require.Equal(t, 2, Detect("pages/users.tsx", "typescript", nodes, nil))
	require.Equal(t, "nextjs:page", nodes[0].Meta[MetaEntryKind])
	require.Equal(t, true, nodes[1].Meta[MetaEntryPoint])
}

func TestDetect_NextJSAppRouter(t *testing.T) {
	page := []*graph.Node{{ID: "src/app/dashboard/page.tsx", Kind: graph.KindFile, FilePath: "src/app/dashboard/page.tsx"}}
	require.Equal(t, 1, Detect("src/app/dashboard/page.tsx", "typescript", page, nil))
	require.Equal(t, "nextjs:page", page[0].Meta[MetaEntryKind])

	route := []*graph.Node{{ID: "app/api/users/route.ts", Kind: graph.KindFile, FilePath: "app/api/users/route.ts"}}
	require.Equal(t, 1, Detect("app/api/users/route.ts", "typescript", route, nil))
	require.Equal(t, "nextjs:route", route[0].Meta[MetaEntryKind])
}

func TestDetect_NextJSGenericAppDirIgnored(t *testing.T) {
	// A non-special file under app/ must NOT be flagged Next.js.
	nodes := []*graph.Node{{ID: "app/helpers.ts", Kind: graph.KindFile, FilePath: "app/helpers.ts"}}
	require.Equal(t, 0, Detect("app/helpers.ts", "typescript", nodes, nil))
}

func TestDetect_ASPNetHost(t *testing.T) {
	nodes := []*graph.Node{
		{ID: "Program.cs", Kind: graph.KindFile, FilePath: "Program.cs"},
		{ID: "p::Main", Kind: graph.KindMethod, Name: "Main"},
		{ID: "p::Helper", Kind: graph.KindMethod, Name: "Helper"},
	}
	require.Equal(t, 2, Detect("src/Program.cs", "csharp", nodes, nil)) // file + Main
	require.Equal(t, "aspnet:host", nodes[0].Meta[MetaEntryKind])
	require.Equal(t, true, nodes[1].Meta[MetaEntryPoint])
	require.Nil(t, nodes[2].Meta) // Helper is not a lifecycle method
}

func TestDetect_NonEntryFileIgnored(t *testing.T) {
	nodes := []*graph.Node{{ID: "src/util.go", Kind: graph.KindFile, FilePath: "src/util.go"}}
	require.Equal(t, 0, Detect("src/util.go", "go", nodes, nil))
}

func TestDetect_JavaSpringControllerAndMain(t *testing.T) {
	nodes := []*graph.Node{
		{ID: "UserController.java", Kind: graph.KindFile, FilePath: "UserController.java", Language: "java"},
		{ID: "UserController.java::UserController", Kind: graph.KindType, Name: "UserController"},
		{ID: "UserController.java::list", Kind: graph.KindMethod, Name: "list"},     // @GetMapping
		{ID: "UserController.java::helper", Kind: graph.KindMethod, Name: "helper"}, // private, no annotation
		{ID: "UserController.java::main", Kind: graph.KindMethod, Name: "main"},     // JVM entry
	}
	edges := []*graph.Edge{
		{From: "UserController.java::UserController", To: "annotation::java::RestController", Kind: graph.EdgeAnnotated},
		{From: "UserController.java::list", To: "annotation::java::GetMapping", Kind: graph.EdgeAnnotated},
	}
	// controller class + list handler + main = 3 (NOT the file node, NOT the helper).
	require.Equal(t, 3, Detect("UserController.java", "java", nodes, edges))
	require.Nil(t, nodes[0].Meta, "Java stamps annotated members, never the whole file")
	require.Equal(t, "spring:controller", nodes[1].Meta[MetaEntryKind])
	require.Equal(t, true, nodes[2].Meta[MetaEntryPoint])
	require.Equal(t, "spring:handler", nodes[2].Meta[MetaEntryKind])
	require.Nil(t, nodes[3].Meta, "a private helper with no entry annotation stays a dead-code candidate")
	require.Equal(t, "java:main", nodes[4].Meta[MetaEntryKind])
}

func TestDetect_JavaJUnitTest(t *testing.T) {
	nodes := []*graph.Node{{ID: "T.java::shouldWork", Kind: graph.KindMethod, Name: "shouldWork"}}
	edges := []*graph.Edge{{From: "T.java::shouldWork", To: "annotation::java::Test", Kind: graph.EdgeAnnotated}}
	require.Equal(t, 1, Detect("T.java", "java", nodes, edges))
	require.Equal(t, "junit:test", nodes[0].Meta[MetaEntryKind])
}
