package languages

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// TestXsjsAndExtensionMapping proves the extension cluster routes the
// previously-UNKNOWN server-side-JS / ESM-TS suffixes to the right extractor
// and that an IIFE/AMD-wrapped inner function is still mined — recall the
// bare-extension table missed. The mapping is asserted through the registry
// (the real routing path), not by calling the extractor directly.
func TestXsjsAndExtensionMapping(t *testing.T) {
	reg := parser.NewRegistry()
	RegisterAll(reg)

	wantLang := map[string]string{
		".xsjs":    "javascript",
		".xsjslib": "javascript",
		".mts":     "typescript",
		".cts":     "typescript",
	}
	for ext, lang := range wantLang {
		ex, ok := reg.GetByExtension(ext)
		if !ok {
			t.Errorf("extension %q routes to no extractor", ext)
			continue
		}
		if ex.Language() != lang {
			t.Errorf("extension %q routes to %q, want %q", ext, ex.Language(), lang)
		}
	}

	// IIFE / AMD module wrapper: the inner function must still be extracted —
	// the tree-sitter query matches function_declaration at any depth.
	const iife = `(function () {
  function handleGet(req) { return req.body; }
})();
`
	res, err := NewJavaScriptExtractor().Extract("service.xsjs", []byte(iife))
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, n := range res.Nodes {
		if n.Kind == graph.KindFunction && n.Name == "handleGet" {
			found = true
		}
	}
	if !found {
		t.Error("IIFE inner function handleGet not extracted from .xsjs source")
	}
}

// TestGeneratedDartSkip proves generated Dart boilerplate is excluded from the
// symbol surface (measured: same source yields symbols as a hand-written
// .dart, zero as a .g.dart/.freezed.dart/.pb.dart), while the file node is kept
// and flagged so incremental tracking still sees the file.
func TestGeneratedDartSkip(t *testing.T) {
	const src = `class User {
  String name;
  void greet() {}
}
`
	symbolCount := func(filePath string) (symbols int, generated bool) {
		res, err := NewDartExtractor().Extract(filePath, []byte(src))
		if err != nil {
			t.Fatalf("Extract(%s): %v", filePath, err)
		}
		for _, n := range res.Nodes {
			switch n.Kind {
			case graph.KindType, graph.KindMethod, graph.KindFunction:
				symbols++
			case graph.KindFile:
				if g, _ := n.Meta["generated"].(bool); g {
					generated = true
				}
			}
		}
		return symbols, generated
	}

	// Hand-written source: real symbols, not flagged generated.
	if n, gen := symbolCount("user.dart"); n == 0 || gen {
		t.Errorf("user.dart: symbols=%d generated=%v, want symbols>0 generated=false", n, gen)
	}
	// Generated suffixes: no symbols, file flagged generated.
	for _, fp := range []string{"user.g.dart", "user.freezed.dart", "user.pb.dart"} {
		n, gen := symbolCount(fp)
		if n != 0 {
			t.Errorf("%s: extracted %d symbols, want 0 (generated file should be skipped)", fp, n)
		}
		if !gen {
			t.Errorf("%s: file node not flagged generated", fp)
		}
	}
}

// TestCConstClassification proves a file-scope `const` is classified as a named
// constant (joining the value-ref impact surface) while a mutable global stays
// a variable — and a pointer-to-const never spuriously becomes a constant.
func TestCConstClassification(t *testing.T) {
	const src = `const int MAX_RETRIES = 5;
int g_counter = 0;
const char *g_name = "x";
`
	res, err := NewCExtractor().Extract("k.c", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	kindOf := map[string]graph.NodeKind{}
	for _, n := range res.Nodes {
		if n.Kind == graph.KindConstant || n.Kind == graph.KindVariable {
			kindOf[n.Name] = n.Kind
		}
	}
	if kindOf["MAX_RETRIES"] != graph.KindConstant {
		t.Errorf("MAX_RETRIES kind=%q, want constant", kindOf["MAX_RETRIES"])
	}
	if kindOf["g_counter"] != graph.KindVariable {
		t.Errorf("g_counter kind=%q, want variable", kindOf["g_counter"])
	}
	// `const char *p` is a pointer-to-const (mutable pointer) — it must not be
	// misclassified as a named constant.
	if kindOf["g_name"] == graph.KindConstant {
		t.Error("pointer-to-const g_name wrongly classified as a constant")
	}
}

// TestAnonClassNodes confirms Java and C# anonymous classes are first-class
// graph nodes (a navigable symbol carrying the anonymous marker), not dropped
// — the recall the audit flagged for verification.
func TestAnonClassNodes(t *testing.T) {
	java := `class A {
  void m() {
    Runnable r = new Runnable() { public void run() {} };
  }
}
`
	jres, err := NewJavaExtractor().Extract("A.java", []byte(java))
	if err != nil {
		t.Fatal(err)
	}
	if !hasAnonymousNode(jres.Nodes) {
		t.Error("Java anonymous class produced no anonymous node")
	}

	csharp := `class A {
  void M() {
    var x = new { Name = "a", Age = 1 };
  }
}
`
	cres, err := NewCSharpExtractor().Extract("A.cs", []byte(csharp))
	if err != nil {
		t.Fatal(err)
	}
	if !hasAnonymousNode(cres.Nodes) {
		t.Error("C# anonymous type produced no anonymous node")
	}
}

func hasAnonymousNode(nodes []*graph.Node) bool {
	for _, n := range nodes {
		if a, _ := n.Meta["anonymous"].(bool); a {
			return true
		}
	}
	return false
}
