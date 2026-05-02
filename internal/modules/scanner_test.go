package modules

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestParseGoMod_Variants(t *testing.T) {
	src := []byte(`module github.com/example/x

go 1.22

require github.com/spf13/cobra v1.10.0

require (
	github.com/sabhiram/go-gitignore v0.0.0-20210923224102-525f6e181f06
	github.com/stretchr/testify v1.11.1
	go.uber.org/zap v1.27.1 // indirect
)

replace github.com/foo/bar => ./local/bar
`)
	specs := ParseGoMod(src)
	if len(specs) != 4 {
		t.Fatalf("expected 4 specs, got %d: %+v", len(specs), specs)
	}

	got := map[string]Spec{}
	for _, s := range specs {
		got[s.Path] = s
	}

	if got["github.com/spf13/cobra"].Version != "v1.10.0" {
		t.Errorf("cobra version = %q", got["github.com/spf13/cobra"].Version)
	}
	if !got["go.uber.org/zap"].Indirect {
		t.Errorf("zap should be indirect")
	}
	if got["go.uber.org/zap"].Indirect != true {
		t.Errorf("zap indirect flag wrong")
	}
	if got["github.com/sabhiram/go-gitignore"].Indirect {
		t.Errorf("go-gitignore should not be indirect")
	}
}

func TestParseGoMod_ReplaceDirective(t *testing.T) {
	src := []byte(`module x

require github.com/foo/bar v1.0.0
replace github.com/foo/bar => ./local/bar
`)
	specs := ParseGoMod(src)
	if len(specs) != 1 {
		t.Fatalf("expected 1 spec, got %d", len(specs))
	}
	if specs[0].Replace != "./local/bar" {
		t.Errorf("replace = %q", specs[0].Replace)
	}
}

func TestParseGoMod_Empty(t *testing.T) {
	if got := ParseGoMod(nil); got != nil {
		t.Errorf("nil input should yield nil specs")
	}
	if got := ParseGoMod([]byte("module x\n")); len(got) != 0 {
		t.Errorf("module-only manifest should have no deps")
	}
}

func TestModuleNodeID(t *testing.T) {
	cases := []struct {
		ecosystem, path, version, want string
	}{
		{"go", "github.com/foo/bar", "v1.0.0", "module::go:github.com/foo/bar@v1.0.0"},
		{"go", "github.com/foo/bar", "", "module::go:github.com/foo/bar"},
		{"npm", "lodash", "4.17.0", "module::npm:lodash@4.17.0"},
	}
	for _, c := range cases {
		if got := ModuleNodeID(c.ecosystem, c.path, c.version); got != c.want {
			t.Errorf("ModuleNodeID(%q,%q,%q) = %q, want %q",
				c.ecosystem, c.path, c.version, got, c.want)
		}
	}
}

func TestBuildGraphArtifacts(t *testing.T) {
	specs := []Spec{
		{Ecosystem: "go", Path: "github.com/foo/bar", Version: "v1.0.0", Line: 5},
		{Ecosystem: "go", Path: "github.com/foo/bar", Version: "v1.0.0", Line: 6}, // dup
		{Ecosystem: "go", Path: "go.uber.org/zap", Version: "v1.27.1", Indirect: true, Line: 7},
	}
	nodes, edges := BuildGraphArtifacts("go.mod", specs)

	if len(nodes) != 2 {
		t.Errorf("expected 2 unique nodes, got %d", len(nodes))
	}
	if len(edges) != 3 {
		t.Errorf("expected 3 edges (one per spec, dups produce dup edges), got %d", len(edges))
	}
	for _, e := range edges {
		if e.From != "go.mod" {
			t.Errorf("edge from = %q", e.From)
		}
		if e.Kind != graph.EdgeDependsOnModule {
			t.Errorf("edge kind = %q", e.Kind)
		}
	}

	for _, n := range nodes {
		if n.Kind != graph.KindModule {
			t.Errorf("node kind = %q", n.Kind)
		}
		if n.Meta["ecosystem"] != "go" {
			t.Errorf("ecosystem meta = %v", n.Meta["ecosystem"])
		}
	}
	// Verify the indirect flag on the zap node.
	for _, n := range nodes {
		if n.Meta["path"] == "go.uber.org/zap" {
			if v, _ := n.Meta["indirect"].(bool); !v {
				t.Errorf("zap indirect flag missing")
			}
		}
	}
}

func TestShortName(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"github.com/foo/bar", "bar"},
		{"github.com/foo/bar/v2", "bar"},
		{"github.com/foo/bar/v10", "bar"},
		{"foo", "foo"},
		{"", ""},
	}
	for _, c := range cases {
		if got := shortName(c.in); got != c.want {
			t.Errorf("shortName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
