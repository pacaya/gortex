package contracts

import (
	"testing"
)

// TestHTTPExtractor_ClientAlias_NameSuffixMethod covers the primary
// shape: a configured wrapper whose name ends in an HTTP verb
// (apiGet/apiPost). The verb comes from the name suffix and the path
// from the first string-literal argument, producing the canonical
// http::METHOD::/path consumer contract the matcher pairs against
// providers.
func TestHTTPExtractor_ClientAlias_NameSuffixMethod(t *testing.T) {
	src := []byte(`
async function loadUsers() {
  const users = await apiGet('/users');
  await apiPost('/users', { name: 'x' });
}
`)
	nodes := makeNodes("api.ts", []struct {
		name       string
		start, end int
	}{
		{"loadUsers", 2, 5},
	})

	ext := &HTTPExtractor{ClientAliases: []string{"apiGet", "apiPost"}}
	contracts := ext.Extract("api.ts", src, nodes, nil)

	byID := map[string]Contract{}
	for _, c := range contracts {
		byID[c.ID] = c
	}

	get, ok := byID["http::GET::/users"]
	if !ok {
		t.Fatalf("missing http::GET::/users; have: %v", keysOf(byID))
	}
	if get.Role != RoleConsumer {
		t.Errorf("GET role = %s, want consumer", get.Role)
	}
	if get.Type != ContractHTTP {
		t.Errorf("GET type = %s, want http", get.Type)
	}
	if get.Meta["framework"] != "client-alias" {
		t.Errorf("GET framework = %v, want client-alias", get.Meta["framework"])
	}
	if get.Meta["alias"] != "apiGet" {
		t.Errorf("GET alias = %v, want apiGet", get.Meta["alias"])
	}
	if get.SymbolID != "api.ts::loadUsers" {
		t.Errorf("GET symbol = %q, want api.ts::loadUsers", get.SymbolID)
	}

	post, ok := byID["http::POST::/users"]
	if !ok {
		t.Fatalf("missing http::POST::/users; have: %v", keysOf(byID))
	}
	if post.Role != RoleConsumer {
		t.Errorf("POST role = %s, want consumer", post.Role)
	}
	if post.Meta["method"] != "POST" {
		t.Errorf("POST method = %v, want POST", post.Meta["method"])
	}
}

// TestHTTPExtractor_ClientAlias_TwoArgMethodForm covers the generic
// (method, path) two-arg form for a suffix-less alias: apiCall('GET',
// '/users') and client.request('POST', '/orders'). The first literal is
// read as the method, the second as the path.
func TestHTTPExtractor_ClientAlias_TwoArgMethodForm(t *testing.T) {
	src := []byte(`
async function go() {
  await apiCall('GET', '/users');
  await client.request('POST', '/orders', body);
}
`)
	nodes := makeNodes("client.ts", []struct {
		name       string
		start, end int
	}{
		{"go", 2, 5},
	})

	ext := &HTTPExtractor{ClientAliases: []string{"apiCall", "client.request"}}
	contracts := ext.Extract("client.ts", src, nodes, nil)

	byID := map[string]Contract{}
	for _, c := range contracts {
		byID[c.ID] = c
	}

	if c, ok := byID["http::GET::/users"]; !ok {
		t.Errorf("missing http::GET::/users; have: %v", keysOf(byID))
	} else if c.Role != RoleConsumer {
		t.Errorf("GET role = %s, want consumer", c.Role)
	}

	if c, ok := byID["http::POST::/orders"]; !ok {
		t.Errorf("missing http::POST::/orders; have: %v", keysOf(byID))
	} else {
		if c.Role != RoleConsumer {
			t.Errorf("POST role = %s, want consumer", c.Role)
		}
		if c.Meta["alias"] != "client.request" {
			t.Errorf("POST alias = %v, want client.request", c.Meta["alias"])
		}
	}
}

// TestHTTPExtractor_ClientAlias_ParametricPath asserts a template-literal
// path with an inline placeholder normalises to the positional {p1}
// form so the alias-derived consumer pairs with a provider's parametric
// route ID — the whole point of the canonical scheme.
func TestHTTPExtractor_ClientAlias_ParametricPath(t *testing.T) {
	src := []byte("async function del(id) {\n" +
		"  await apiDelete(`/users/${id}`);\n" +
		"}\n")
	nodes := makeNodes("api.ts", []struct {
		name       string
		start, end int
	}{
		{"del", 1, 3},
	})

	ext := &HTTPExtractor{ClientAliases: []string{"apiDelete"}}
	contracts := ext.Extract("api.ts", src, nodes, nil)

	byID := map[string]Contract{}
	for _, c := range contracts {
		byID[c.ID] = c
	}
	c, ok := byID["http::DELETE::/users/{p1}"]
	if !ok {
		t.Fatalf("missing http::DELETE::/users/{p1}; have: %v", keysOf(byID))
	}
	if c.Meta["path"] != "/users/{p1}" {
		t.Errorf("path = %v, want /users/{p1}", c.Meta["path"])
	}
}

