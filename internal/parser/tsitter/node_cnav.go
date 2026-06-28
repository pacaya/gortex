package tsitter

// Direct C navigation for the hot child-access path.
//
// go-tree-sitter's Node navigation (Child / NamedChild / Parent, the cursor,
// even the []Node value-slice helpers) funnels through newNode, which
// heap-allocates a *ts.Node for every visited node. Profiling a large index
// put that at ~43% of all bytes allocated — 95% of it NamedChild + Child +
// Parent in the extractor child loops — and the resulting GC churn dominated
// CPU under a memory cap.
//
// The tree-sitter C API returns nodes BY VALUE (TSNode is a 32-byte struct).
// We call those C functions directly and drop the returned value straight
// into the pooled Node arena, so a child visit costs one bump-allocated
// wrapper and zero heap garbage.
//
// The functions are declared here rather than pulled from <tree_sitter/api.h>
// so this file needs no -I path into the go-tree-sitter module cache (which
// is version- and machine-specific). The symbols themselves are defined in
// the tree-sitter C library that go-tree-sitter already compiles and links
// into the binary; C has no name mangling, so a local prototype with the
// identical ABI resolves to them at link time.

/*
#include <stdint.h>

// Mirror of tree-sitter's TSNode (include/tree_sitter/api.h): a 4-word
// context plus two opaque pointers. Layout must match exactly — it is
// reinterpreted from ts.Node, whose sole field is this struct.
typedef struct { uint32_t context[4]; const void *id; const void *tree; } GxTSNode;

// Mirror of TSTreeCursor — reinterpreted from *ts.TreeCursor (whose sole
// field is this struct) so the cursor's current node can be read by value.
typedef struct { const void *tree; const void *id; uint32_t context[3]; } GxTSTreeCursor;

extern GxTSNode ts_node_child(GxTSNode, uint32_t);
extern GxTSNode ts_node_named_child(GxTSNode, uint32_t);
extern GxTSNode ts_node_parent(GxTSNode);
extern GxTSNode ts_node_next_sibling(GxTSNode);
extern GxTSNode ts_node_prev_sibling(GxTSNode);
extern GxTSNode ts_node_next_named_sibling(GxTSNode);
extern GxTSNode ts_node_prev_named_sibling(GxTSNode);
extern GxTSNode ts_node_child_by_field_name(GxTSNode, const char *, uint32_t);
extern GxTSNode ts_tree_cursor_current_node(const GxTSTreeCursor *);
*/
import "C"

import (
	"unsafe"

	ts "github.com/tree-sitter/go-tree-sitter"
)

// init fails fast if a tree-sitter upgrade ever changes the TSNode layout
// out from under the reinterpret casts below, rather than silently corrupting
// navigated nodes at runtime.
func init() {
	if unsafe.Sizeof(ts.Node{}) != unsafe.Sizeof(C.GxTSNode{}) {
		panic("tsitter: ts.Node and TSNode size mismatch — tree-sitter ABI changed, update node_cnav.go")
	}
	if unsafe.Sizeof(ts.TreeCursor{}) != unsafe.Sizeof(C.GxTSTreeCursor{}) {
		panic("tsitter: ts.TreeCursor and TSTreeCursor size mismatch — tree-sitter ABI changed, update node_cnav.go")
	}
}

// asC reinterprets an upstream ts.Node as the locally-declared C node. ts.Node
// is `struct { _inner C.TSNode }`, so its bytes are exactly a TSNode and the
// two share an identical ABI layout.
func asC(n ts.Node) C.GxTSNode { return *(*C.GxTSNode)(unsafe.Pointer(&n)) }

// asGo reinterprets a C node back into an upstream ts.Node value.
func asGo(c C.GxTSNode) ts.Node { return *(*ts.Node)(unsafe.Pointer(&c)) }

// childDirect returns parent's i-th child as a value, with no heap
// allocation. ok is false for a null child (index past the end).
func childDirect(parent ts.Node, i int) (ts.Node, bool) {
	c := C.ts_node_child(asC(parent), C.uint32_t(i))
	if c.id == nil {
		return ts.Node{}, false
	}
	return asGo(c), true
}

// namedChildDirect is childDirect over named children only.
func namedChildDirect(parent ts.Node, i int) (ts.Node, bool) {
	c := C.ts_node_named_child(asC(parent), C.uint32_t(i))
	if c.id == nil {
		return ts.Node{}, false
	}
	return asGo(c), true
}

// parentDirect returns n's parent as a value. ok is false at the root.
func parentDirect(n ts.Node) (ts.Node, bool) {
	c := C.ts_node_parent(asC(n))
	if c.id == nil {
		return ts.Node{}, false
	}
	return asGo(c), true
}

func nextSiblingDirect(n ts.Node) (ts.Node, bool) {
	c := C.ts_node_next_sibling(asC(n))
	if c.id == nil {
		return ts.Node{}, false
	}
	return asGo(c), true
}

func prevSiblingDirect(n ts.Node) (ts.Node, bool) {
	c := C.ts_node_prev_sibling(asC(n))
	if c.id == nil {
		return ts.Node{}, false
	}
	return asGo(c), true
}

func nextNamedSiblingDirect(n ts.Node) (ts.Node, bool) {
	c := C.ts_node_next_named_sibling(asC(n))
	if c.id == nil {
		return ts.Node{}, false
	}
	return asGo(c), true
}

func prevNamedSiblingDirect(n ts.Node) (ts.Node, bool) {
	c := C.ts_node_prev_named_sibling(asC(n))
	if c.id == nil {
		return ts.Node{}, false
	}
	return asGo(c), true
}

// cursorCurrentNode returns a tree cursor's current node by value, avoiding
// TreeCursor.Node's per-step heap *Node. The cursor walk itself (GotoFirstChild
// / GotoNextSibling) stays O(total children); only the node read is changed.
func cursorCurrentNode(cursor *ts.TreeCursor) ts.Node {
	return asGo(C.ts_tree_cursor_current_node((*C.GxTSTreeCursor)(unsafe.Pointer(cursor))))
}

// childByFieldNameDirect returns parent's child for a grammar field name. The
// name's bytes are passed to C by pointer+length (ts_node_child_by_field_name
// does not require NUL termination and reads them only for the duration of the
// call), so no C string is allocated. ok is false when no such field exists.
func childByFieldNameDirect(parent ts.Node, name string) (ts.Node, bool) {
	if name == "" {
		return ts.Node{}, false
	}
	c := C.ts_node_child_by_field_name(
		asC(parent),
		(*C.char)(unsafe.Pointer(unsafe.StringData(name))),
		C.uint32_t(len(name)),
	)
	if c.id == nil {
		return ts.Node{}, false
	}
	return asGo(c), true
}
