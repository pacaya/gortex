package languages

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// findNode returns the node with the given id, or nil.
func findResultNode(res *parser.ExtractionResult, id string) *graph.Node {
	for _, n := range res.Nodes {
		if n.ID == id {
			return n
		}
	}
	return nil
}

// edgeFromTo returns the edge of the given kind with the exact From and To,
// or nil.
func edgeFromTo(res *parser.ExtractionResult, kind graph.EdgeKind, from, to string) *graph.Edge {
	for _, e := range res.Edges {
		if e.Kind == kind && e.From == from && e.To == to {
			return e
		}
	}
	return nil
}

// barrelFixture models zustand's src/middleware.ts: named function re-exports
// forwarded from sibling modules, one of them aliased.
const barrelFixture = `export { persist } from './middleware/persist';
export { devtools, redux } from './middleware/devtools';
export { ssrSafe as unstable_ssrSafe } from './ssr';
`

func assertBarrelReExportNodes(t *testing.T, res *parser.ExtractionResult, file, lang string) {
	t.Helper()

	// Named re-export mints a queryable node under the standard <file>::<name>
	// scheme so `middleware.ts::persist` resolves.
	persist := findResultNode(res, file+"::persist")
	if persist == nil {
		t.Fatalf("no re-export node minted for persist (%s)", file+"::persist")
	}
	if persist.Kind != graph.KindVariable {
		t.Errorf("persist node kind = %q, want %q", persist.Kind, graph.KindVariable)
	}
	if persist.Name != "persist" {
		t.Errorf("persist node name = %q, want persist", persist.Name)
	}
	if persist.Language != lang {
		t.Errorf("persist node language = %q, want %q", persist.Language, lang)
	}
	if v, _ := persist.Meta["reexport"].(bool); !v {
		t.Errorf("persist node missing reexport marker: %#v", persist.Meta)
	}
	if persist.StartLine != 1 {
		t.Errorf("persist node start line = %d, want 1", persist.StartLine)
	}

	// The node → canonical link (via the unresolved import machinery) and the
	// file → node defines edge both ride the same specifier line.
	if edgeFromTo(res, graph.EdgeReExports, file+"::persist", "unresolved::import::./middleware/persist::persist") == nil {
		t.Error("missing node→canonical re-export edge for persist")
	}
	if edgeFromTo(res, graph.EdgeDefines, file, file+"::persist") == nil {
		t.Error("missing file→node defines edge for persist")
	}

	// A multi-specifier statement mints one node per binding.
	if findResultNode(res, file+"::devtools") == nil {
		t.Error("no re-export node minted for devtools")
	}
	if findResultNode(res, file+"::redux") == nil {
		t.Error("no re-export node minted for redux")
	}

	// Aliased re-export mints under the ALIAS name, not the original.
	alias := findResultNode(res, file+"::unstable_ssrSafe")
	if alias == nil {
		t.Fatalf("no re-export node minted for alias unstable_ssrSafe")
	}
	if alias.Name != "unstable_ssrSafe" {
		t.Errorf("alias node name = %q, want unstable_ssrSafe", alias.Name)
	}
	if orig, _ := alias.Meta["reexport_original"].(string); orig != "ssrSafe" {
		t.Errorf("alias node reexport_original = %q, want ssrSafe", orig)
	}
	if alias.StartLine != 3 {
		t.Errorf("alias node start line = %d, want 3", alias.StartLine)
	}
	if findResultNode(res, file+"::ssrSafe") != nil {
		t.Error("aliased re-export must NOT mint a node under the original name ssrSafe")
	}
	// The aliased node's forward edge still points at the ORIGINAL name and
	// carries the alias.
	fwd := edgeFromTo(res, graph.EdgeReExports, file+"::unstable_ssrSafe", "unresolved::import::./ssr::ssrSafe")
	if fwd == nil {
		t.Fatal("missing node→canonical re-export edge for the alias")
	}
	if fwd.Alias != "unstable_ssrSafe" {
		t.Errorf("alias forward edge alias = %q, want unstable_ssrSafe", fwd.Alias)
	}

	// The existing file-level per-specifier re-export edges are untouched.
	if importEdge(res, graph.EdgeReExports, "unresolved::import::./middleware/persist::persist") == nil {
		t.Error("file-level per-specifier re-export edge for persist was dropped")
	}
}

func TestTSExtractor_BarrelReExportNodes(t *testing.T) {
	res, err := NewTypeScriptExtractor().Extract("middleware.ts", []byte(barrelFixture))
	if err != nil {
		t.Fatal(err)
	}
	assertBarrelReExportNodes(t, res, "middleware.ts", "typescript")
}

func TestJSExtractor_BarrelReExportNodes(t *testing.T) {
	res, err := NewJavaScriptExtractor().Extract("middleware.js", []byte(barrelFixture))
	if err != nil {
		t.Fatal(err)
	}
	assertBarrelReExportNodes(t, res, "middleware.js", "javascript")
}

// TestTSExtractor_TypeOnlyReExportMintsNoValueNode guards the value/type split:
// a type-only re-export names a type, handled as a reference edge elsewhere, so
// it must not mint a value binding node.
func TestTSExtractor_TypeOnlyReExportMintsNoValueNode(t *testing.T) {
	src := `export type { Config } from './types';
export { type Options, run } from './run';
`
	res, err := NewTypeScriptExtractor().Extract("types_barrel.ts", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if findResultNode(res, "types_barrel.ts::Config") != nil {
		t.Error("type-only re-export `export type { Config }` must not mint a value node")
	}
	if findResultNode(res, "types_barrel.ts::Options") != nil {
		t.Error("inline type-only specifier `{ type Options }` must not mint a value node")
	}
	// The value specifier alongside the inline type-only one still mints.
	if findResultNode(res, "types_barrel.ts::run") == nil {
		t.Error("value re-export `run` alongside a type-only specifier should still mint a node")
	}
}

// TestTSExtractor_ReExportVolumeGuardMintsNoNodes verifies a barrel past the
// binding cap collapses to the module edge and mints no per-binding nodes.
func TestTSExtractor_ReExportVolumeGuardMintsNoNodes(t *testing.T) {
	var b []byte
	b = append(b, []byte("export { ")...)
	for i := 0; i < jsImportBindingCap+5; i++ {
		if i > 0 {
			b = append(b, ", "...)
		}
		b = append(b, []byte("n")...)
		b = append(b, []byte{byte('0' + i%10)}...)
		b = append(b, []byte{byte('a' + byte(i/10))}...)
	}
	b = append(b, []byte(" } from './big';\n")...)
	res, err := NewTypeScriptExtractor().Extract("big_barrel.ts", b)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range res.Nodes {
		if v, _ := n.Meta["reexport"].(bool); v {
			t.Fatalf("re-export node %q minted past the volume guard", n.ID)
		}
	}
	if importEdge(res, graph.EdgeReExports, "unresolved::import::./big") == nil {
		t.Error("module-level re-export edge dropped under the volume guard")
	}
}
