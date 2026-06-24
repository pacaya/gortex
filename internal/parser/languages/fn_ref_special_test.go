package languages

import (
	"testing"

	"github.com/zzet/gortex/internal/parser"
)

func specialCand(t *testing.T, res *parser.ExtractionResult, name string) map[string]any {
	t.Helper()
	cands := fnValueCands(res)
	meta, ok := cands[name]
	if !ok {
		t.Fatalf("special-form value %q not captured (got: %v)", name, keys(cands))
	}
	if meta["fn_ref_form"] != "special" {
		t.Errorf("fn_ref_form = %v, want special", meta["fn_ref_form"])
	}
	return meta
}

// TestFnRefSpecial_JavaMethodReference pins `Foo::bar` capture with the type as
// receiver hint.
func TestFnRefSpecial_JavaMethodReference(t *testing.T) {
	src := []byte(`class Demo {
    static String valueOf(int x) { return ""; }
    void use() {
        java.util.function.Function<Integer, String> f = Demo::valueOf;
    }
}
`)
	res, err := NewJavaExtractor().Extract("Demo.java", src)
	if err != nil {
		t.Fatal(err)
	}
	meta := specialCand(t, res, "valueOf")
	if meta["fn_ref_recv_hint"] != "Demo" {
		t.Errorf("recv_hint = %v, want Demo", meta["fn_ref_recv_hint"])
	}
}

// TestFnRefSpecial_KotlinCallableReference pins bare `::handler` capture scoped
// to the enclosing scope.
func TestFnRefSpecial_KotlinCallableReference(t *testing.T) {
	src := []byte(`fun handler() {}

fun use() {
    val f = ::handler
}
`)
	res, err := NewKotlinExtractor().Extract("a.kt", src)
	if err != nil {
		t.Fatal(err)
	}
	meta := specialCand(t, res, "handler")
	if meta["fn_ref_recv_hint"] != "<self>" {
		t.Errorf("recv_hint = %v, want <self>", meta["fn_ref_recv_hint"])
	}
}

// TestFnRefSpecial_PythonSelfMethod pins `self.handle` capture.
func TestFnRefSpecial_PythonSelfMethod(t *testing.T) {
	src := []byte(`class C:
    def handle(self):
        pass

    def wire(self):
        register(self.handle)
`)
	res, err := NewPythonExtractor().Extract("c.py", src)
	if err != nil {
		t.Fatal(err)
	}
	meta := specialCand(t, res, "handle")
	if meta["fn_ref_recv_hint"] != "<self>" {
		t.Errorf("recv_hint = %v, want <self>", meta["fn_ref_recv_hint"])
	}
}

// TestFnRefSpecial_JSThisMethod pins `this.handle` capture inside a class.
func TestFnRefSpecial_JSThisMethod(t *testing.T) {
	src := []byte(`class C {
  handle() {}
  wire() {
    arr.forEach(this.handle);
  }
}
`)
	res, err := NewJavaScriptExtractor().Extract("c.js", src)
	if err != nil {
		t.Fatal(err)
	}
	meta := specialCand(t, res, "handle")
	if meta["fn_ref_recv_hint"] != "<self>" {
		t.Errorf("recv_hint = %v, want <self>", meta["fn_ref_recv_hint"])
	}
}

// TestFnRefSpecial_RubyMethodSymbol pins `method(:handle)` capture.
func TestFnRefSpecial_RubyMethodSymbol(t *testing.T) {
	src := []byte(`class C
  def handle
  end

  def wire
    register(method(:handle))
  end
end
`)
	res, err := NewRubyExtractor().Extract("c.rb", src)
	if err != nil {
		t.Fatal(err)
	}
	specialCand(t, res, "handle")
}

// TestFnRefSpecial_RubySymbolProc pins Ruby `&:handle` symbol-to-proc capture —
// the symbol names the method invoked on each element, resolved repo-wide so the
// receiver hint is empty.
func TestFnRefSpecial_RubySymbolProc(t *testing.T) {
	src := []byte(`class C
  def handle
  end

  def wire(items)
    items.map(&:handle)
  end
end
`)
	res, err := NewRubyExtractor().Extract("c.rb", src)
	if err != nil {
		t.Fatal(err)
	}
	meta := specialCand(t, res, "handle")
	// A repo-wide symbol-proc carries no receiver hint, so the key is absent
	// (distinct from a "<self>"-scoped form).
	if rh, ok := meta["fn_ref_recv_hint"]; ok {
		t.Errorf("recv_hint = %v, want no hint (repo-wide symbol-proc)", rh)
	}
}

// TestFnRefSpecial_CSharpThisMethod pins C# `this.Handle` member access scoped
// to the enclosing type.
func TestFnRefSpecial_CSharpThisMethod(t *testing.T) {
	src := []byte(`class C {
    void Handle() {}
    void Wire() {
        Register(this.Handle);
    }
}
`)
	res, err := NewCSharpExtractor().Extract("C.cs", src)
	if err != nil {
		t.Fatal(err)
	}
	meta := specialCand(t, res, "Handle")
	if meta["fn_ref_recv_hint"] != "<self>" {
		t.Errorf("recv_hint = %v, want <self>", meta["fn_ref_recv_hint"])
	}
}

// TestFnRefSpecial_SwiftSelector pins Swift `#selector(handle)` capture scoped to
// the enclosing type.
func TestFnRefSpecial_SwiftSelector(t *testing.T) {
	src := []byte(`class C {
    func handle() {}
    func wire() {
        let s = #selector(handle)
    }
}
`)
	res, err := NewSwiftExtractor().Extract("C.swift", src)
	if err != nil {
		t.Fatal(err)
	}
	meta := specialCand(t, res, "handle")
	if meta["fn_ref_recv_hint"] != "<self>" {
		t.Errorf("recv_hint = %v, want <self>", meta["fn_ref_recv_hint"])
	}
}
