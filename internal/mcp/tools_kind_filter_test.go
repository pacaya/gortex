package mcp

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// TestFilterNodesByKind exercises the kind argument introduced for
// search_symbols by spec-graph-coverage.md §7.2.
func TestFilterNodesByKind(t *testing.T) {
	nodes := []*graph.Node{
		{ID: "a", Kind: graph.KindFunction, Name: "Foo"},
		{ID: "b", Kind: graph.KindTodo, Name: "todo:10"},
		{ID: "c", Kind: graph.KindLicense, Name: "MIT"},
		{ID: "d", Kind: graph.KindTeam, Name: "@core"},
		{ID: "e", Kind: graph.KindMethod, Name: "Bar"},
	}

	cases := []struct {
		name string
		arg  string
		want []string // node IDs in input order
	}{
		{"empty arg keeps all", "", []string{"a", "b", "c", "d", "e"}},
		{"single kind", "todo", []string{"b"}},
		{"multiple kinds", "todo,license", []string{"b", "c"}},
		{"case-insensitive", "TODO , LICENSE", []string{"b", "c"}},
		{"unknown kind drops everything", "nonexistent", nil},
		{"function still selectable", "function", []string{"a"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := filterNodesByKind(nodes, tc.arg)
			if len(got) != len(tc.want) {
				t.Fatalf("len = %d, want %d (got %+v)", len(got), len(tc.want), got)
			}
			for i, n := range got {
				if n.ID != tc.want[i] {
					t.Errorf("position %d: id = %q, want %q", i, n.ID, tc.want[i])
				}
			}
		})
	}
}
