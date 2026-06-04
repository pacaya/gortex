package contracts

import (
	"testing"
)

// srcMap drives JoinRouterPrefixes directly: a file-path -> source map
// standing in for the indexer's disk reader. The pass is exercised
// without any graph / disk dependency.
type srcMap map[string]string

func (m srcMap) reader() func(string) []byte {
	return func(p string) []byte {
		s, ok := m[p]
		if !ok {
			return nil
		}
		return []byte(s)
	}
}

// paths returns every file path in the map — the scan-file universe
// JoinRouterPrefixes consumes (mount files included, which carry no
// route contracts).
func (m srcMap) paths() []string {
	out := make([]string, 0, len(m))
	for p := range m {
		out = append(out, p)
	}
	return out
}

// extractInto runs the HTTP extractor over each file in the map and adds
// every contract to reg with the given repo/workspace scope, so the
// contracts carry real framework meta and normalised paths.
func extractInto(t *testing.T, reg *Registry, files srcMap, repo, ws string) {
	t.Helper()
	ext := &HTTPExtractor{}
	for path, src := range files {
		for _, c := range ext.Extract(path, []byte(src), nil, nil) {
			c.RepoPrefix = repo
			c.WorkspaceID = ws
			c.ProjectID = ws
			reg.Add(c)
		}
	}
}

func idSet(reg *Registry) map[string]bool {
	out := map[string]bool{}
	for _, c := range reg.All() {
		out[c.ID] = true
	}
	return out
}

// TestJoinRouterPrefixes_FastAPI_CrossFile is the primary target: a
// router declared with its own prefix in users.py, mounted under a
// second prefix in main.py. The route @router.get("/{id}") must land at
// http::GET::/api/users/{p1}.
func TestJoinRouterPrefixes_FastAPI_CrossFile(t *testing.T) {
	files := srcMap{
		"users.py": `
from fastapi import APIRouter

router = APIRouter(prefix="/users")

@router.get("/{id}")
def get_user(id: int):
    return id

@router.get("/")
def list_users():
    return []
`,
		"main.py": `
from fastapi import FastAPI
from .users import router

app = FastAPI()
app.include_router(router, prefix="/api")
`,
	}

	reg := NewRegistry()
	extractInto(t, reg, files, "svc", "svc")

	JoinRouterPrefixes(reg, files.paths(), files.reader())

	ids := idSet(reg)
	if !ids["http::GET::/api/users/{p1}"] {
		t.Errorf("expected joined provider http::GET::/api/users/{p1}; got %v", keysOfBool(ids))
	}
	if !ids["http::GET::/api/users"] {
		t.Errorf("expected joined provider http::GET::/api/users (from /); got %v", keysOfBool(ids))
	}
	// The un-joined originals must be gone.
	if ids["http::GET::/{p1}"] {
		t.Errorf("un-joined original http::GET::/{p1} still present: %v", keysOfBool(ids))
	}
}

// TestJoinRouterPrefixes_FastAPI_Idempotent runs the pass twice; the
// second run must be a no-op (no double-join to /api/api/users).
func TestJoinRouterPrefixes_FastAPI_Idempotent(t *testing.T) {
	files := srcMap{
		"users.py": `router = APIRouter(prefix="/users")

@router.get("/{id}")
def get_user(id): return id
`,
		"main.py": `app.include_router(router, prefix="/api")`,
	}
	reg := NewRegistry()
	extractInto(t, reg, files, "svc", "svc")

	JoinRouterPrefixes(reg, files.paths(), files.reader())
	JoinRouterPrefixes(reg, files.paths(), files.reader())

	ids := idSet(reg)
	if !ids["http::GET::/api/users/{p1}"] {
		t.Errorf("expected http::GET::/api/users/{p1} after double run; got %v", keysOfBool(ids))
	}
	if ids["http::GET::/api/api/users/{p1}"] {
		t.Errorf("double-join detected: %v", keysOfBool(ids))
	}
}

// TestJoinRouterPrefixes_FastAPI_NestedIncludes chains prefixes: a
// sub-router is included into a parent router, which is itself included
// into the app. /v2 (app mount) + /admin (parent self) ... actually the
// chain is app.include_router(parent, prefix="/api") and
// parent.include_router(child, prefix="/admin"), child=APIRouter(prefix="/users").
// Route @child.get("/{id}") -> /api/admin/users/{id}.
func TestJoinRouterPrefixes_FastAPI_NestedIncludes(t *testing.T) {
	files := srcMap{
		"users.py": `child = APIRouter(prefix="/users")

@child.get("/{id}")
def get_user(id): return id
`,
		"admin.py": `parent = APIRouter()
parent.include_router(child, prefix="/admin")
`,
		"main.py": `app.include_router(parent, prefix="/api")`,
	}
	reg := NewRegistry()
	extractInto(t, reg, files, "svc", "svc")

	JoinRouterPrefixes(reg, files.paths(), files.reader())

	ids := idSet(reg)
	if !ids["http::GET::/api/admin/users/{p1}"] {
		t.Errorf("expected chained http::GET::/api/admin/users/{p1}; got %v", keysOfBool(ids))
	}
}

