package languages

import (
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// TestVueNuxt_LazyAndBuiltinComponents pins Nuxt component auto-import name
// normalization: a `<LazyBaseButton>` lazy-hydrated usage references the same
// BaseButton component as `<BaseButton>`, Nuxt framework components
// (`<NuxtLink>`) never become cross-file references, and the existing PascalCase
// component edge is unaffected.
func TestVueNuxt_LazyAndBuiltinComponents(t *testing.T) {
	sfc := "<template>\n" +
		"  <div>\n" +
		"    <BaseButton />\n" +
		"    <LazyBaseButton />\n" +
		"    <NuxtLink to=\"/\">home</NuxtLink>\n" +
		"  </div>\n" +
		"</template>\n"

	res, err := NewVueExtractor().Extract("pages/Index.vue", []byte(sfc))
	if err != nil {
		t.Fatal(err)
	}

	refs := map[string]int{}
	for _, e := range res.Edges {
		if e.Kind == graph.EdgeReferences && strings.HasPrefix(e.To, "unresolved::") &&
			e.Meta != nil && e.Meta["template"] == true {
			refs[strings.TrimPrefix(e.To, "unresolved::")]++
		}
	}
	if refs["BaseButton"] < 2 {
		t.Errorf("expected both <BaseButton> and <LazyBaseButton> to reference BaseButton, got %d", refs["BaseButton"])
	}
	if refs["LazyBaseButton"] != 0 {
		t.Errorf("Lazy prefix must be stripped; got %d LazyBaseButton refs", refs["LazyBaseButton"])
	}
	if refs["NuxtLink"] != 0 {
		t.Errorf("NuxtLink is a framework builtin and must not be referenced, got %d", refs["NuxtLink"])
	}
}

// TestVueNuxt_ComposableDestructureHandler pins the composable-destructure
// handler binding: a `@click="c"` where c aliases useModal().close binds to the
// composable member (close), while a normal handler bound to an in-script
// function is unaffected.
func TestVueNuxt_ComposableDestructureHandler(t *testing.T) {
	sfc := "<script setup>\n" +
		"const { close: c, open } = useModal()\n" +
		"function onClick() {}\n" +
		"</script>\n" +
		"<template>\n" +
		"  <button @click=\"c\">x</button>\n" +
		"  <button @click=\"open\">o</button>\n" +
		"  <button @mouseover=\"onClick\">m</button>\n" +
		"</template>\n"

	res, err := NewVueExtractor().Extract("Modal.vue", []byte(sfc))
	if err != nil {
		t.Fatal(err)
	}

	var renamedBound, plainBound, funcBound bool
	for _, e := range res.Edges {
		if e.Meta == nil {
			continue
		}
		switch e.Meta["via"] {
		case "composable_handler":
			if e.To == "unresolved::close" && e.Meta["composable"] == "useModal" {
				renamedBound = true // @click="c" -> useModal().close
			}
			if e.To == "unresolved::open" && e.Meta["composable"] == "useModal" {
				plainBound = true // @click="open" -> useModal().open
			}
		case "template_handler":
			if strings.HasSuffix(e.To, "::onClick") {
				funcBound = true // existing in-script function binding unaffected
			}
		}
	}
	if !renamedBound {
		t.Error("@click=\"c\" did not bind to the composable member useModal().close")
	}
	if !plainBound {
		t.Error("@click=\"open\" did not bind to the composable member useModal().open")
	}
	if !funcBound {
		t.Error("@mouseover=\"onClick\" must still bind to the in-script onClick function")
	}
}
