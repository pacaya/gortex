package contracts

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// method returns the contract's HTTP method for terse assertions.
func contractMethod(c Contract) string {
	m, _ := c.Meta["method"].(string)
	return m
}

// providerByMethodPath indexes provider contracts by "METHOD path" for lookup.
func providerByMethodPath(cs []Contract) map[string]Contract {
	out := map[string]Contract{}
	for _, c := range cs {
		if c.Role != RoleProvider {
			continue
		}
		p, _ := c.Meta["path"].(string)
		out[contractMethod(c)+" "+p] = c
	}
	return out
}

// TestLaravelRailsSpringAxumHandlerBinding proves the backend route handlers
// bind to their controller-method symbols — receiver-precise — across the four
// dominant backend stacks. The controller nodes are supplied in scope (in
// production, same-file frameworks resolve directly and cross-file route files
// are bound by the indexer's module-wide pass via the stamped handler_class);
// the receiver-aware resolution is the beat over a name-only same-file regex:
// `[UserController::class, 'index']` binds to UserController.index even when a
// decoy AdminController.index shares the action name.
func TestLaravelRailsSpringAxumHandlerBinding(t *testing.T) {
	h := &HTTPExtractor{}

	t.Run("laravel", func(t *testing.T) {
		const rf = "routes/web.php"
		src := "<?php\n" +
			"Route::get('/users', [UserController::class, 'index']);\n" +
			"Route::post('/users', 'UserController@store');\n"
		nodes := []*graph.Node{
			{ID: "ctrl::UserController.index", Kind: graph.KindMethod, Name: "index", FilePath: rf, Meta: map[string]any{"receiver": "UserController"}},
			{ID: "ctrl::AdminController.index", Kind: graph.KindMethod, Name: "index", FilePath: rf, Meta: map[string]any{"receiver": "AdminController"}}, // decoy
			{ID: "ctrl::UserController.store", Kind: graph.KindMethod, Name: "store", FilePath: rf, Meta: map[string]any{"receiver": "UserController"}},
		}
		got := providerByMethodPath(h.Extract(rf, []byte(src), nodes, nil))
		if c := got["GET /users"]; c.SymbolID != "ctrl::UserController.index" {
			t.Errorf("Laravel array action: SymbolID=%q, want UserController.index (receiver-correct, not the AdminController decoy)", c.SymbolID)
		}
		if c := got["GET /users"]; c.Meta["handler_class"] != "UserController" {
			t.Errorf("Laravel: handler_class=%v, want UserController", c.Meta["handler_class"])
		}
		if c := got["POST /users"]; c.SymbolID != "ctrl::UserController.store" {
			t.Errorf("Laravel string action: SymbolID=%q, want UserController.store", c.SymbolID)
		}
	})

	t.Run("rails", func(t *testing.T) {
		const cf = "config/routes.rb"
		src := "Rails.application.routes.draw do\n" +
			"  get '/users', to: 'users#index'\n" +
			"  post '/sessions' => 'sessions#create'\n" +
			"end\n"
		nodes := []*graph.Node{
			{ID: "u::UsersController.index", Kind: graph.KindMethod, Name: "index", FilePath: cf, Meta: map[string]any{"receiver": "UsersController"}},
			{ID: "s::SessionsController.create", Kind: graph.KindMethod, Name: "create", FilePath: cf, Meta: map[string]any{"receiver": "SessionsController"}},
		}
		got := providerByMethodPath(h.Extract(cf, []byte(src), nodes, nil))
		if c := got["GET /users"]; c.SymbolID != "u::UsersController.index" {
			t.Errorf("Rails to: action: SymbolID=%q, want UsersController.index (camelized controller)", c.SymbolID)
		}
		if c := got["POST /sessions"]; c.SymbolID != "s::SessionsController.create" {
			t.Errorf("Rails => action: SymbolID=%q, want SessionsController.create", c.SymbolID)
		}
	})

	t.Run("spring", func(t *testing.T) {
		const sf = "UserController.java"
		// The @GetMapping annotation sits above its handler method (same file);
		// the forward-scan binds it.
		src := "class UserController {\n" +
			"  @GetMapping(\"/users\")\n" +
			"  @ResponseBody\n" +
			"  public List<User> getUsers() { return null; }\n" +
			"}\n"
		nodes := []*graph.Node{
			{ID: "s::UserController.getUsers", Kind: graph.KindMethod, Name: "getUsers", FilePath: sf, Meta: map[string]any{"receiver": "UserController"}},
		}
		got := providerByMethodPath(h.Extract(sf, []byte(src), nodes, nil))
		if c := got["GET /users"]; c.SymbolID != "s::UserController.getUsers" {
			t.Errorf("Spring @GetMapping: SymbolID=%q, want getUsers (forward-scan past stacked annotation)", c.SymbolID)
		}
	})

	t.Run("axum", func(t *testing.T) {
		const af = "main.rs"
		src := "fn router() -> Router {\n" +
			"    Router::new().route(\"/users\", get(list_users))\n" +
			"}\n" +
			"async fn list_users() {}\n"
		nodes := []*graph.Node{
			{ID: "a::list_users", Kind: graph.KindFunction, Name: "list_users", FilePath: af},
		}
		got := providerByMethodPath(h.Extract(af, []byte(src), nodes, nil))
		if c := got["GET /users"]; c.SymbolID != "a::list_users" {
			t.Errorf("Axum .route handler: SymbolID=%q, want list_users", c.SymbolID)
		}
	})
}

