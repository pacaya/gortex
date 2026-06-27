package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

func TestPascalExtractor_Unit(t *testing.T) {
	src := []byte(`unit Shapes;

interface

uses
  SysUtils, Classes;

type
  TCircle = class
  public
    constructor Create(R: Double);
    function Area: Double;
  end;

implementation

constructor TCircle.Create(R: Double);
begin
end;

function TCircle.Area: Double;
begin
  Result := 3.14 * 1.0;
end;

procedure Hello;
begin
  WriteLn('hi');
end;

end.
`)
	e := NewPascalExtractor()
	require.Equal(t, "pascal", e.Language())

	res, err := e.Extract("Shapes.pas", src)
	require.NoError(t, err)

	var gotUnit, gotType, gotCreate, gotArea, gotHello bool
	for _, n := range res.Nodes {
		// Methods carry the bare name and a class-qualified ID — the
		// convention shared with every other language — instead of jamming
		// the qualified name into Name as the old regex extractor did.
		switch n.ID {
		case "Shapes.pas::Shapes":
			gotUnit = true
		case "Shapes.pas::TCircle":
			gotType = true
		case "Shapes.pas::TCircle.Create":
			gotCreate = true
		case "Shapes.pas::TCircle.Area":
			gotArea = true
		case "Shapes.pas::Hello":
			gotHello = true
		}
	}
	var gotUses bool
	for _, ed := range res.Edges {
		if ed.Kind == graph.EdgeImports && ed.To == "unresolved::import::SysUtils" {
			gotUses = true
		}
	}
	assert.True(t, gotUnit)
	assert.True(t, gotType)
	assert.True(t, gotCreate)
	assert.True(t, gotArea)
	assert.True(t, gotHello)
	assert.True(t, gotUses)
}

func TestPascalExtractor_EmptyInput(t *testing.T) {
	res, err := NewPascalExtractor().Extract("e.pas", []byte(""))
	require.NoError(t, err)
	require.Len(t, res.Nodes, 1)
	assert.Equal(t, graph.KindFile, res.Nodes[0].Kind)
}

// TestPascalParenlessCallEdges is the C1 named test: the tree-sitter extractor
// emits call edges for both parenthesised (`Compute(x)`) and paren-less (`Baz;`)
// calls — the sharpest regression, since the old regex extractor emitted zero —
// and resolves same-file callees directly to their definition at a higher
// provenance tier than an unresolved external call.
func TestPascalParenlessCallEdges(t *testing.T) {
	src := []byte(`unit MyUnit;
interface
type
  TFoo = class
  public
    function Bar(x: Integer): string;
    procedure Baz;
  end;
implementation
function TFoo.Bar(x: Integer): string;
begin
  Result := Compute(x);
  Baz;
end;
procedure TFoo.Baz;
begin
  DoThing;
end;
end.
`)
	res, err := NewPascalExtractor().Extract("MyUnit.pas", src)
	require.NoError(t, err)

	type edge struct {
		from, to string
		origin   string
	}
	var calls []edge
	for _, e := range res.Edges {
		if e.Kind == graph.EdgeCalls {
			calls = append(calls, edge{e.From, e.To, e.Origin})
		}
	}

	has := func(from, to, origin string) bool {
		for _, c := range calls {
			if c.from == from && c.to == to && c.origin == origin {
				return true
			}
		}
		return false
	}

	// Parenthesised call to an external proc — unresolved, name-only tier.
	assert.True(t, has("MyUnit.pas::TFoo.Bar", "unresolved::*.Compute", graph.OriginTextMatched),
		"parenthesised call Compute(x) should emit an edge; got %v", calls)
	// Paren-less call to a same-file method — resolved directly, higher tier.
	assert.True(t, has("MyUnit.pas::TFoo.Bar", "MyUnit.pas::TFoo.Baz", graph.OriginASTResolved),
		"paren-less call Baz; should resolve in-file to TFoo.Baz; got %v", calls)
	// Paren-less call to an external proc — unresolved.
	assert.True(t, has("MyUnit.pas::TFoo.Baz", "unresolved::*.DoThing", graph.OriginTextMatched),
		"paren-less call DoThing; should emit an edge; got %v", calls)
}

func TestPascalExtractor_FactoryChainReceiver(t *testing.T) {
	src := []byte("procedure Run;\nbegin\n  Builder().WithX().Build();\nend;\n")
	res, err := NewPascalExtractor().Extract("w.pas", src)
	if err != nil {
		t.Fatal(err)
	}
	var build *graph.Edge
	seen := map[string]int{}
	for _, e := range res.Edges {
		if e.Kind == graph.EdgeCalls {
			seen[e.To]++
			if e.To == "unresolved::*.Build" {
				build = e
			}
		}
	}
	if build == nil {
		t.Fatal("Build() call edge not found")
	}
	if got, _ := build.Meta["receiver_expr"].(string); got != "Builder().WithX()" {
		t.Errorf("receiver_expr = %q, want Builder().WithX()", got)
	}
	if seen["unresolved::*.Build"] != 1 {
		t.Errorf("Build() emitted %d times, want exactly 1", seen["unresolved::*.Build"])
	}
}
