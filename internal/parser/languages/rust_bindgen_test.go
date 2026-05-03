package languages

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestRustWasmBindgenStampsFileMeta(t *testing.T) {
	src := `use wasm_bindgen::prelude::*;

#[wasm_bindgen]
pub fn greet(name: &str) -> String {
    format!("Hello, {}", name)
}
`
	ext := NewRustExtractor()
	res, err := ext.Extract("pkg/lib.rs", []byte(src))
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	var fileNode *graph.Node
	for _, n := range res.Nodes {
		if n.Kind == graph.KindFile {
			fileNode = n
			break
		}
	}
	if fileNode == nil {
		t.Fatal("no file node emitted")
	}
	if v, _ := fileNode.Meta["uses_wasm_bindgen"].(bool); !v {
		t.Errorf("expected uses_wasm_bindgen=true on file node, meta=%+v", fileNode.Meta)
	}
}

func TestRustWasmBindgenAbsentOnRegularFile(t *testing.T) {
	src := `pub fn add(a: i32, b: i32) -> i32 {
    a + b
}
`
	ext := NewRustExtractor()
	res, err := ext.Extract("pkg/lib.rs", []byte(src))
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	for _, n := range res.Nodes {
		if n.Kind == graph.KindFile {
			if v, _ := n.Meta["uses_wasm_bindgen"].(bool); v {
				t.Errorf("regular Rust file should not have uses_wasm_bindgen set")
			}
			return
		}
	}
}
