package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// diBinding is a flattened view of a single DI EdgeProvides edge for
// assertion convenience.
type diBinding struct {
	to            string
	providesFor   string
	di            string
	interfaceFQCN string
	implFQCN      string
	binding       string
	origin        string
}

// collectDIBindings pulls every EdgeProvides edge that carries the
// Meta["di"] DI marker out of a result.
func collectDIBindings(result *parser.ExtractionResult) []diBinding {
	var out []diBinding
	for _, e := range result.Edges {
		if e.Kind != graph.EdgeProvides || e.Meta == nil {
			continue
		}
		di, _ := e.Meta["di"].(string)
		if di == "" {
			continue
		}
		pf, _ := e.Meta["provides_for"].(string)
		ifc, _ := e.Meta["interface_fqcn"].(string)
		impl, _ := e.Meta["impl_fqcn"].(string)
		b, _ := e.Meta["binding"].(string)
		out = append(out, diBinding{
			to:            e.To,
			providesFor:   pf,
			di:            di,
			interfaceFQCN: ifc,
			implFQCN:      impl,
			binding:       b,
			origin:        e.Origin,
		})
	}
	return out
}

// findBinding returns the binding whose provides_for matches the given
// interface short name, or (zero, false).
func findBinding(bindings []diBinding, providesFor string) (diBinding, bool) {
	for _, b := range bindings {
		if b.providesFor == providesFor {
			return b, true
		}
	}
	return diBinding{}, false
}

func TestSymfonyServicesYAML_ClassForm(t *testing.T) {
	// `services:` entry whose key is an interface FQCN and value carries
	// `class: <ConcreteFQCN>`.
	src := []byte(`services:
    App\Domain\ClockInterface:
        class: App\Infra\SystemClock
`)
	result := &parser.ExtractionResult{}
	ok := extractSymfonyServicesYAML("services.yaml", "services.yaml", src, result)
	require.True(t, ok, "services.yaml with services: block must be recognised")

	bindings := collectDIBindings(result)
	require.Len(t, bindings, 1)

	b := bindings[0]
	assert.Equal(t, "symfony", b.di)
	assert.Equal(t, "useClass", b.binding)
	assert.Equal(t, graph.OriginASTInferred, b.origin)
	assert.Equal(t, "ClockInterface", b.providesFor, "provides_for is the interface short name")
	assert.Equal(t, "unresolved::SystemClock", b.to, "To is the unresolved impl short name")
	assert.Equal(t, "App\\Domain\\ClockInterface", b.interfaceFQCN)
	assert.Equal(t, "App\\Infra\\SystemClock", b.implFQCN)
}

func TestSymfonyServicesYAML_AliasForm(t *testing.T) {
	// alias form: `App\SomeInterface: '@App\ConcreteImpl'`.
	src := []byte(`services:
    App\SomeInterface: '@App\ConcreteImpl'
`)
	result := &parser.ExtractionResult{}
	ok := extractSymfonyServicesYAML("services.yaml", "services.yaml", src, result)
	require.True(t, ok)

	bindings := collectDIBindings(result)
	require.Len(t, bindings, 1)
	b := bindings[0]
	assert.Equal(t, "SomeInterface", b.providesFor)
	assert.Equal(t, "unresolved::ConcreteImpl", b.to)
	assert.Equal(t, "App\\ConcreteImpl", b.implFQCN, "leading @ stripped from impl FQCN")
}

func TestSymfonyServicesYAML_NestedAliasKey(t *testing.T) {
	// alias declared via a nested `alias:` key.
	src := []byte(`services:
    App\SomeInterface:
        alias: '@App\ConcreteImpl'
        public: true
`)
	result := &parser.ExtractionResult{}
	ok := extractSymfonyServicesYAML("services.yaml", "services.yaml", src, result)
	require.True(t, ok)

	bindings := collectDIBindings(result)
	require.Len(t, bindings, 1)
	assert.Equal(t, "unresolved::ConcreteImpl", bindings[0].to)
	assert.Equal(t, "SomeInterface", bindings[0].providesFor)
}

func TestSymfonyServicesYAML_SelfRegistrationProducesNoBinding(t *testing.T) {
	// `App\ConcreteImpl: ~` and a redundant self-class declaration are
	// concrete service registrations, not interface→impl bindings.
	src := []byte(`services:
    App\Infra\SystemClock: ~
    App\Infra\Mailer:
        class: App\Infra\Mailer
    App\Domain\ClockInterface:
        class: App\Infra\SystemClock
`)
	result := &parser.ExtractionResult{}
	ok := extractSymfonyServicesYAML("services.yaml", "services.yaml", src, result)
	require.True(t, ok)

	bindings := collectDIBindings(result)
	// Only the ClockInterface→SystemClock entry is a real binding.
	require.Len(t, bindings, 1)
	assert.Equal(t, "ClockInterface", bindings[0].providesFor)
	assert.Equal(t, "unresolved::SystemClock", bindings[0].to)
}

func TestSymfonyServicesYAML_DefaultsAndParametersIgnored(t *testing.T) {
	src := []byte(`parameters:
    locale: en
services:
    _defaults:
        autowire: true
        autoconfigure: true
    App\SomeInterface: '@App\ConcreteImpl'
`)
	result := &parser.ExtractionResult{}
	ok := extractSymfonyServicesYAML("services.yaml", "services.yaml", src, result)
	require.True(t, ok)

	bindings := collectDIBindings(result)
	require.Len(t, bindings, 1, "_defaults must not produce a binding")
	assert.Equal(t, "SomeInterface", bindings[0].providesFor)
}

