package languages

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// tsuEdgesTo returns every edge whose target is unresolved::<name>, regardless
// of kind — find_usages counts EdgeReferences and EdgeTypedAs alike as usages.
func tsuEdgesTo(edges []*graph.Edge, name string) []*graph.Edge {
	var out []*graph.Edge
	for _, e := range edges {
		if e.To == "unresolved::"+name {
			out = append(out, e)
		}
	}
	return out
}

func tsuHasEdgeTo(edges []*graph.Edge, name string) bool { return len(tsuEdgesTo(edges, name)) > 0 }

// TestTSTypeUseExtra_GenericConstraint pins `<T extends ExcalidrawElement>` →
// ExcalidrawElement on a class, interface, function, and type alias — a type
// named only as a generic bound must be reachable without a language server.
func TestTSTypeUseExtra_GenericConstraint(t *testing.T) {
	cases := map[string]string{
		"class":     `class Store<T extends ExcalidrawElement> { items: T[] = []; }`,
		"interface": `interface Box<T extends ExcalidrawElement> { value: T; }`,
		"function":  `function pick<T extends ExcalidrawElement>(x: T): T { return x; }`,
		"alias":     `type Wrap<T extends ExcalidrawElement> = { v: T };`,
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			_, edges := runTSExtract(t, "src/store.ts", src)
			if !tsuHasEdgeTo(edges, "ExcalidrawElement") {
				t.Errorf("%s constraint `T extends ExcalidrawElement` should reference ExcalidrawElement", name)
			}
		})
	}
}

// TestTSTypeUseExtra_StructuralAliasBody pins the structural-alias recall fix: a
// type used only deep inside an object-type literal or function type must still
// be reachable — the text decomposer collapsed `{ … }` to one token and dropped
// its members.
func TestTSTypeUseExtra_StructuralAliasBody(t *testing.T) {
	src := `type SceneData = {
	elements: readonly ExcalidrawElement[];
	appState: AppState;
	onChange: (el: ExcalidrawElement, next: AppState) => void;
};
`
	_, edges := runTSExtract(t, "src/scene.ts", src)
	for _, want := range []string{"ExcalidrawElement", "AppState"} {
		if !tsuHasEdgeTo(edges, want) {
			t.Errorf("structural alias body should reference %q", want)
		}
	}
}

// TestTSTypeUseExtra_InterfaceMethodParam pins method-signature parameter types:
// an interface method `mutate(el: ExcalidrawElement): AppState` references both
// the parameter type and the return type.
func TestTSTypeUseExtra_InterfaceMethodParam(t *testing.T) {
	src := `interface Api {
	mutate(el: ExcalidrawElement): AppState;
}
`
	_, edges := runTSExtract(t, "src/api.ts", src)
	if !tsuHasEdgeTo(edges, "ExcalidrawElement") {
		t.Error("interface method parameter type ExcalidrawElement should be referenced")
	}
	if !tsuHasEdgeTo(edges, "AppState") {
		t.Error("interface method return type AppState should be referenced")
	}
}

// TestTSTypeUseExtra_TypeofInInterfaceMember pins `typeof X` inside an interface
// property type (`InstanceType<typeof App>["resetScene"]`) — the queried value
// is a plain identifier the type_identifier-only walk skipped. This is the
// React-component-via-imperative-API form.
func TestTSTypeUseExtra_TypeofInInterfaceMember(t *testing.T) {
	src := `interface ImperativeAPI {
	updateScene: InstanceType<typeof App>["updateScene"];
	resetScene: InstanceType<typeof App>["resetScene"];
}
`
	_, edges := runTSExtract(t, "src/api.ts", src)
	if !tsuHasEdgeTo(edges, "App") {
		t.Error("typeof App inside interface property type should reference App")
	}
}

// TestTSTypeUseExtra_PerMemberDedup pins that each interface member naming the
// same type is counted as a distinct usage site — an interface whose three
// properties are each typed AppState references AppState three times, not once.
func TestTSTypeUseExtra_PerMemberDedup(t *testing.T) {
	src := `interface State {
	current: AppState;
	previous: AppState;
	pending: AppState;
}
`
	_, edges := runTSExtract(t, "src/state.ts", src)
	if n := len(tsuEdgesTo(edges, "AppState")); n < 3 {
		t.Errorf("three members each typed AppState should yield >=3 references, got %d", n)
	}
}

// TestTSTypeUseExtra_DefaultTypeImport pins the default type-only import form
// `import type App from "mod"` (App is the module's default export); a plain
// value default import is not a type reference.
func TestTSTypeUseExtra_DefaultTypeImport(t *testing.T) {
	src := `import type App from "./components/App";
import DefaultValue from "./runtime";
`
	_, edges := runTSExtract(t, "src/x.ts", src)
	if !tsuHasEdgeTo(edges, "App") {
		t.Error("default type-only import `import type App` should reference App")
	}
	if tsuHasEdgeTo(edges, "DefaultValue") {
		t.Error("a value default import must not emit a type reference")
	}
}
