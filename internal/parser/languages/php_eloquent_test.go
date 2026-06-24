package languages

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func eloquentEdge(edges []*graph.Edge, model string) *graph.Edge {
	for _, e := range edges {
		if e.Kind != graph.EdgeCalls || e.Meta == nil {
			continue
		}
		if m, _ := e.Meta["eloquent_model"].(string); m == model {
			return e
		}
	}
	return nil
}

func hasCallTo(edges []*graph.Edge, to string) bool {
	for _, e := range edges {
		if e.Kind == graph.EdgeCalls && e.To == to {
			return true
		}
	}
	return false
}

func TestPHPEloquent_StaticFinderBindsModel(t *testing.T) {
	src := `<?php
namespace App\Http;
class C {
  public function show() {
    $u = User::find(1);
    $q = User::where('active', true);
    $p = Post::create(['title' => 'x']);
  }
}
`
	res, err := NewPHPExtractor().Extract("c.php", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	find := eloquentEdge(res.Edges, "User")
	if find == nil {
		t.Fatalf("User::find should bind to the User model")
	}
	if find.To != "unresolved::User" {
		t.Errorf("User::find To = %q (want unresolved::User, a model ref not a method)", find.To)
	}
	if !hasCallTo(res.Edges, "unresolved::User") {
		t.Errorf("expected a model ref to User")
	}
	if eloquentEdge(res.Edges, "Post") == nil {
		t.Errorf("Post::create should bind to the Post model")
	}
}

func TestPHPEloquent_NonModelStaticUnchanged(t *testing.T) {
	src := `<?php
class C {
  public function f() {
    $x = SomeHelper::staticHelper();
    $c = Cache::get('k');
  }
}
`
	res, err := NewPHPExtractor().Extract("c.php", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	// staticHelper is not an Eloquent method → not bound to a model.
	for _, e := range res.Edges {
		if e.Meta != nil {
			if m, _ := e.Meta["eloquent_model"].(string); m == "SomeHelper" {
				t.Errorf("a non-Eloquent static call must not be treated as a model finder")
			}
		}
	}
	// Cache::get keeps its facade tagging.
	var facadeKept bool
	for _, e := range res.Edges {
		if e.Meta != nil {
			if f, _ := e.Meta["facade"].(string); f == "Cache" {
				facadeKept = true
			}
		}
	}
	if !facadeKept {
		t.Errorf("Cache::get facade tagging should be preserved")
	}
}