func TestSymfonyServicesYAML_NotAServicesFile(t *testing.T) {
	// A plain config YAML with no services: block is not recognised.
	src := []byte(`framework:
    secret: '%env(APP_SECRET)%'
`)
	result := &parser.ExtractionResult{}
	ok := extractSymfonyServicesYAML("config.yaml", "config.yaml", src, result)
	assert.False(t, ok)
	assert.Empty(t, collectDIBindings(result))
}

func TestSymfonyServicesYAML_Malformed(t *testing.T) {
	// Malformed YAML must not panic; it simply yields no bindings.
	src := []byte("services:\n    App\\Foo: : : '@bad\n   - broken")
	result := &parser.ExtractionResult{}
	assert.NotPanics(t, func() {
		extractSymfonyServicesYAML("services.yaml", "services.yaml", src, result)
	})
}

func TestSpringContext_DirectIdClassBinding(t *testing.T) {
	// <bean id="<interface FQCN>" class="<impl FQCN>"/> — the id names
	// the interface, the class is the impl.
	src := []byte(`<?xml version="1.0" encoding="UTF-8"?>
<beans xmlns="http://www.springframework.org/schema/beans">
    <bean id="com.app.GreetingService" class="com.app.GreetingServiceImpl"/>
</beans>
`)
	e := NewSpringContextExtractor()
	result, err := e.Extract("applicationContext.xml", src)
	require.NoError(t, err)

	bindings := collectDIBindings(result)
	require.Len(t, bindings, 1)
	b := bindings[0]
	assert.Equal(t, "spring", b.di)
	assert.Equal(t, "useClass", b.binding)
	assert.Equal(t, graph.OriginASTInferred, b.origin)
	assert.Equal(t, "GreetingService", b.providesFor)
	assert.Equal(t, "unresolved::GreetingServiceImpl", b.to)
	assert.Equal(t, "com.app.GreetingService", b.interfaceFQCN)
	assert.Equal(t, "com.app.GreetingServiceImpl", b.implFQCN)
}

func TestSpringContext_RefBinding(t *testing.T) {
	// A consumer bean references a provider bean whose id is an interface
	// FQCN; the referenced bean's id→class is the wired binding.
	src := []byte(`<?xml version="1.0" encoding="UTF-8"?>
<beans xmlns="http://www.springframework.org/schema/beans">
    <bean id="com.app.Repository" class="com.app.JpaRepository"/>
    <bean id="userService" class="com.app.UserService">
        <constructor-arg ref="com.app.Repository"/>
    </bean>
</beans>
`)
	e := NewSpringContextExtractor()
	result, err := e.Extract("applicationContext.xml", src)
	require.NoError(t, err)

	bindings := collectDIBindings(result)
	// Repository→JpaRepository is the interface→impl binding. userService
	// (id != interface-shaped distinct class) also yields a direct id!=class
	// binding (userService→UserService) which we accept as a concrete
	// registration alias; assert the meaningful one is present.
	b, ok := findBinding(bindings, "Repository")
	require.True(t, ok, "ref-based binding for the interface bean must be emitted")
	assert.Equal(t, "unresolved::JpaRepository", b.to)
	assert.Equal(t, "com.app.JpaRepository", b.implFQCN)
}

func TestSpringContext_PropertyRefBinding(t *testing.T) {
	src := []byte(`<beans xmlns="http://www.springframework.org/schema/beans">
    <bean id="com.app.Mailer" class="com.app.SmtpMailer"/>
    <bean id="notifier" class="com.app.Notifier">
        <property name="mailer" ref="com.app.Mailer"/>
    </bean>
</beans>
`)
	e := NewSpringContextExtractor()
	result, err := e.Extract("applicationContext.xml", src)
	require.NoError(t, err)

	b, ok := findBinding(collectDIBindings(result), "Mailer")
	require.True(t, ok)
	assert.Equal(t, "unresolved::SmtpMailer", b.to)
}

func TestSpringContext_NotASpringFile(t *testing.T) {
	// Plain XML that is not a Spring beans file yields only the file node.
	src := []byte(`<?xml version="1.0"?><configuration><foo bar="baz"/></configuration>`)
	e := NewSpringContextExtractor()
	result, err := e.Extract("config.xml", src)
	require.NoError(t, err)
	assert.Empty(t, collectDIBindings(result))
	// The file node is always emitted.
	require.Len(t, result.Nodes, 1)
	assert.Equal(t, graph.KindFile, result.Nodes[0].Kind)
}

func TestSpringContext_Malformed(t *testing.T) {
	// Truncated / malformed XML must not panic.
	src := []byte(`<beans><bean id="com.app.A" class="com.app.AImpl"><constructor-arg ref=`)
	e := NewSpringContextExtractor()
	assert.NotPanics(t, func() {
		_, _ = e.Extract("applicationContext.xml", src)
	})
}

func TestDIShortName(t *testing.T) {
	cases := map[string]string{
		"App\\Foo\\BarImpl": "BarImpl",
		"com.app.BarImpl":   "BarImpl",
		"@App\\Concrete":    "Concrete",
		"Bare":              "Bare",
		"":                  "",
	}
	for in, want := range cases {
		assert.Equalf(t, want, diShortName(in), "diShortName(%q)", in)
	}
}
