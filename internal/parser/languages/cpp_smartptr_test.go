package languages

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// cppReturnTypeOf extracts src and returns the return_type meta of the first
// function/method node with the given name.
func cppReturnTypeOf(t *testing.T, src, name string) (string, bool) {
	t.Helper()
	res, err := NewCppExtractor().Extract("t.cpp", []byte(src))
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	for _, n := range res.Nodes {
		if (n.Kind == graph.KindFunction || n.Kind == graph.KindMethod) && n.Name == name {
			rt, ok := n.Meta["return_type"].(string)
			return rt, ok
		}
	}
	t.Fatalf("function/method %q not found", name)
	return "", false
}

// TestCppReturnType_SmartPointerFreeFunction pins the smart-pointer pointee
// unwrap: a factory returning unique_ptr<Widget> records Widget as its return
// type (not unique_ptr), so a chained `make_widget()->draw()` infers Widget as
// the receiver.
func TestCppReturnType_SmartPointerFreeFunction(t *testing.T) {
	src := `#include <memory>
struct Widget { void draw(); };
std::unique_ptr<Widget> make_widget() { return nullptr; }
`
	rt, ok := cppReturnTypeOf(t, src, "make_widget")
	if !ok || rt != "Widget" {
		t.Errorf("make_widget return_type = %q (ok=%v), want Widget", rt, ok)
	}
}

// TestCppReturnType_SmartPointerMethod pins the same unwrap on a class method
// returning shared_ptr<Widget>.
func TestCppReturnType_SmartPointerMethod(t *testing.T) {
	src := `#include <memory>
struct Widget { void draw(); };
class Factory {
public:
	std::shared_ptr<Widget> build() { return nullptr; }
};
`
	rt, ok := cppReturnTypeOf(t, src, "build")
	if !ok || rt != "Widget" {
		t.Errorf("Factory::build return_type = %q (ok=%v), want Widget", rt, ok)
	}
}
