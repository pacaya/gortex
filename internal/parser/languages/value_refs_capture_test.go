package languages

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// constNode returns the node of the given name emitted as a KindConstant, or nil.
func constNode(res *parser.ExtractionResult, name string) *graph.Node {
	for _, n := range res.Nodes {
		if n.Name == name && n.Kind == graph.KindConstant {
			return n
		}
	}
	return nil
}

// valueRefRead reports whether a value-ref candidate read of name was emitted.
func valueRefRead(res *parser.ExtractionResult, name string) bool {
	for _, e := range res.Edges {
		if e.Kind != graph.EdgeReads || e.Meta == nil {
			continue
		}
		if v, _ := e.Meta["via"].(string); v != "value_ref_candidate" {
			continue
		}
		if n, _ := e.Meta["name"].(string); n == name {
			return true
		}
	}
	return false
}

// TestValueRefKind_ScalaVal pins that a distinctive immutable val is kinded as a
// constant and its read is captured.
func TestValueRefKind_ScalaVal(t *testing.T) {
	src := []byte(`object Config {
  val MAX_SIZE = 100
  def use(): Int = MAX_SIZE
}
`)
	res, err := NewScalaExtractor().Extract("Config.scala", src)
	if err != nil {
		t.Fatal(err)
	}
	if constNode(res, "MAX_SIZE") == nil {
		t.Error("Scala distinctive val MAX_SIZE should be KindConstant")
	}
	if !valueRefRead(res, "MAX_SIZE") {
		t.Error("read of MAX_SIZE should be captured as a value-ref candidate")
	}
}

// TestValueRefKind_RubyConst pins that a Ruby constant is kinded as a constant
// and its read is captured.
func TestValueRefKind_RubyConst(t *testing.T) {
	src := []byte(`MAX_SIZE = 100

def use
  MAX_SIZE
end
`)
	res, err := NewRubyExtractor().Extract("config.rb", src)
	if err != nil {
		t.Fatal(err)
	}
	if constNode(res, "MAX_SIZE") == nil {
		t.Error("Ruby constant MAX_SIZE should be KindConstant")
	}
	if !valueRefRead(res, "MAX_SIZE") {
		t.Error("read of MAX_SIZE should be captured as a value-ref candidate")
	}
}

// TestValueRefKind_JavaStaticFinal pins the existing Java static-final → constant
// kinding remains intact and its read is captured.
func TestValueRefKind_JavaStaticFinal(t *testing.T) {
	src := []byte(`class Demo {
    public static final int MAX_SIZE = 5;
    int use() { return MAX_SIZE; }
}
`)
	res, err := NewJavaExtractor().Extract("Demo.java", src)
	if err != nil {
		t.Fatal(err)
	}
	if constNode(res, "MAX_SIZE") == nil {
		t.Error("Java static final MAX_SIZE should be KindConstant")
	}
	if !valueRefRead(res, "MAX_SIZE") {
		t.Error("read of MAX_SIZE should be captured as a value-ref candidate")
	}
}

// TestValueRefKind_CppNoCandidate pins the deliberate C++ revert: no value-ref
// candidate is emitted for a C++ constant read.
func TestValueRefKind_CppNoCandidate(t *testing.T) {
	src := []byte(`const int MAX_SIZE = 5;
int use() { return MAX_SIZE; }
`)
	res, err := NewCppExtractor().Extract("demo.cpp", src)
	if err != nil {
		t.Fatal(err)
	}
	if valueRefRead(res, "MAX_SIZE") {
		t.Error("C++ must not emit value-ref candidates (revert parity)")
	}
}
