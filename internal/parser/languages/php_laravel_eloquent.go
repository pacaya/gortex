package languages

import "strings"

// Eloquent static finder/query methods. A Laravel model's static call
// (`User::find(1)`, `User::where(...)`, `Post::create([...])`) dispatches to
// the query-builder inherited from Illuminate\Database\Eloquent\Model, so the
// concrete method is not declared on the model class and the call would go
// unresolved. This allowlist lets the extractor bind such a call to the model
// class node instead — a far more useful target than a name-only `find`
// match. Kept beside phpLaravelFacadesDefault as a plain table.
var phpEloquentStaticMethods = map[string]bool{
	"find": true, "findOrFail": true, "findOr": true, "findMany": true,
	"where": true, "whereIn": true, "whereNotIn": true, "whereHas": true,
	"all": true, "get": true, "first": true, "firstOrFail": true,
	"firstOrCreate": true, "firstOrNew": true, "firstWhere": true,
	"create": true, "make": true, "forceCreate": true,
	"updateOrCreate": true, "insert": true, "query": true, "with": true,
	"orderBy": true, "latest": true, "oldest": true, "count": true,
	"pluck": true, "destroy": true, "paginate": true, "exists": true,
}

// phpEloquentModelCall reports whether a `Scope::method(...)` static call is
// an Eloquent finder on a model — a PascalCase class name plus an
// allowlisted query method. Returns the bare model class name on a match.
func phpEloquentModelCall(scope, method string) (string, bool) {
	if !phpEloquentStaticMethods[method] {
		return "", false
	}
	model := strings.TrimPrefix(scope, "\\")
	if i := strings.LastIndex(model, "\\"); i >= 0 {
		model = model[i+1:]
	}
	if model == "" || model[0] < 'A' || model[0] > 'Z' {
		return "", false
	}
	return model, true
}
