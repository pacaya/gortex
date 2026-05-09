package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

func runTSExtractFixture(t *testing.T, filePath, src string) *extractedFixture {
	t.Helper()
	ext := NewTypeScriptExtractor()
	result, err := ext.Extract(filePath, []byte(src))
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	return foldFixture(result)
}

func runJSExtractFixture(t *testing.T, filePath, src string) *extractedFixture {
	t.Helper()
	ext := NewJavaScriptExtractor()
	result, err := ext.Extract(filePath, []byte(src))
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	return foldFixture(result)
}

func foldFixture(result *parser.ExtractionResult) *extractedFixture {
	fix := &extractedFixture{
		nodesByID:    map[string]*graph.Node{},
		nodesByKind:  map[graph.NodeKind][]*graph.Node{},
		edgesByKind:  map[graph.EdgeKind][]*graph.Edge{},
		edgesByOwner: map[string][]*graph.Edge{},
	}
	for _, n := range result.Nodes {
		fix.nodesByID[n.ID] = n
		fix.nodesByKind[n.Kind] = append(fix.nodesByKind[n.Kind], n)
	}
	for _, e := range result.Edges {
		fix.edgesByKind[e.Kind] = append(fix.edgesByKind[e.Kind], e)
		fix.edgesByOwner[e.From] = append(fix.edgesByOwner[e.From], e)
	}
	return fix
}

func TestJSXRender_TSXFunctionRendersChild(t *testing.T) {
	src := `function Page() {
  return (
    <div>
      <Header />
      <Card title="hi" />
      <Button />
    </div>
  );
}
`
	fix := runTSExtractFixture(t, "src/Page.tsx", src)
	rendered := fix.edgesByKind[graph.EdgeRendersChild]
	require.Len(t, rendered, 3, "expected three renders_child edges (Header, Card, Button)")

	names := map[string]bool{}
	for _, e := range rendered {
		assert.Equal(t, "src/Page.tsx::Page", e.From)
		name, _ := e.Meta["child_name"].(string)
		names[name] = true
		assert.True(t, e.To == "unresolved::"+name, "unresolved target shape")
	}
	assert.True(t, names["Header"])
	assert.True(t, names["Card"])
	assert.True(t, names["Button"])
}

func TestJSXRender_LowercaseElementsSkipped(t *testing.T) {
	src := `function App() {
  return (
    <div>
      <span>hi</span>
      <p />
      <Inner />
    </div>
  );
}
`
	fix := runTSExtractFixture(t, "src/App.tsx", src)
	rendered := fix.edgesByKind[graph.EdgeRendersChild]
	require.Len(t, rendered, 1, "only the uppercase-first-letter component should produce an edge")
	name, _ := rendered[0].Meta["child_name"].(string)
	assert.Equal(t, "Inner", name)
}

func TestJSXRender_QualifiedComponentName(t *testing.T) {
	src := `function Page() {
  return (
    <div>
      <Foo.Bar />
      <Header.Item label="x" />
    </div>
  );
}
`
	fix := runTSExtractFixture(t, "src/Page.tsx", src)
	rendered := fix.edgesByKind[graph.EdgeRendersChild]
	require.Len(t, rendered, 2, "qualified names should each produce one edge")
	names := map[string]bool{}
	for _, e := range rendered {
		name, _ := e.Meta["child_name"].(string)
		names[name] = true
	}
	assert.True(t, names["Foo.Bar"])
	assert.True(t, names["Header.Item"])
}

func TestJSXRender_DedupeRepeatedChild(t *testing.T) {
	src := `function Page({rows}: any) {
  return (
    <ul>
      {rows.map(r => <Row key={r.id} />)}
      <Row />
    </ul>
  );
}
`
	fix := runTSExtractFixture(t, "src/Page.tsx", src)
	rendered := fix.edgesByKind[graph.EdgeRendersChild]
	require.Len(t, rendered, 1, "the same child rendered multiple times must produce exactly one edge")
}

func TestJSXRender_ArrowComponentBody(t *testing.T) {
	src := `const Page = () => (
  <div>
    <Card />
  </div>
);
`
	fix := runTSExtractFixture(t, "src/Page.tsx", src)
	rendered := fix.edgesByKind[graph.EdgeRendersChild]
	require.Len(t, rendered, 1)
	assert.Equal(t, "src/Page.tsx::Page", rendered[0].From)
}

func TestJSXRender_JavaScriptArrow(t *testing.T) {
	src := `const Page = () => (
  <div>
    <Banner />
  </div>
);
`
	fix := runJSExtractFixture(t, "src/Page.jsx", src)
	rendered := fix.edgesByKind[graph.EdgeRendersChild]
	require.Len(t, rendered, 1)
	assert.Equal(t, "src/Page.jsx::Page", rendered[0].From)
	name, _ := rendered[0].Meta["child_name"].(string)
	assert.Equal(t, "Banner", name)
}

func TestIsJSXComponentName(t *testing.T) {
	cases := map[string]bool{
		"":         false,
		"div":      false,
		"span":     false,
		"Button":   true,
		"Foo.Bar":  true, // qualified name
		"x.y":      true, // any qualified name treated as component (rare in practice; safe default)
		"123":      false,
	}
	for input, want := range cases {
		got := isJSXComponentName(input)
		assert.Equal(t, want, got, "input=%q", input)
	}
}
