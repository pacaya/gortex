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

// TestTemplateUsageTwoRenderSitesPositioned proves the upgrade over a
// name-deduplicated single reference: rendering the same child component at two
// template locations emits TWO positioned edges — distinct line numbers, each
// AST-resolved provenance, each carrying the template role — so find_usages can
// report every render site rather than collapsing them into one position-less
// reference.
func TestTemplateUsageTwoRenderSitesPositioned(t *testing.T) {
	const sfc = `<script setup lang="ts">
import Counter from './Counter.vue'
</script>

<template>
  <div>
    <Counter :start="0" />
    <Counter :start="1" />
  </div>
</template>
`
	res, err := NewVueExtractor().Extract("App.vue", []byte(sfc))
	if err != nil {
		t.Fatal(err)
	}

	var sites []*graph.Edge
	for _, e := range res.Edges {
		if e.Kind == graph.EdgeReferences && e.To == "unresolved::Counter" {
			sites = append(sites, e)
		}
	}
	if len(sites) != 2 {
		t.Fatalf("expected 2 positioned render edges for Counter, got %d", len(sites))
	}

	lines := map[int]bool{}
	for _, e := range sites {
		if e.Line == 0 {
			t.Errorf("render edge for Counter has no line number")
		}
		if e.Origin != graph.OriginASTResolved {
			t.Errorf("render edge at line %d origin=%v, want OriginASTResolved", e.Line, e.Origin)
		}
		if isTmpl, _ := e.Meta["template"].(bool); !isTmpl {
			t.Errorf("render edge at line %d missing Meta[template]=true", e.Line)
		}
		if got := graph.RefContextOf(e, graph.KindType); got != graph.RefContextTemplate {
			t.Errorf("render edge at line %d ref_context=%q, want %q", e.Line, got, graph.RefContextTemplate)
		}
		lines[e.Line] = true
	}
	if len(lines) != 2 {
		t.Errorf("expected 2 distinct render-site lines, got %v", lines)
	}
}

func templateExprCall(edges []*graph.Edge, from, name string) *graph.Edge {
	for _, e := range edges {
		if e.Kind != graph.EdgeCalls || e.From != from || e.To != "unresolved::"+name || e.Meta == nil {
			continue
		}
		if v, _ := e.Meta["via"].(string); v == "template_expr" {
			return e
		}
	}
	return nil
}

func TestTemplateExpr_SvelteMarkupCalls(t *testing.T) {
	src := []byte(`<script>
  function cn(x) { return x }
  function fmt(p) { return p }
</script>

<div class={cn(active)}>{fmt(price)}</div>
<ul>{items.map((i) => i)}</ul>
`)
	res, err := NewSvelteExtractor().Extract("Button.svelte", src)
	if err != nil {
		t.Fatal(err)
	}
	const comp = "Button.svelte::Button"
	if templateExprCall(res.Edges, comp, "cn") == nil {
		t.Errorf("expected a template_expr call to cn from class={cn(active)}")
	}
	if templateExprCall(res.Edges, comp, "fmt") == nil {
		t.Errorf("expected a template_expr call to fmt from {fmt(price)}")
	}
	// A method call's name is not a receiver-less free-function call.
	if templateExprCall(res.Edges, comp, "map") != nil {
		t.Errorf("method call items.map must not emit a template_expr call to map")
	}
	var sawCn bool
	for _, n := range res.Nodes {
		if n.Kind == graph.KindFunction && n.Name == "cn" {
			sawCn = true
		}
	}
	if !sawCn {
		t.Errorf("the script function cn should be present for resolution")
	}
}

func TestTemplateExpr_SvelteRuneSuppressed(t *testing.T) {
	src := []byte(`<script>
  let count = $state(0)
</script>
<button>{$state(5)}</button>
`)
	res, err := NewSvelteExtractor().Extract("Counter.svelte", src)
	if err != nil {
		t.Fatal(err)
	}
	if templateExprCall(res.Edges, "Counter.svelte::Counter", "$state") != nil {
		t.Errorf("svelte rune $state must be suppressed, not emitted as a template call")
	}
}

func TestTemplateExpr_AstroMultiLineMapBlock(t *testing.T) {
	// A helper called only inside an Astro multi-line `{collection.map(() => (...))}`
	// render block must resolve — the open-brace span is captured in one pass and
	// the frontmatter is not double-scanned.
	src := []byte(`---
function fmt(p) { return p }
const posts = []
---
<ul>
{posts.map((p) => (
  <li>{fmt(p)}</li>
))}
</ul>
`)
	res, err := NewAstroExtractor().Extract("Page.astro", src)
	if err != nil {
		t.Fatal(err)
	}
	const comp = "Page.astro::Page"
	e := templateExprCall(res.Edges, comp, "fmt")
	if e == nil {
		t.Fatalf("expected a template_expr call to fmt from the multi-line map block")
	}
	if e.Line != 7 {
		t.Errorf("fmt call line = %d (want 7, the actual call site inside the span)", e.Line)
	}
	// The `.map(` is a method call, not a free function.
	if templateExprCall(res.Edges, comp, "map") != nil {
		t.Errorf("posts.map must not emit a template_expr call to map")
	}
	// Frontmatter is delegated TS — its own `fmt` definition must not be
	// re-scanned as a markup call (only one template_expr fmt edge, from L7).
	count := 0
	for _, ed := range res.Edges {
		if ed.Kind == graph.EdgeCalls && ed.To == "unresolved::fmt" && ed.Meta != nil {
			if v, _ := ed.Meta["via"].(string); v == "template_expr" {
				count++
			}
		}
	}
	if count != 1 {
		t.Errorf("expected exactly 1 template_expr fmt call (no frontmatter double-scan), got %d", count)
	}
}

func TestTemplateMustacheSpans_MultiLine(t *testing.T) {
	// A group that opens on one line and closes several lines later is one span.
	b := []byte("a {posts.map((p) => (\n  fmt(p)\n))} b")
	spans := templateMustacheSpans(b)
	if len(spans) != 1 {
		t.Fatalf("expected 1 multi-line span, got %d", len(spans))
	}
	body := string(b[spans[0].start:spans[0].end])
	if want := "posts.map((p) => (\n  fmt(p)\n))"; body != want {
		t.Errorf("span body = %q (want %q)", body, want)
	}
}