// TestRailsResourcesExpansion proves a single `resources` declaration expands
// to the canonical RESTful seven routes — implicit routes a path-only regex
// extractor never recovers — each bound to its controller action, and that
// only:/except: filters trim the set.
func TestRailsResourcesExpansion(t *testing.T) {
	h := &HTTPExtractor{}
	const cf = "config/routes.rb"

	actions := []string{"index", "create", "new", "edit", "show", "update", "destroy"}
	nodes := make([]*graph.Node, 0, len(actions))
	for _, a := range actions {
		nodes = append(nodes, &graph.Node{
			ID: "p::PhotosController." + a, Kind: graph.KindMethod, Name: a,
			FilePath: cf, Meta: map[string]any{"receiver": "PhotosController"},
		})
	}

	t.Run("full_seven", func(t *testing.T) {
		src := "Rails.application.routes.draw do\n  resources :photos\nend\n"
		got := providerByMethodPath(h.Extract(cf, []byte(src), nodes, nil))
		want := map[string]string{
			"GET /photos":          "index",
			"POST /photos":         "create",
			"GET /photos/new":      "new",
			"GET /photos/{p1}/edit": "edit",
			"GET /photos/{p1}":     "show",
			"PATCH /photos/{p1}":   "update",
			"DELETE /photos/{p1}":  "destroy",
		}
		if len(got) != len(want) {
			t.Fatalf("resources :photos produced %d routes, want %d: %v", len(got), len(want), keysOf(got))
		}
		for mp, action := range want {
			c, ok := got[mp]
			if !ok {
				t.Errorf("missing RESTful route %q", mp)
				continue
			}
			if c.SymbolID != "p::PhotosController."+action {
				t.Errorf("route %q bound to %q, want PhotosController.%s", mp, c.SymbolID, action)
			}
		}
	})

	t.Run("only_filter", func(t *testing.T) {
		src := "Rails.application.routes.draw do\n  resources :photos, only: [:index, :show]\nend\n"
		got := providerByMethodPath(h.Extract(cf, []byte(src), nodes, nil))
		if len(got) != 2 {
			t.Fatalf("only: [:index, :show] produced %d routes, want 2: %v", len(got), keysOf(got))
		}
		if _, ok := got["GET /photos"]; !ok {
			t.Error("only: filter dropped the index route")
		}
		if _, ok := got["GET /photos/{p1}"]; !ok {
			t.Error("only: filter dropped the show route")
		}
	})

	t.Run("except_filter", func(t *testing.T) {
		src := "Rails.application.routes.draw do\n  resources :photos, except: [:destroy]\nend\n"
		got := providerByMethodPath(h.Extract(cf, []byte(src), nodes, nil))
		if len(got) != 6 {
			t.Fatalf("except: [:destroy] produced %d routes, want 6: %v", len(got), keysOf(got))
		}
		if _, ok := got["DELETE /photos/{p1}"]; ok {
			t.Error("except: [:destroy] should have dropped the destroy route")
		}
	})
}
