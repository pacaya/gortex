package analysis

import (
	"testing"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
)

func archTestGraph() *graph.Graph {
	g := graph.New()
	add := func(id, file string) {
		g.AddNode(&graph.Node{ID: id, Kind: graph.KindFunction, Name: id, FilePath: file})
	}
	add("dom", "internal/domain/user.go")
	add("inf", "internal/infra/db.go")
	add("api", "internal/api/handler.go")
	add("free", "cmd/tool/main.go") // belongs to no layer
	return g
}

func archConfig() config.ArchitectureConfig {
	return config.ArchitectureConfig{
		Layers: map[string]config.LayerRule{
			"domain": {Paths: []string{"internal/domain/**"}, Deny: []string{"*"}},
			"infra":  {Paths: []string{"internal/infra/**"}, Allow: []string{"domain"}},
			"api":    {Paths: []string{"internal/api/**"}, Allow: []string{"domain", "infra"}},
		},
	}
}

func TestEvaluateArchitecture_AllowedDependency(t *testing.T) {
	g := archTestGraph()
	g.AddEdge(&graph.Edge{From: "api", To: "dom", Kind: graph.EdgeCalls})
	g.AddEdge(&graph.Edge{From: "api", To: "inf", Kind: graph.EdgeCalls})

	v := EvaluateArchitecture(g, archConfig(), []string{"api"})
	if len(v) != 0 {
		t.Fatalf("api -> domain/infra are allowed, got violations: %+v", v)
	}
}

func TestEvaluateArchitecture_DenyWildcard(t *testing.T) {
	g := archTestGraph()
	// domain denies "*" — any cross-layer dependency is a violation.
	g.AddEdge(&graph.Edge{From: "dom", To: "inf", Kind: graph.EdgeCalls})

	v := EvaluateArchitecture(g, archConfig(), []string{"dom"})
	if len(v) != 1 {
		t.Fatalf("expected 1 violation for domain -> infra, got %d: %+v", len(v), v)
	}
	if v[0].Kind != "layer" || v[0].LayerFrom != "domain" || v[0].LayerTo != "infra" {
		t.Errorf("unexpected violation shape: %+v", v[0])
	}
	if v[0].Violator != "dom" || v[0].EdgeType != string(graph.EdgeCalls) {
		t.Errorf("violator/edge_type wrong: %+v", v[0])
	}
}

func TestEvaluateArchitecture_AllowWhitelistMiss(t *testing.T) {
	g := archTestGraph()
	// infra may depend only on domain — infra -> api violates.
	g.AddEdge(&graph.Edge{From: "inf", To: "api", Kind: graph.EdgeCalls})

	v := EvaluateArchitecture(g, archConfig(), []string{"inf"})
	if len(v) != 1 {
		t.Fatalf("expected 1 violation for infra -> api, got %d: %+v", len(v), v)
	}
	if v[0].LayerFrom != "infra" || v[0].LayerTo != "api" {
		t.Errorf("unexpected violation: %+v", v[0])
	}
}

func TestEvaluateArchitecture_UnlayeredUnconstrained(t *testing.T) {
	g := archTestGraph()
	// free belongs to no layer; edges to/from it are unconstrained.
	g.AddEdge(&graph.Edge{From: "free", To: "dom", Kind: graph.EdgeCalls})
	g.AddEdge(&graph.Edge{From: "dom", To: "free", Kind: graph.EdgeCalls})

	if v := EvaluateArchitecture(g, archConfig(), []string{"free", "dom"}); len(v) != 0 {
		t.Errorf("unlayered files must not produce violations, got %+v", v)
	}
}

func TestEvaluateArchitecture_EmptyConfigIsNoop(t *testing.T) {
	g := archTestGraph()
	g.AddEdge(&graph.Edge{From: "dom", To: "inf", Kind: graph.EdgeCalls})
	if v := EvaluateArchitecture(g, config.ArchitectureConfig{}, []string{"dom"}); v != nil {
		t.Errorf("empty architecture config must yield no violations, got %+v", v)
	}
}

func TestEvaluateArchitecture_NameSegmentFallback(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "d", Kind: graph.KindFunction, FilePath: "internal/domain/x.go"})
	g.AddNode(&graph.Node{ID: "i", Kind: graph.KindFunction, FilePath: "internal/infra/y.go"})
	g.AddEdge(&graph.Edge{From: "d", To: "i", Kind: graph.EdgeCalls})
	// Layers declare no Paths — membership falls back to the name
	// appearing as a path segment.
	arch := config.ArchitectureConfig{
		Layers: map[string]config.LayerRule{
			"domain": {Deny: []string{"*"}},
			"infra":  {},
		},
	}
	if v := EvaluateArchitecture(g, arch, []string{"d"}); len(v) != 1 {
		t.Errorf("name-segment fallback should detect domain -> infra, got %+v", v)
	}
}

