package languages

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestSwiftExtractor_Extension(t *testing.T) {
	const swift = `struct Point {
    var x: Int
    var y: Int
}

extension Point {
    func magnitude() -> Int {
        return x + y
    }
}
`
	res, err := NewSwiftExtractor().Extract("Point.swift", []byte(swift))
	if err != nil {
		t.Fatal(err)
	}

	var mag *graph.Node
	for _, n := range res.Nodes {
		if n.Name == "magnitude" {
			mag = n
		}
	}
	if mag == nil {
		t.Fatal("extension method 'magnitude' was not extracted")
	}
	if mag.Kind != graph.KindMethod {
		t.Errorf("magnitude should be a method, got %s", mag.Kind)
	}
	if mag.Meta["receiver"] != "Point" {
		t.Errorf("magnitude receiver = %v, want Point (the extended type)", mag.Meta["receiver"])
	}

	// The extension member attaches to the extended type's node.
	var memberEdge bool
	for _, e := range res.Edges {
		if e.Kind == graph.EdgeMemberOf && e.From == mag.ID && e.To == "Point.swift::Point" {
			memberEdge = true
		}
	}
	if !memberEdge {
		t.Errorf("magnitude should be member_of Point; mag.ID=%s", mag.ID)
	}
}
