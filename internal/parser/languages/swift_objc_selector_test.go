package languages

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestBuildSwiftObjCSelector(t *testing.T) {
	cases := []struct {
		base   string
		labels []string
		want   string
	}{
		{"viewDidLoad", nil, "viewDidLoad"},
		{"move", []string{"from", "to"}, "moveFrom:to:"},
		{"insertSubview", []string{"", "at"}, "insertSubview:at:"},
		{"init", []string{"frame"}, "initWithFrame:"},
		{"doThing", []string{"with"}, "doThingWith:"},
		{"reset", []string{""}, "reset:"},
	}
	for _, c := range cases {
		if got := buildSwiftObjCSelector(c.base, c.labels); got != c.want {
			t.Errorf("buildSwiftObjCSelector(%q, %v) = %q, want %q", c.base, c.labels, got, c.want)
		}
	}
}

func TestSwiftArgLabels(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"x: Int", []string{"x"}},
		{"from a: Int, to b: Int", []string{"from", "to"}},
		{"_ view: UIView, at index: Int", []string{"", "at"}},
		{"closure: (Int, String) -> Void, name: String", []string{"closure", "name"}},
		{"items: [String], opts: [String: Int]", []string{"items", "opts"}},
	}
	for _, c := range cases {
		got := swiftArgLabels(c.in)
		if len(got) != len(c.want) {
			t.Errorf("swiftArgLabels(%q) = %v, want %v", c.in, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("swiftArgLabels(%q)[%d] = %q, want %q", c.in, i, got[i], c.want[i])
			}
		}
	}
}

// TestSwiftExtract_StampsObjCSelector drives the full extractor and
// asserts each @objc method node carries the computed ObjC selector,
// while a non-@objc method does not.
func TestSwiftExtract_StampsObjCSelector(t *testing.T) {
	src := `import Foundation

@objc class Mover: NSObject {
    @objc func move(from a: Int, to b: Int) {}
    @objc(customMove:) func moveCustom(x: Int) {}
    @objc func viewDidLoad() {}
    func notExposed(x: Int) {}
}
`
	r, err := NewSwiftExtractor().Extract("Mover.swift", []byte(src))
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	got := map[string]string{}
	for _, n := range r.Nodes {
		if n.Kind != graph.KindMethod {
			continue
		}
		sel := ""
		if n.Meta != nil {
			sel, _ = n.Meta["objc_selector"].(string)
		}
		got[n.Name] = sel
	}
	checks := map[string]string{
		"move":       "moveFrom:to:",
		"moveCustom": "customMove:",
		"viewDidLoad": "viewDidLoad",
		"notExposed": "",
	}
	for name, want := range checks {
		if _, ok := got[name]; !ok {
			t.Errorf("method %q not extracted", name)
			continue
		}
		if got[name] != want {
			t.Errorf("method %q objc_selector = %q, want %q", name, got[name], want)
		}
	}
}
