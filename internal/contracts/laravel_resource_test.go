package contracts

import "testing"

func laravelContractIDs(cs []Contract) map[string]Contract {
	out := map[string]Contract{}
	for _, c := range cs {
		if fw, _ := c.Meta["framework"].(string); fw == "laravel" {
			out[c.ID] = c
		}
	}
	return out
}

func TestLaravelResource_FullExpansion(t *testing.T) {
	src := []byte(`<?php
Route::resource('users', UserController::class);
`)
	ext := &HTTPExtractor{}
	byID := laravelContractIDs(ext.Extract("routes/web.php", src, nil, nil))
	want := []string{
		"http::GET::/users", "http::GET::/users/create", "http::POST::/users",
		"http::GET::/users/{p1}", "http::GET::/users/{p1}/edit",
		"http::PUT::/users/{p1}", "http::DELETE::/users/{p1}",
	}
	if len(byID) != 7 {
		t.Fatalf("Route::resource should expand to 7 routes, got %d: %v", len(byID), keysOf(byID))
	}
	for _, id := range want {
		c, ok := byID[id]
		if !ok {
			t.Errorf("missing route %s", id)
			continue
		}
		if hc, _ := c.Meta["handler_class"].(string); hc != "UserController" {
			t.Errorf("%s handler_class = %q (want UserController)", id, hc)
		}
	}
}

func TestLaravelResource_ApiResourceDropsCreateEdit(t *testing.T) {
	src := []byte(`<?php
Route::apiResource('posts', PostController::class);
`)
	ext := &HTTPExtractor{}
	byID := laravelContractIDs(ext.Extract("routes/api.php", src, nil, nil))
	if len(byID) != 5 {
		t.Fatalf("apiResource should expand to 5 routes (no create/edit), got %d: %v", len(byID), keysOf(byID))
	}
	if _, ok := byID["http::GET::/posts/create"]; ok {
		t.Errorf("apiResource must not produce the create route")
	}
	if _, ok := byID["http::GET::/posts/{p1}/edit"]; ok {
		t.Errorf("apiResource must not produce the edit route")
	}
}

func TestLaravelResource_OnlyFilter(t *testing.T) {
	src := []byte(`<?php
Route::resource('tags', TagController::class)->only(['index', 'show']);
`)
	ext := &HTTPExtractor{}
	byID := laravelContractIDs(ext.Extract("routes/web.php", src, nil, nil))
	if len(byID) != 2 {
		t.Fatalf("->only(['index','show']) should yield 2 routes, got %d: %v", len(byID), keysOf(byID))
	}
	if _, ok := byID["http::GET::/tags"]; !ok {
		t.Errorf("missing index route")
	}
	if _, ok := byID["http::GET::/tags/{p1}"]; !ok {
		t.Errorf("missing show route")
	}
}

func TestLaravelResource_ExceptFilter(t *testing.T) {
	src := []byte(`<?php
Route::resource('items', ItemController::class)->except(['destroy', 'edit', 'create']);
`)
	ext := &HTTPExtractor{}
	byID := laravelContractIDs(ext.Extract("routes/web.php", src, nil, nil))
	if _, ok := byID["http::DELETE::/items/{p1}"]; ok {
		t.Errorf("->except must drop the destroy route")
	}
	if len(byID) != 4 {
		t.Errorf("except(destroy,edit,create) should leave 4 routes, got %d: %v", len(byID), keysOf(byID))
	}
}
