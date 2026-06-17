package languages

import (
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestRazorExtractor(t *testing.T) {
	const razor = `@page "/counter"
@inherits ComponentBase
@inject IWeatherService Weather

<h1>Counter</h1>
<button @onclick="Increment">Click</button>

@code {
    private int count = 0;
    private void Increment()
    {
        count++;
    }
}
`
	res, err := NewRazorExtractor().Extract("Counter.razor", []byte(razor))
	if err != nil {
		t.Fatal(err)
	}

	var incr *graph.Node
	refs := map[string]bool{}
	for _, n := range res.Nodes {
		if n.Name == "Increment" {
			incr = n
		}
		if n.Name == "__RazorCode" {
			t.Errorf("synthetic wrapper class leaked into the graph: %+v", n)
		}
	}
	for _, e := range res.Edges {
		if e.Kind == graph.EdgeReferences && strings.HasPrefix(e.To, "unresolved::") {
			refs[strings.TrimPrefix(e.To, "unresolved::")] = true
		}
	}

	// The @code block's C# member is extracted, rebased into host coordinates.
	if incr == nil {
		t.Fatalf("@code method 'Increment' was not extracted from the Razor file")
	}
	if incr.Language != "razor" || incr.Meta["inline_script"] != true {
		t.Errorf("delegated symbol lang=%q meta=%v, want razor + inline_script", incr.Language, incr.Meta)
	}
	if incr.StartLine != 10 {
		t.Errorf("Increment StartLine = %d, want 10 (host-file coordinates)", incr.StartLine)
	}

	// @inherits and @inject directives become type references.
	for _, want := range []string{"ComponentBase", "IWeatherService"} {
		if !refs[want] {
			t.Errorf("missing directive type reference %q (refs: %v)", want, refs)
		}
	}
}
