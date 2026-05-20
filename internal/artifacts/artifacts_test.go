package artifacts

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
)

func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	abs := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func symbolGraph() *graph.Graph {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "models.go::User", Kind: graph.KindType, Name: "User", FilePath: "models.go"})
	g.AddNode(&graph.Node{ID: "orders.go::CreateOrder", Kind: graph.KindFunction, Name: "CreateOrder", FilePath: "orders.go"})
	g.AddNode(&graph.Node{ID: "x.go::Go", Kind: graph.KindFunction, Name: "Go", FilePath: "x.go"}) // too short to index
	return g
}

func TestMaterialize_CreatesNodesAndReferences(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "db/schema.sql", "CREATE TABLE User (id INT);\n-- linked to CreateOrder handler\n")
	writeFile(t, root, "docs/adr/0001-choice.md", "# ADR 0001\nSome decision text.\n")

	g := symbolGraph()
	entries := []config.ArtifactEntry{
		{Path: "db/schema.sql", Kind: "schema"},
		{Path: "docs/adr/*.md"},
	}
	arts := Materialize(g, root, entries, "")
	if len(arts) != 2 {
		t.Fatalf("expected 2 artifacts, got %d: %+v", len(arts), arts)
	}

	var schema *Artifact
	for i := range arts {
		if arts[i].Path == "db/schema.sql" {
			schema = &arts[i]
		}
	}
	if schema == nil {
		t.Fatal("db/schema.sql artifact missing")
	}
	if schema.Kind != "schema" {
		t.Errorf("kind = %q, want schema", schema.Kind)
	}
	if schema.ContentHash == "" {
		t.Error("content hash not computed")
	}
	// The schema text names User and CreateOrder.
	if len(schema.References) != 2 {
		t.Errorf("expected 2 references, got %v", schema.References)
	}

	// The artifact node and its EdgeReferences edges are in the graph.
	node := g.GetNode("artifact::db/schema.sql")
	if node == nil || node.Kind != graph.KindArtifact {
		t.Fatalf("artifact node missing or wrong kind: %+v", node)
	}
	refEdges := 0
	for _, e := range g.GetOutEdges(node.ID) {
		if e.Kind == graph.EdgeReferences {
			refEdges++
		}
	}
	if refEdges != 2 {
		t.Errorf("expected 2 EdgeReferences out of the artifact, got %d", refEdges)
	}

	// The .md artifact auto-detects kind "doc".
	for _, a := range arts {
		if a.Path == "docs/adr/0001-choice.md" && a.Kind != "doc" {
			t.Errorf("adr kind = %q, want doc", a.Kind)
		}
	}
}

func TestMaterialize_RecursiveGlob(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "docs/a/one.md", "alpha")
	writeFile(t, root, "docs/a/b/two.md", "beta")
	writeFile(t, root, "docs/note.txt", "skip me")

	g := graph.New()
	arts := Materialize(g, root, []config.ArtifactEntry{{Path: "docs/**/*.md"}}, "")
	if len(arts) != 2 {
		t.Fatalf("recursive glob should match 2 .md files, got %d: %+v", len(arts), arts)
	}
}

func TestMaterialize_RepoPrefix(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "schema.sql", "CREATE TABLE t (id int);")
	g := graph.New()
	arts := Materialize(g, root, []config.ArtifactEntry{{Path: "schema.sql"}}, "myrepo")
	if len(arts) != 1 {
		t.Fatalf("expected 1 artifact, got %d", len(arts))
	}
	if arts[0].ID != "artifact::myrepo/schema.sql" {
		t.Errorf("prefixed ID = %q", arts[0].ID)
	}
	if g.GetNode("artifact::myrepo/schema.sql") == nil {
		t.Error("prefixed artifact node missing from graph")
	}
}

func TestDetectKind(t *testing.T) {
	cases := map[string]string{
		"db/schema.sql":          "schema",
		"prisma/schema.prisma":   "schema",
		"api/service.proto":      "api",
		"api/openapi-v2.yaml":    "api",
		"infra/main.tf":          "infra",
		"k8s/kustomization.yaml": "infra",
		"docs/adr/0001-thing.md": "doc",
		"notes.txt":              "doc",
	}
	for path, want := range cases {
		if got := detectKind(path); got != want {
			t.Errorf("detectKind(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestMaterialize_EmptyManifest(t *testing.T) {
	if arts := Materialize(graph.New(), t.TempDir(), nil, ""); arts != nil {
		t.Errorf("empty manifest should yield nil, got %+v", arts)
	}
}