// TestHTTPExtractor_ClientAlias_WholeIdentifierOnly guards against an
// alias matching a longer identifier it's a suffix of: alias "apiGet"
// must not fire on a call to "myApiGet". Only the exact whole-name call
// counts.
func TestHTTPExtractor_ClientAlias_WholeIdentifierOnly(t *testing.T) {
	src := []byte(`
function f() {
  myApiGet('/should-not-match');
  apiGet('/should-match');
}
`)
	nodes := makeNodes("api.ts", []struct {
		name       string
		start, end int
	}{
		{"f", 2, 5},
	})

	ext := &HTTPExtractor{ClientAliases: []string{"apiGet"}}
	contracts := ext.Extract("api.ts", src, nodes, nil)

	byID := map[string]Contract{}
	for _, c := range contracts {
		byID[c.ID] = c
	}
	if _, ok := byID["http::GET::/should-not-match"]; ok {
		t.Errorf("alias apiGet wrongly matched myApiGet; have: %v", keysOf(byID))
	}
	if _, ok := byID["http::GET::/should-match"]; !ok {
		t.Errorf("alias apiGet missed the exact call; have: %v", keysOf(byID))
	}
}

// TestHTTPExtractor_ClientAlias_Disabled confirms the alias pass is a
// no-op when no aliases are configured — existing detection behaviour is
// unchanged for a plain non-fetch/non-axios wrapper call.
func TestHTTPExtractor_ClientAlias_Disabled(t *testing.T) {
	src := []byte(`
function f() {
  apiGet('/users');
}
`)
	nodes := makeNodes("api.ts", []struct {
		name       string
		start, end int
	}{
		{"f", 2, 4},
	})

	ext := &HTTPExtractor{} // no ClientAliases
	contracts := ext.Extract("api.ts", src, nodes, nil)
	for _, c := range contracts {
		if c.Meta["framework"] == "client-alias" {
			t.Errorf("alias pass ran with no aliases configured: %+v", c)
		}
	}
}

// TestHTTPExtractor_ClientAlias_PairsWithProvider is the end-to-end pin:
// an alias-derived consumer in one repo pairs with a real provider in a
// sibling repo because both mint the same http::GET::/users ID.
func TestHTTPExtractor_ClientAlias_PairsWithProvider(t *testing.T) {
	providerSrc := []byte(`package main

import "net/http"

func wire(mux *http.ServeMux, h *Handler) {
	mux.HandleFunc("GET /users", h.ListUsers)
}
`)
	providerNodes := makeNodes("server.go", []struct {
		name       string
		start, end int
	}{
		{"wire", 5, 7},
	})

	consumerSrc := []byte(`
async function loadUsers() {
  return apiGet('/users');
}
`)
	consumerNodes := makeNodes("api.ts", []struct {
		name       string
		start, end int
	}{
		{"loadUsers", 2, 4},
	})

	prov := (&HTTPExtractor{}).Extract("server.go", providerSrc, providerNodes, nil)
	cons := (&HTTPExtractor{ClientAliases: []string{"apiGet"}}).Extract("api.ts", consumerSrc, consumerNodes, nil)

	reg := NewRegistry()
	for _, c := range prov {
		c.RepoPrefix, c.WorkspaceID, c.ProjectID = "core", "ws", "ws"
		reg.Add(c)
	}
	for _, c := range cons {
		c.RepoPrefix, c.WorkspaceID, c.ProjectID = "web", "ws", "ws"
		reg.Add(c)
	}

	result := Match(reg)
	var paired *CrossLink
	for i, m := range result.Matched {
		if m.ContractID == "http::GET::/users" {
			paired = &result.Matched[i]
			break
		}
	}
	if paired == nil {
		t.Fatalf("expected http::GET::/users pair; matched=%d orphan_p=%d orphan_c=%d",
			len(result.Matched), len(result.OrphanProviders), len(result.OrphanConsumers))
	}
	if !paired.CrossRepo {
		t.Errorf("expected CrossRepo=true (provider core, consumer web)")
	}
}
