package languages

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func drupalHookOf(nodes []*graph.Node, fn string) string {
	for _, n := range nodes {
		if n.Name == fn && n.Meta != nil {
			s, _ := n.Meta["drupal_hook"].(string)
			return s
		}
	}
	return ""
}

func drupalHookNode(nodes []*graph.Node, hook string) *graph.Node {
	for _, n := range nodes {
		if n.Kind == graph.KindInterface && n.Name == hook && n.Meta != nil && n.Meta["drupal_hook_def"] != nil {
			return n
		}
	}
	return nil
}

func drupalImplements(edges []*graph.Edge, from, to string) bool {
	for _, e := range edges {
		if e.Kind == graph.EdgeImplements && e.From == from && e.To == to {
			return true
		}
	}
	return false
}

func TestDrupalHooks_DocblockAndNamePattern(t *testing.T) {
	src := `<?php
/**
 * Implements hook_user_login().
 */
function custom_login_handler($account) {}

function mymodule_node_insert($node) {}

function mymodule_helper_util() {}
`
	res, err := NewPHPExtractor().Extract("modules/mymodule/mymodule.module", []byte(src))
	if err != nil {
		t.Fatal(err)
	}

	// Docblock-detected hook.
	if h := drupalHookOf(res.Nodes, "custom_login_handler"); h != "hook_user_login" {
		t.Errorf("@Implements docblock hook = %q (want hook_user_login)", h)
	}
	// Name-pattern detected hook.
	if h := drupalHookOf(res.Nodes, "mymodule_node_insert"); h != "hook_node_insert" {
		t.Errorf("name-pattern hook = %q (want hook_node_insert)", h)
	}
	// A plain helper is not a hook.
	if h := drupalHookOf(res.Nodes, "mymodule_helper_util"); h != "" {
		t.Errorf("a non-hook helper must not be flagged, got %q", h)
	}

	// Synthetic hook node + EdgeImplements wiring (for find_implementations).
	hookNode := drupalHookNode(res.Nodes, "hook_node_insert")
	if hookNode == nil {
		t.Fatalf("no synthetic hook_node_insert node")
	}
	if !drupalImplements(res.Edges, "modules/mymodule/mymodule.module::mymodule_node_insert", hookNode.ID) {
		t.Errorf("missing EdgeImplements from mymodule_node_insert to the hook node")
	}
}

func TestDrupalHooks_BroadenedCoreHooks(t *testing.T) {
	// Common core hooks added to the allowlist plus the variable-segment
	// families, alongside a couple of plain helpers that must stay unflagged.
	src := `<?php
function mymodule_form_alter(&$form, $form_state, $form_id) {}

function mymodule_form_user_login_form_alter(&$form, $form_state) {}

function mymodule_node_access($node, $op, $account) {}

function mymodule_entity_access($entity, $op, $account) {}

function mymodule_preprocess_node(&$variables) {}

function mymodule_theme($existing, $type, $theme, $path) {}

function mymodule_cron() {}

function mymodule_mail($key, &$message, $params) {}

function mymodule_views_data() {}

function mymodule_tokens($type, $tokens, $data, $options) {}

function mymodule_library_info_alter(&$libraries, $extension) {}

function mymodule_field_widget_single_element_string_textfield_form_alter(&$element, $form_state, $context) {}

function mymodule_helper_util() {}

function mymodule_build_thing() {}
`
	res, err := NewPHPExtractor().Extract("modules/mymodule/mymodule.module", []byte(src))
	if err != nil {
		t.Fatal(err)
	}

	want := map[string]string{
		"mymodule_form_alter":                 "hook_form_alter",
		"mymodule_form_user_login_form_alter": "hook_form_FORM_ID_alter",
		"mymodule_node_access":                "hook_node_access",
		"mymodule_entity_access":              "hook_entity_access",
		"mymodule_preprocess_node":            "hook_preprocess_HOOK",
		"mymodule_theme":                      "hook_theme",
		"mymodule_cron":                       "hook_cron",
		"mymodule_mail":                       "hook_mail",
		"mymodule_views_data":                 "hook_views_data",
		"mymodule_tokens":                     "hook_tokens",
		"mymodule_library_info_alter":         "hook_library_info_alter",
		"mymodule_field_widget_single_element_string_textfield_form_alter": "hook_field_widget_WIDGET_TYPE_form_alter",
	}
	for fn, hook := range want {
		if got := drupalHookOf(res.Nodes, fn); got != hook {
			t.Errorf("%s detected as %q, want %q", fn, got, hook)
		}
	}

	// Plain helpers must not be mistaken for hooks (precision guard).
	for _, fn := range []string{"mymodule_helper_util", "mymodule_build_thing"} {
		if got := drupalHookOf(res.Nodes, fn); got != "" {
			t.Errorf("non-hook %s wrongly flagged as %q", fn, got)
		}
	}

	// The variable-segment families collapse to one synthetic node + EdgeImplements.
	hookNode := drupalHookNode(res.Nodes, "hook_form_FORM_ID_alter")
	if hookNode == nil {
		t.Fatalf("no synthetic hook_form_FORM_ID_alter node")
	}
	if !drupalImplements(res.Edges, "modules/mymodule/mymodule.module::mymodule_form_user_login_form_alter", hookNode.ID) {
		t.Errorf("missing EdgeImplements for the form_FORM_ID_alter implementation")
	}
}

func TestDrupalHooks_NonModuleFileNamePatternIgnored(t *testing.T) {
	// In a plain .php file (not a Drupal module file), the name pattern does
	// not apply — only the @Implements docblock would.
	src := `<?php
function mymodule_node_insert($node) {}
`
	res, err := NewPHPExtractor().Extract("src/Service.php", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if h := drupalHookOf(res.Nodes, "mymodule_node_insert"); h != "" {
		t.Errorf("name-pattern must not fire in a non-module .php file, got %q", h)
	}
}