func TestGlobMatch(t *testing.T) {
	cases := []struct {
		pattern, path string
		want          bool
	}{
		{"internal/domain/**", "internal/domain/user.go", true},
		{"internal/domain/**", "internal/domain/sub/user.go", true},
		{"internal/domain/**", "internal/infra/db.go", false},
		{"internal/**/handler.go", "internal/api/v2/handler.go", true},
		{"**/*_test.go", "internal/api/handler_test.go", true},
		{"cmd/*/main.go", "cmd/tool/main.go", true},
		{"cmd/*/main.go", "cmd/a/b/main.go", false},
		{"**", "anything/at/all.go", true},
	}
	for _, c := range cases {
		if got := globMatch(c.pattern, c.path); got != c.want {
			t.Errorf("globMatch(%q, %q) = %v, want %v", c.pattern, c.path, got, c.want)
		}
	}
}

func TestEvaluateArchitecture_MaxFanOut(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "hub", Kind: graph.KindFunction, FilePath: "internal/domain/hub.go"})
	for i, id := range []string{"t1", "t2", "t3", "t4", "t5", "t6"} {
		g.AddNode(&graph.Node{ID: id, Kind: graph.KindFunction, FilePath: "internal/domain/t.go", StartLine: i})
		g.AddEdge(&graph.Edge{From: "hub", To: id, Kind: graph.EdgeCalls})
	}
	arch := config.ArchitectureConfig{
		Layers: map[string]config.LayerRule{
			"domain": {Paths: []string{"internal/domain/**"}},
		},
		Rules: []config.ArchRule{
			{Layer: "domain", MaxFanOut: 5},
		},
	}
	v := EvaluateArchitecture(g, arch, []string{"hub"})
	if len(v) != 1 || v[0].Kind != "fan_out" {
		t.Fatalf("expected 1 fan_out violation, got %+v", v)
	}
	if v[0].Violator != "hub" {
		t.Errorf("violator = %q, want hub", v[0].Violator)
	}

	// Within the limit — no violation.
	arch.Rules[0].MaxFanOut = 10
	if v := EvaluateArchitecture(g, arch, []string{"hub"}); len(v) != 0 {
		t.Errorf("fan-out under the limit should not violate, got %+v", v)
	}
}

func TestEvaluateArchitecture_DenyCallersOutside(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "secret", Kind: graph.KindFunction, FilePath: "internal/secret/key.go"})
	g.AddNode(&graph.Node{ID: "peer", Kind: graph.KindFunction, FilePath: "internal/secret/other.go"})
	g.AddNode(&graph.Node{ID: "sec", Kind: graph.KindFunction, FilePath: "internal/security/auth.go"})
	g.AddNode(&graph.Node{ID: "api", Kind: graph.KindFunction, FilePath: "internal/api/handler.go"})
	g.AddEdge(&graph.Edge{From: "peer", To: "secret", Kind: graph.EdgeCalls}) // intra-set: allowed
	g.AddEdge(&graph.Edge{From: "sec", To: "secret", Kind: graph.EdgeCalls})  // allowlisted: allowed
	g.AddEdge(&graph.Edge{From: "api", To: "secret", Kind: graph.EdgeCalls})  // outside: violation

	arch := config.ArchitectureConfig{
		Rules: []config.ArchRule{
			{
				Name:               "secret-boundary",
				Pattern:            "internal/secret/**",
				DenyCallersOutside: []string{"internal/security/**"},
			},
		},
	}
	v := EvaluateArchitecture(g, arch, []string{"secret"})
	if len(v) != 1 {
		t.Fatalf("expected exactly one caller-boundary violation, got %d: %+v", len(v), v)
	}
	if v[0].Kind != "caller_boundary" || v[0].Violator != "api" {
		t.Errorf("unexpected violation: %+v", v[0])
	}
	if v[0].RuleName != "secret-boundary" {
		t.Errorf("rule name = %q, want secret-boundary", v[0].RuleName)
	}
}

func TestEvaluateArchitecture_RuleWithoutSelectorMatchesNothing(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "x", Kind: graph.KindFunction, FilePath: "internal/x.go"})
	g.AddNode(&graph.Node{ID: "y", Kind: graph.KindFunction, FilePath: "internal/y.go"})
	g.AddEdge(&graph.Edge{From: "x", To: "y", Kind: graph.EdgeCalls})
	arch := config.ArchitectureConfig{
		Rules: []config.ArchRule{{MaxFanOut: 0, Message: "no selector"}},
	}
	if v := EvaluateArchitecture(g, arch, []string{"x"}); len(v) != 0 {
		t.Errorf("a rule with no layer/pattern selector must match nothing, got %+v", v)
	}
}
