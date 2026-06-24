package languages

import (
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// fnValueCands returns the captured function-as-value placeholder edges, keyed
// by captured name → its Meta.
func fnValueCands(res *parser.ExtractionResult) map[string]map[string]any {
	out := map[string]map[string]any{}
	for _, e := range res.Edges {
		if e.Kind != graph.EdgeReferences || e.Meta == nil {
			continue
		}
		if v, _ := e.Meta["via"].(string); v != "callback_candidate" {
			continue
		}
		name, _ := e.Meta["fn_value_name"].(string)
		out[name] = e.Meta
	}
	return out
}

func TestFnRefSpecFor(t *testing.T) {
	if !fnRefSpecFor("rust").ungated {
		t.Error("rust spec should be ungated (qualified paths)")
	}
	if !fnRefSpecFor("go").matchesIDNode("selector_expression") {
		t.Error("go spec should match selector_expression")
	}
	if !fnRefSpecFor("swift").matchesIDNode("simple_identifier") {
		t.Error("swift spec should match simple_identifier")
	}
	if !fnRefSpecFor("python").matchesIDNode("identifier") || fnRefSpecFor("python").ungated {
		t.Error("default spec is bare-identifier, gated")
	}
}

// TestFnValueCapture_GoSelectorMethodValue pins capture of a Go method passed by
// value through a selector (`t.Handle`), which the bare-identifier walk misses
// (the field is a field_identifier, not an identifier).
func TestFnValueCapture_GoSelectorMethodValue(t *testing.T) {
	src := []byte(`package main

type T struct{}

func (t T) Handle() {}

func register(t T) {
	cb := t.Handle
	_ = cb
}
`)
	res, err := NewGoExtractor().Extract("main.go", src)
	if err != nil {
		t.Fatal(err)
	}
	cands := fnValueCands(res)
	meta, ok := cands["Handle"]
	if !ok {
		t.Fatalf("Go selector method-value not captured (got: %v)", keys(cands))
	}
	if meta["fn_ref_lang"] != "go" {
		t.Errorf("fn_ref_lang = %v, want go", meta["fn_ref_lang"])
	}
	// The receiver var `t` and the called declaration must not be captured.
	if _, bad := cands["t"]; bad {
		t.Error("receiver var t should not be captured")
	}
}

// TestFnValueCapture_RustScopedCrossModule pins capture of a qualified Rust path
// value (`other::process`) as an ungated cross-module candidate.
func TestFnValueCapture_RustScopedCrossModule(t *testing.T) {
	src := []byte(`fn run() {
    let cb = other::process;
    let _ = cb;
}
`)
	res, err := NewRustExtractor().Extract("main.rs", src)
	if err != nil {
		t.Fatal(err)
	}
	cands := fnValueCands(res)
	meta, ok := cands["process"]
	if !ok {
		t.Fatalf("Rust scoped path value not captured (got: %v)", keys(cands))
	}
	if meta["fn_value_ungated"] != true {
		t.Errorf("scoped cross-module path should be ungated (got: %v)", meta)
	}
}

// TestFnValueCapture_BareIdentifierStillWorks pins the pre-existing bare-name
// capture (a same-file function passed by value) is unchanged, and a local of
// the same shape is not captured.
func TestFnValueCapture_BareIdentifierStillWorks(t *testing.T) {
	src := []byte(`package main

func handler() {}

func register() {
	cb := handler
	other := cb
	_ = other
}
`)
	res, err := NewGoExtractor().Extract("main.go", src)
	if err != nil {
		t.Fatal(err)
	}
	cands := fnValueCands(res)
	if _, ok := cands["handler"]; !ok {
		t.Fatalf("bare same-file function value not captured (got: %v)", keys(cands))
	}
	if _, bad := cands["cb"]; bad {
		t.Error("local variable cb should not be captured")
	}
}

func keys(m map[string]map[string]any) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return []string{strings.Join(out, ",")}
}
