package languages

import (
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// Django ORM descriptor hint. A QuerySet sets `self._iterable_class =
// ModelIterable` and later iterates `self._iterable_class(self)`, which runs
// the iterable class's `__iter__`. The static graph cannot follow the
// descriptor, so this pass records which class a QuerySet assigns to
// `_iterable_class` by stamping the enclosing class node; the resolver's
// Django claiming resolver reads the hint to bind the descriptor reference
// to `<IterableClass>.__iter__`.

// captureDjangoDescriptors stamps Meta["django_iterable_class"] on a class
// that assigns `_iterable_class = <Class>`. Runs at the tail of Extract.
func captureDjangoDescriptors(result *parser.ExtractionResult, root *sitter.Node, _ string, src []byte) {
	if root == nil || result == nil {
		return
	}
	classByName := map[string]*graph.Node{}
	for _, n := range result.Nodes {
		if n != nil && n.Kind == graph.KindType {
			classByName[n.Name] = n
		}
	}
	if len(classByName) == 0 {
		return
	}
	djangoWalk(root, func(n *sitter.Node) {
		if n.Type() != "assignment" {
			return
		}
		left := n.ChildByFieldName("left")
		if left == nil || djangoAssignTarget(left, src) != "_iterable_class" {
			return
		}
		right := n.ChildByFieldName("right")
		if right == nil || right.Type() != "identifier" {
			return
		}
		cls := djangoEnclosingClassName(n, src)
		cn := classByName[cls]
		if cn == nil {
			return
		}
		if cn.Meta == nil {
			cn.Meta = map[string]any{}
		}
		cn.Meta["django_iterable_class"] = right.Content(src)
	})
}

// djangoAssignTarget returns the attribute/variable name an assignment
// targets: `_iterable_class` for both `self._iterable_class = X` and a
// class-level `_iterable_class = X`.
func djangoAssignTarget(left *sitter.Node, src []byte) string {
	switch left.Type() {
	case "attribute":
		if a := left.ChildByFieldName("attribute"); a != nil {
			return a.Content(src)
		}
	case "identifier":
		return left.Content(src)
	}
	return ""
}

// djangoEnclosingClassName walks up to the nearest class_definition name.
func djangoEnclosingClassName(n *sitter.Node, src []byte) string {
	for cur := n.Parent(); cur != nil; cur = cur.Parent() {
		if cur.Type() == "class_definition" {
			if nm := cur.ChildByFieldName("name"); nm != nil {
				return nm.Content(src)
			}
			return ""
		}
	}
	return ""
}

// djangoWalk visits n and all its named descendants.
func djangoWalk(n *sitter.Node, fn func(*sitter.Node)) {
	if n == nil {
		return
	}
	fn(n)
	for i, _nc := 0, int(n.NamedChildCount()); i < _nc; i++ {
		djangoWalk(n.NamedChild(i), fn)
	}
}