// TestJoinRouterPrefixes_FastAPI_NoPrefix: a router with no self-prefix
// mounted with no prefix is a no-op join — the ID stays as declared.
func TestJoinRouterPrefixes_FastAPI_NoPrefix(t *testing.T) {
	files := srcMap{
		"users.py": `router = APIRouter()

@router.get("/users/{id}")
def get_user(id): return id
`,
		"main.py": `app.include_router(router)`,
	}
	reg := NewRegistry()
	extractInto(t, reg, files, "svc", "svc")

	JoinRouterPrefixes(reg, files.paths(), files.reader())

	ids := idSet(reg)
	if !ids["http::GET::/users/{p1}"] {
		t.Errorf("no-prefix join should leave http::GET::/users/{p1}; got %v", keysOfBool(ids))
	}
}

// TestJoinRouterPrefixes_FastAPI_SelfPrefixOnly: router carries a
// prefix but is never mounted under another (or mounted with no
// prefix). The self-prefix still applies.
func TestJoinRouterPrefixes_FastAPI_SelfPrefixOnly(t *testing.T) {
	files := srcMap{
		"users.py": `router = APIRouter(prefix="/users")

@router.get("/{id}")
def get_user(id): return id
`,
		"main.py": `app.include_router(router)`,
	}
	reg := NewRegistry()
	extractInto(t, reg, files, "svc", "svc")

	JoinRouterPrefixes(reg, files.paths(), files.reader())

	ids := idSet(reg)
	if !ids["http::GET::/users/{p1}"] {
		t.Errorf("self-prefix-only should yield http::GET::/users/{p1}; got %v", keysOfBool(ids))
	}
}

// TestJoinRouterPrefixes_ConsumerPairsViaMatch is the end-to-end pin:
// after the join, a consumer calling /api/users/42 (full path) pairs
// with the rewritten provider via Match.
func TestJoinRouterPrefixes_ConsumerPairsViaMatch(t *testing.T) {
	providerFiles := srcMap{
		"users.py": `router = APIRouter(prefix="/users")

@router.get("/{id}")
def get_user(id): return id
`,
		"main.py": `app.include_router(router, prefix="/api")`,
	}
	reg := NewRegistry()
	extractInto(t, reg, providerFiles, "api", "shop")

	// Consumer in a sibling repo of the same workspace calls the full
	// joined path. Consumer paths are never prefix-joined (they already
	// carry the mount prefix), so this is added as-is.
	consumerSrc := `async function getUser(id) {
  return fetch(` + "`/api/users/${id}`" + `);
}
`
	ext := &HTTPExtractor{}
	for _, c := range ext.Extract("api.ts", []byte(consumerSrc), nil, nil) {
		c.RepoPrefix = "web"
		c.WorkspaceID = "shop"
		c.ProjectID = "shop"
		reg.Add(c)
	}

	JoinRouterPrefixes(reg, providerFiles.paths(), providerFiles.reader())

	result := Match(reg)
	var paired *CrossLink
	for i, m := range result.Matched {
		if m.ContractID == "http::GET::/api/users/{p1}" {
			paired = &result.Matched[i]
			break
		}
	}
	if paired == nil {
		t.Fatalf("expected http::GET::/api/users/{p1} pair; matched=%d orphan_prov=%d orphan_cons=%d",
			len(result.Matched), len(result.OrphanProviders), len(result.OrphanConsumers))
	}
	if !paired.CrossRepo {
		t.Errorf("expected CrossRepo=true (provider api, consumer web)")
	}
	if paired.Provider.RepoPrefix != "api" || paired.Consumer.RepoPrefix != "web" {
		t.Errorf("wrong wiring: provider=%s consumer=%s",
			paired.Provider.RepoPrefix, paired.Consumer.RepoPrefix)
	}
}

// TestJoinRouterPrefixes_Express joins an app.use('/api', router) mount
// onto a route declared on that router in a sibling file.
func TestJoinRouterPrefixes_Express(t *testing.T) {
	files := srcMap{
		"routes.ts": `
const router = express.Router();
router.get('/users/:id', getUser);
`,
		"app.ts": `
const app = express();
app.use('/api', router);
`,
	}
	reg := NewRegistry()
	extractInto(t, reg, files, "svc", "svc")

	JoinRouterPrefixes(reg, files.paths(), files.reader())

	ids := idSet(reg)
	if !ids["http::GET::/api/users/{p1}"] {
		t.Errorf("expected express-joined http::GET::/api/users/{p1}; got %v", keysOfBool(ids))
	}
}

// TestJoinRouterPrefixes_NestJS joins a @Controller('cats') class
// prefix onto the class's @Get(':id') method route (single file).
func TestJoinRouterPrefixes_NestJS(t *testing.T) {
	files := srcMap{
		"cats.controller.ts": `
@Controller('cats')
export class CatsController {
  @Get(':id')
  findOne(@Param('id') id: string) {
    return id;
  }

  @Post('bulk')
  createBulk() {
    return 'created';
  }
}
`,
	}
	reg := NewRegistry()
	extractInto(t, reg, files, "svc", "svc")

	JoinRouterPrefixes(reg, files.paths(), files.reader())

	ids := idSet(reg)
	if !ids["http::GET::/cats/{p1}"] {
		t.Errorf("expected nestjs-joined http::GET::/cats/{p1}; got %v", keysOfBool(ids))
	}
	if !ids["http::POST::/cats/bulk"] {
		t.Errorf("expected nestjs-joined http::POST::/cats/bulk; got %v", keysOfBool(ids))
	}
}

// keysOfBool returns the keys of a string->bool set for diagnostics.
func keysOfBool(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
