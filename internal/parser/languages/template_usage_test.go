package languages

import (
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestTemplateUsage(t *testing.T) {
	const sfc = `<script setup lang="ts">
import Counter from './Counter.vue'
function noop() {}
</script>

<template>
  <div class="wrap">
    <Counter :start="0" />
    <user-card name="x" />
    <button @click="noop">plain html</button>
    <Teleport to="body"><Modal /></Teleport>
  </div>
</template>
`
	res, err := NewVueExtractor().Extract("App.vue", []byte(sfc))
	if err != nil {
		t.Fatal(err)
	}

	refs := map[string]bool{}
	for _, e := range res.Edges {
		if e.Kind == graph.EdgeReferences && strings.HasPrefix(e.To, "unresolved::") {
			refs[strings.TrimPrefix(e.To, "unresolved::")] = true
		}
	}

	// Component tags become cross-file references; kebab-case is PascalCased.
	for _, want := range []string{"Counter", "UserCard", "Modal"} {
		if !refs[want] {
			t.Errorf("missing component reference %q (got refs: %v)", want, refs)
		}
	}
	// Framework builtins and plain HTML elements are not references.
	if refs["Teleport"] {
		t.Error("framework builtin <Teleport> should be skipped")
	}
	if refs["Button"] || refs["Div"] || refs["button"] {
		t.Error("plain HTML elements should be skipped")
	}
}
