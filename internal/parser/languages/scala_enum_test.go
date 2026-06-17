package languages

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestScalaExtractor_Enum(t *testing.T) {
	const scala = `package com.app

enum Color {
  case Red, Green, Blue
}
`
	res, err := NewScalaExtractor().Extract("Color.scala", []byte(scala))
	if err != nil {
		t.Fatal(err)
	}

	var enumNode *graph.Node
	members := map[string]*graph.Node{}
	for _, n := range res.Nodes {
		if n.Kind == graph.KindType && n.Name == "Color" {
			enumNode = n
		}
		if n.Kind == graph.KindEnumMember {
			members[n.Name] = n
		}
	}

	if enumNode == nil {
		t.Fatalf("enum 'Color' was not extracted as a type")
	}
	if enumNode.Meta["kind"] != "enum" {
		t.Errorf("Color should be kind=enum, got meta=%v", enumNode.Meta)
	}

	for _, want := range []string{"Red", "Green", "Blue"} {
		m := members[want]
		if m == nil {
			t.Errorf("enum case %q was not extracted", want)
			continue
		}
		var memberEdge bool
		for _, e := range res.Edges {
			if e.Kind == graph.EdgeMemberOf && e.From == m.ID && e.To == "Color.scala::Color" {
				memberEdge = true
			}
		}
		if !memberEdge {
			t.Errorf("enum case %q should be member_of Color", want)
		}
	}
}
