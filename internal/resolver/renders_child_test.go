package resolver

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// TestRendersChildImportBindsViaImport proves an EdgeRendersChild (`<Button/>`)
// binds to the EXACT component the caller file imported under that name —
// disambiguating two same-named components in different directories by
// ground-truth import binding, where a directory-proximity guess would pick
// the wrong (nearer) one. The bound edge is stamped resolution=import_binding
// at OriginASTResolved.
func TestRendersChildImportBindsViaImport(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "src/app/App.tsx", Kind: graph.KindFile, Name: "App.tsx", FilePath: "src/app/App.tsx", Language: "typescript"})
	g.AddNode(&graph.Node{ID: "src/app/App.tsx::App", Kind: graph.KindFunction, Name: "App", FilePath: "src/app/App.tsx"})
	// The imported Button lives far from the caller; a same-named decoy sits in
	// a sibling dir right next to the caller (where dir-proximity would land).
	g.AddNode(&graph.Node{ID: "src/far/Button.tsx::Button", Kind: graph.KindFunction, Name: "Button", FilePath: "src/far/Button.tsx"})
	g.AddNode(&graph.Node{ID: "src/app/components/Button.tsx::Button", Kind: graph.KindFunction, Name: "Button", FilePath: "src/app/components/Button.tsx"})

	g.AddEdge(&graph.Edge{From: "src/app/App.tsx", To: "unresolved::import::../far/Button", Kind: graph.EdgeImports, FilePath: "src/app/App.tsx"})
	rc := &graph.Edge{From: "src/app/App.tsx::App", To: "unresolved::Button", Kind: graph.EdgeRendersChild, FilePath: "src/app/App.tsx"}
	g.AddEdge(rc)

	r := New(g)
	if _, changed := r.resolveEdge(rc, &ResolveStats{}); !changed {
		t.Fatal("renders-child edge was not resolved")
	}
	if rc.To != "src/far/Button.tsx::Button" {
		t.Errorf("bound to %q, want the IMPORTED src/far/Button.tsx::Button (not the near decoy)", rc.To)
	}
	if rc.Meta["resolution"] != "import_binding" {
		t.Errorf("resolution=%v, want import_binding", rc.Meta["resolution"])
	}
	if rc.Origin != graph.OriginASTResolved {
		t.Errorf("origin=%v, want OriginASTResolved", rc.Origin)
	}
}

// TestRendersChildImportNamedAndAliased covers the named (`import { Button }`)
// and aliased (`import { Button as Btn }`) binding forms.
func TestRendersChildImportNamedAndAliased(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "src/ui/Card.tsx", Kind: graph.KindFile, Name: "Card.tsx", FilePath: "src/ui/Card.tsx", Language: "typescript"})
	g.AddNode(&graph.Node{ID: "src/ui/Card.tsx::Card", Kind: graph.KindFunction, Name: "Card", FilePath: "src/ui/Card.tsx"})
	g.AddNode(&graph.Node{ID: "src/widgets/Avatar.tsx::Avatar", Kind: graph.KindFunction, Name: "Avatar", FilePath: "src/widgets/Avatar.tsx"})

	// Named import: import { Avatar } from '../widgets/Avatar'
	g.AddEdge(&graph.Edge{From: "src/ui/Card.tsx", To: "unresolved::import::../widgets/Avatar::Avatar", Kind: graph.EdgeImports, FilePath: "src/ui/Card.tsx"})
	// Aliased import of the same module: import { Avatar as Ava }
	g.AddEdge(&graph.Edge{From: "src/ui/Card.tsx", To: "unresolved::import::../widgets/Avatar::Avatar", Kind: graph.EdgeImports, FilePath: "src/ui/Card.tsx", Alias: "Ava"})

	r := New(g)

	named := &graph.Edge{From: "src/ui/Card.tsx::Card", To: "unresolved::Avatar", Kind: graph.EdgeRendersChild, FilePath: "src/ui/Card.tsx"}
	g.AddEdge(named)
	r.resolveEdge(named, &ResolveStats{})
	if named.To != "src/widgets/Avatar.tsx::Avatar" {
		t.Errorf("named import: bound to %q, want src/widgets/Avatar.tsx::Avatar", named.To)
	}

	aliased := &graph.Edge{From: "src/ui/Card.tsx::Card", To: "unresolved::Ava", Kind: graph.EdgeRendersChild, FilePath: "src/ui/Card.tsx"}
	g.AddEdge(aliased)
	r.resolveEdge(aliased, &ResolveStats{})
	if aliased.To != "src/widgets/Avatar.tsx::Avatar" {
		t.Errorf("aliased import (<Ava/>): bound to %q, want src/widgets/Avatar.tsx::Avatar", aliased.To)
	}
}
