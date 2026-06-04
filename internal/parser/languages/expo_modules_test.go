package languages

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func expoMethodMeta(nodes []*graph.Node) map[string][2]any {
	out := map[string][2]any{}
	for _, n := range nodes {
		if n.Kind != graph.KindMethod || n.Meta == nil {
			continue
		}
		mod, _ := n.Meta["expo_module"].(string)
		if mod == "" {
			continue
		}
		async, _ := n.Meta["expo_async"].(bool)
		out[n.Name] = [2]any{mod, async}
	}
	return out
}

func TestSwiftExtract_ExpoModule(t *testing.T) {
	src := `import ExpoModulesCore

public class MathModule: Module {
  public func definition() -> ModuleDefinition {
    Name("Math")
    Function("add") { (a: Int, b: Int) -> Int in
      return a + b
    }
    AsyncFunction("fetchData") { (url: String) in
    }
  }
}
`
	r, err := NewSwiftExtractor().Extract("MathModule.swift", []byte(src))
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	meta := expoMethodMeta(r.Nodes)
	if got, ok := meta["add"]; !ok || got[0] != "Math" || got[1] != false {
		t.Errorf("add = %v (ok=%v), want module Math, async false", meta["add"], ok)
	}
	if got, ok := meta["fetchData"]; !ok || got[0] != "Math" || got[1] != true {
		t.Errorf("fetchData = %v (ok=%v), want module Math, async true", meta["fetchData"], ok)
	}
}

func TestKotlinExtract_ExpoModule(t *testing.T) {
	src := `package expo.modules.math

import expo.modules.kotlin.modules.Module
import expo.modules.kotlin.modules.ModuleDefinition

class MathModule : Module() {
  override fun definition() = ModuleDefinition {
    Name("Math")
    Function("add") { a: Int, b: Int -> a + b }
    AsyncFunction("fetchData") { url: String -> }
  }
}
`
	r, err := NewKotlinExtractor().Extract("MathModule.kt", []byte(src))
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	meta := expoMethodMeta(r.Nodes)
	if got, ok := meta["add"]; !ok || got[0] != "Math" {
		t.Errorf("add = %v (ok=%v), want module Math", meta["add"], ok)
	}
	if _, ok := meta["fetchData"]; !ok {
		t.Errorf("fetchData not extracted")
	}
}

func TestExtractExpoModules_NonExpoFileIgnored(t *testing.T) {
	// A Swift file that happens to call Name("x")/Function("y") but is not
	// an Expo module (no ModuleDefinition) must yield nothing.
	if got := extractExpoModules([]byte(`func f() { Name("x"); Function("y") }`)); got != nil {
		t.Errorf("non-Expo file produced %v, want nil", got)
	}
}
