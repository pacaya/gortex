package analysis

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

// addJavaMethod adds a Java method node with a recorded visibility and
// no incoming edges (i.e. uncalled in the indexed graph).
func addJavaMethod(g *graph.Graph, file, name, vis string) string {
	id := file + "::" + name
	g.AddNode(&graph.Node{
		ID: id, Kind: graph.KindMethod, Name: name,
		FilePath: file, Language: "java", StartLine: 1, EndLine: 3,
		Meta: map[string]any{"visibility": vis},
	})
	return id
}

// TestFindDeadCode_JavaVisibility is the parity fix: dead-code must read
// Java's recorded public/private modifier instead of the Go-style /
// underscore name heuristic (which marked every Java symbol exported and
// skipped the whole language).
func TestFindDeadCode_JavaVisibility(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "Svc.java", Kind: graph.KindFile, FilePath: "Svc.java", Language: "java"})

	privDead := addJavaMethod(g, "Svc.java", "computeInternal", "private")
	pkgDead := addJavaMethod(g, "Svc.java", "helperPkg", "package")
	pubAPI := addJavaMethod(g, "Svc.java", "doWork", "public")
	protAPI := addJavaMethod(g, "Svc.java", "onInit", "protected")

	dead := map[string]bool{}
	for _, d := range FindDeadCode(g, nil, nil) {
		dead[d.ID] = true
	}

	require.True(t, dead[privDead], "an uncalled private method is genuinely dead")
	require.True(t, dead[pkgDead], "an uncalled package-private method is genuinely dead")
	require.False(t, dead[pubAPI], "a public method is API surface, not dead")
	require.False(t, dead[protAPI], "a protected method is subclass API, not dead")
}

// TestFindDeadCode_JavaOverrideRescued guards the false positive the
// visibility fix would otherwise introduce: a non-public @Override
// implements a supertype contract and is reached through it, so it must
// not be reported even with no direct caller.
func TestFindDeadCode_JavaOverrideRescued(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "Cmp.java", Kind: graph.KindFile, FilePath: "Cmp.java", Language: "java"})

	plainPkg := addJavaMethod(g, "Cmp.java", "unusedPkgHelper", "package")
	overridePkg := addJavaMethod(g, "Cmp.java", "compareTo", "package")
	g.AddEdge(&graph.Edge{From: overridePkg, To: javaOverrideAnnoID, Kind: graph.EdgeAnnotated})

	dead := map[string]bool{}
	for _, d := range FindDeadCode(g, nil, nil) {
		dead[d.ID] = true
	}

	require.True(t, dead[plainPkg], "a plain uncalled package-private method is dead")
	require.False(t, dead[overridePkg], "a package-private @Override is reached via its contract")
}

// TestFindDeadCode_JavaEntryPointRescued confirms a framework-stamped
// entry point (e.g. a package-private @PostConstruct callback) is not
// flagged once visibility makes non-public methods eligible.
func TestFindDeadCode_JavaEntryPointRescued(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "Bean.java", Kind: graph.KindFile, FilePath: "Bean.java", Language: "java"})

	// Stamp it like entrypoints.detectJava would for @PostConstruct.
	callback := "Bean.java::init"
	g.AddNode(&graph.Node{
		ID: callback, Kind: graph.KindMethod, Name: "init",
		FilePath: "Bean.java", Language: "java", StartLine: 1, EndLine: 3,
		Meta: map[string]any{
			"visibility":       "package",
			"entry_point":      true,
			"entry_point_kind": "lifecycle:init",
		},
	})

	dead := map[string]bool{}
	for _, d := range FindDeadCode(g, nil, nil) {
		dead[d.ID] = true
	}
	require.False(t, dead[callback], "a stamped framework callback is a live root")
}
