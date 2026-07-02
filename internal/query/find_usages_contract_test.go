package query

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/zzet/gortex/internal/graph"
)

// A framework-contract `provides` edge landing on a @Bean factory method is
// plumbing, not a usage — find_usages on the method must exclude it while
// keeping genuine call usages. The same `provides` kind landing on a DI token
// (a non-code symbol) IS the meaningful relationship and must survive.
func TestFindUsages_ExcludesFrameworkContractOnCode(t *testing.T) {
	g := graph.New()
	cfg := "Config.java::AppConfig"
	bean := "Config.java::AppConfig.localeResolver"
	caller := "Other.java::Other.use"
	token := "Tokens.java::API_TOKEN"
	provider := "Providers.java::Providers.register"

	g.AddNode(&graph.Node{ID: "Config.java", Kind: graph.KindFile, Name: "Config.java", Language: "java"})
	g.AddNode(&graph.Node{ID: cfg, Kind: graph.KindType, Name: "AppConfig", FilePath: "Config.java", Language: "java"})
	g.AddNode(&graph.Node{ID: bean, Kind: graph.KindMethod, Name: "localeResolver", FilePath: "Config.java", Language: "java"})
	g.AddNode(&graph.Node{ID: caller, Kind: graph.KindMethod, Name: "use", FilePath: "Other.java", Language: "java"})
	g.AddNode(&graph.Node{ID: token, Kind: graph.KindConstant, Name: "API_TOKEN", FilePath: "Tokens.java", Language: "java"})
	g.AddNode(&graph.Node{ID: provider, Kind: graph.KindMethod, Name: "register", FilePath: "Providers.java", Language: "java"})

	// @Bean plumbing: AppConfig provides the localeResolver factory method.
	g.AddEdge(&graph.Edge{From: cfg, To: bean, Kind: graph.EdgeProvides, FilePath: "Config.java", Line: 9})
	// A genuine call usage of the bean method.
	g.AddEdge(&graph.Edge{From: caller, To: bean, Kind: graph.EdgeCalls, FilePath: "Other.java", Line: 12})
	// DI-token provider: register() provides the API_TOKEN.
	g.AddEdge(&graph.Edge{From: provider, To: token, Kind: graph.EdgeProvides, FilePath: "Providers.java", Line: 4})

	e := NewEngine(g)

	beanUsages := e.FindUsages(bean)
	var sawProvides, sawCall bool
	for _, ed := range beanUsages.Edges {
		switch ed.Kind {
		case graph.EdgeProvides:
			sawProvides = true
		case graph.EdgeCalls:
			sawCall = true
		}
	}
	assert.False(t, sawProvides, "find_usages on a @Bean method must not surface the provides contract edge")
	assert.True(t, sawCall, "find_usages on a @Bean method must still surface genuine call usages")

	// The empty-usage @Bean case: a bean method whose only incoming edge is
	// the provides contract must report zero usages.
	g.AddNode(&graph.Node{ID: "Config.java::AppConfig.cacheCustomizer", Kind: graph.KindMethod, Name: "cacheCustomizer", FilePath: "Config.java", Language: "java"})
	g.AddEdge(&graph.Edge{From: cfg, To: "Config.java::AppConfig.cacheCustomizer", Kind: graph.EdgeProvides, FilePath: "Config.java", Line: 15})
	e2 := NewEngine(g)
	emptyBean := e2.FindUsages("Config.java::AppConfig.cacheCustomizer")
	assert.Empty(t, emptyBean.Edges, "a @Bean method whose only edge is the provides contract has no usages")

	// A DI token keeps its providers.
	tokenUsages := e2.FindUsages(token)
	var tokenSawProvides bool
	for _, ed := range tokenUsages.Edges {
		if ed.Kind == graph.EdgeProvides {
			tokenSawProvides = true
		}
	}
	assert.True(t, tokenSawProvides, "find_usages on a DI token must still surface its providers")
}
