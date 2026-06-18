package contracts

import (
	"fmt"
	"regexp"
	"strings"
)

// Cross-file route-prefix joining for HTTP contract IDs.
//
// A sub-router is frequently declared in one file and *mounted* under a
// path prefix in another. The route the sub-router declares should be
// recorded with the joined path so a provider and a consumer agree on
// the same canonical contract ID:
//
//	# users.py
//	router = APIRouter(prefix="/users")
//	@router.get("/{id}")            # declared path: /{id}
//	def get_user(id): ...
//
//	# main.py
//	app.include_router(router, prefix="/api")
//
// Without this pass the provider emits http::GET::/{p1}; the consumer
// calling /api/users/42 emits http::GET::/api/users/{p1}; the two never
// pair. JoinRouterPrefixes rewrites the provider to
// http::GET::/api/users/{p1} so the matcher sees the same ID on both
// sides.
//
// Coverage:
//   - FastAPI (full): APIRouter(prefix=...) self-prefix + cross-file
//     include_router(var, prefix=...) mounts, including nested includes
//     (a router included into an already-included router → prefixes
//     chain) and a missing prefix (no-op join).
//   - Express (partial): app.use('/api', router) mounts joined onto
//     routes declared on a receiver named app/router. Limited by the
//     existing express regex, which only recognises those two receiver
//     names.
//   - NestJS (partial): @Controller('cats') class prefix joined onto
//     the class's @Get('x') routes (single-file). Cross-module
//     RouterModule.register prefixes are out of scope.
//
// The pass is idempotent: a contract already stamped joined is skipped,
// so a re-run (incremental reindex) never double-joins.

// routePrefixJoinedMeta marks a contract whose path/ID this pass has
// already rewritten, so a second run is a no-op.
const routePrefixJoinedMeta = "route_prefix_joined"

// Frameworks whose routes participate in prefix joining. The HTTP
// extractor stamps Meta["framework"] with these values.
var routePrefixFrameworks = map[string]bool{
	"fastapi/flask": true, // FastAPI APIRouter / include_router
	"flask":         true, // Flask Blueprint url_prefix
	"express":       true, // Express app.use mounts
	"nestjs":        true, // NestJS @Controller class prefix
	"gin/echo/chi":  true, // gin RouterGroup .Group chains
	"spring":        true, // Spring class-level @RequestMapping
	"rails":         true, // Rails namespace blocks
	"laravel":       true, // Laravel Route::prefix(...)->group
	"axum":          true, // axum Router .nest mounts
	"fiber":         true, // gin/echo uppercase verbs label as fiber; same .Group model
}

// --- FastAPI / Python -------------------------------------------------------

// pyRouterDefRE matches a router variable bound to an APIRouter that
// carries its own prefix, e.g. `router = APIRouter(prefix="/users")` or
// `users_router = APIRouter(prefix='/users', tags=[...])`. Group 1 is
// the variable name, group 2 the prefix.
var pyRouterDefRE = regexp.MustCompile(`(\w+)\s*=\s*APIRouter\(([^)]*)\)`)

// pyIncludeRouterRE matches a mount site, e.g.
// `app.include_router(router, prefix="/api")` or
// `parent.include_router(users.router, prefix='/api')`. Group 1 is the
// mounted router reference (possibly attribute-qualified), group 2 the
// argument list (scanned for prefix=).
var pyIncludeRouterRE = regexp.MustCompile(`\binclude_router\(\s*([\w.]+)\s*((?:,[^)]*)?)\)`)

// prefixKwargRE pulls prefix="..." out of a keyword-argument list.
var prefixKwargRE = regexp.MustCompile(`prefix\s*=\s*["']([^"']*)["']`)

// urlPrefixKwargRE pulls Flask's url_prefix="..." out of a Blueprint
// definition or register_blueprint call.
var urlPrefixKwargRE = regexp.MustCompile(`url_prefix\s*=\s*["']([^"']*)["']`)

// pyBlueprintDefRE matches a Flask Blueprint bound to a variable, e.g.
// `bp = Blueprint('api', __name__, url_prefix='/api')`. Group 1 is the
// variable, group 2 the argument list (scanned for url_prefix=).
var pyBlueprintDefRE = regexp.MustCompile(`(\w+)\s*=\s*Blueprint\(([^)]*)\)`)

// pyRegisterBlueprintRE matches a mount site, e.g.
// `app.register_blueprint(bp, url_prefix='/admin')`. Group 1 is the
// mounted blueprint reference, group 2 the argument list.
var pyRegisterBlueprintRE = regexp.MustCompile(`register_blueprint\(\s*([\w.]+)\s*((?:,[^)]*)?)\)`)

// flaskRouteReceiverRE recovers the blueprint/app variable from a Flask
// route line, e.g. `@bp.route("/users")` → "bp".
var flaskRouteReceiverRE = regexp.MustCompile(`@(\w+)\.route\(`)

// pyRouteReceiverRE recovers the decorator receiver variable from a
// FastAPI route line, e.g. `@router.get("/{id}")` → "router". Anchored
// to the start-of-line decorator form the Python provider pattern uses.
var pyRouteReceiverRE = regexp.MustCompile(`@(\w+)\.(?:get|post|put|delete|patch|head|options)\(`)

// --- Express / TS-JS --------------------------------------------------------

// jsUseMountRE matches `app.use('/api', router)` /
// `router.use("/v1", sub)`. Group 1 is the mount prefix, group 2 the
// mounted router variable. A `.use('/api', fn1, fn2, router)` form
// (middleware chain) is not handled — only the two-argument shape.
var jsUseMountRE = regexp.MustCompile(`\b(\w+)\.use\(\s*["'` + "`" + `]([^"'` + "`" + `]+)["'` + "`" + `]\s*,\s*(\w+)\s*\)`)

// jsRouteReceiverRE recovers the receiver from an express route line,
// e.g. `router.get('/users', h)` → "router". Mirrors the express
// provider pattern's (?:app|router) anchor.
var jsRouteReceiverRE = regexp.MustCompile(`\b(app|router)\.(?:get|post|put|delete|patch|head|options|all)\(`)

// --- NestJS -----------------------------------------------------------------

// nestControllerRE matches a class-level `@Controller('cats')` /
// `@Controller("/cats")` decorator. Group 1 is the class prefix. An
// argument-less `@Controller()` carries no prefix (no-op).
var nestControllerRE = regexp.MustCompile(`@Controller\(\s*["'` + "`" + `]([^"'` + "`" + `]*)["'` + "`" + `]\s*\)`)

// --- gin / Go --------------------------------------------------------------

// ginGroupRE matches a gin RouterGroup binding, e.g.
// `v1 := r.Group("/api/v1")`. Group 1 is the child var, group 2 the parent
// router, group 3 the group prefix.
var ginGroupRE = regexp.MustCompile(`(\w+)\s*:?=\s*(\w+)\.Group\(\s*"([^"]+)"`)

// ginRouteReceiverRE recovers the router-group variable a gin/echo/chi/fiber
// route is registered on, e.g. `v1.GET("/users", h)` → "v1". The verb is
// matched case-insensitively to cover gin/echo's uppercase `.GET` and
// chi's title-case `.Get`.
var ginRouteReceiverRE = regexp.MustCompile(`(\w+)\.(?i:get|post|put|delete|patch|head|options|any|handle)\(`)

// --- axum / Rust -----------------------------------------------------------

// axumNestRE matches `.nest("/api", api_router)`. Group 1 is the mount
// prefix, group 2 the nested router variable.
var axumNestRE = regexp.MustCompile(`\.nest\(\s*"([^"]+)"\s*,\s*(\w+)\s*\)`)

// axumLetBindingRE recovers the router variable a route chain is bound to,
// e.g. `let api = Router::new().route("/users", get(list));` → "api".
var axumLetBindingRE = regexp.MustCompile(`let\s+(\w+)\s*=`)

// --- Spring / Rails / Laravel block-scoped prefixes ------------------------

// springClassMappingRE matches a class-level @RequestMapping("/api").
var springClassMappingRE = regexp.MustCompile(`@RequestMapping\(\s*(?:value\s*=\s*)?["']([^"']*)["']`)

// railsNamespaceRE matches a Rails `namespace :admin do` block opener.
var railsNamespaceRE = regexp.MustCompile(`\bnamespace\s+:(\w+)\s+do\b`)

// laravelPrefixRE matches a Laravel `Route::prefix('admin')` clause.
var laravelPrefixRE = regexp.MustCompile(`Route::prefix\(\s*["']([^"']+)["']\s*\)`)

// JoinRouterPrefixes rewrites HTTP provider/consumer contract paths and
// IDs to include the prefix at their router's mount site.
//
// scanFiles is the universe of source files to scan for router
// definitions and mount sites (APIRouter(prefix=...), include_router,
// app.use). This must include the *mount* files, which frequently carry
// no route contracts of their own (a FastAPI main.py with only
// include_router calls) — so the file set cannot be derived from the
// contract registry alone. The route contracts being rewritten still
// come from reg.
//
// srcFor returns the raw source bytes for a file path (as stored on the
// contract / in scanFiles — i.e. repo-prefixed when the indexer uses a
// repo prefix); returning nil for a path skips files that can't be read.
// The pass mutates the registry in place via ReplaceByID.
func JoinRouterPrefixes(reg *Registry, scanFiles []string, srcFor func(filePath string) []byte) {
	if reg == nil || srcFor == nil {
		return
	}

	// Per-file router facts, keyed by file path. Parsed once per file.
	type fileFacts struct {
		// selfPrefix: router var -> the prefix it declares on itself
		// (APIRouter(prefix=...)). FastAPI only.
		selfPrefix map[string]string
		// mountPrefix: router var -> the prefix it is mounted under at
		// an include_router / app.use site (may live in another file).
		mountPrefix map[string]string
		// mountChild: parent router var -> child router var it mounts,
		// so nested includes can chain prefixes.
		mountParent map[string]string
	}

	files := make(map[string]*fileFacts)
	parseFile := func(filePath string) *fileFacts {
		if f, ok := files[filePath]; ok {
			return f
		}
		f := &fileFacts{
			selfPrefix:  map[string]string{},
			mountPrefix: map[string]string{},
			mountParent: map[string]string{},
		}
		files[filePath] = f
		src := srcFor(filePath)
		if src == nil {
			return f
		}
		text := string(src)

		// FastAPI: router = APIRouter(prefix="/users")
		for _, m := range pyRouterDefRE.FindAllStringSubmatch(text, -1) {
			varName := m[1]
			if pm := prefixKwargRE.FindStringSubmatch(m[2]); pm != nil {
				f.selfPrefix[varName] = cleanPrefix(pm[1])
			} else {
				// APIRouter() with no prefix still registers the var so
				// a later mount join knows it exists (no-op self-prefix).
				if _, ok := f.selfPrefix[varName]; !ok {
					f.selfPrefix[varName] = ""
				}
			}
		}

		// FastAPI: app.include_router(router, prefix="/api")
		for _, m := range pyIncludeRouterRE.FindAllStringSubmatch(text, -1) {
			child := lastAttr(m[1])
			prefix := ""
			if pm := prefixKwargRE.FindStringSubmatch(m[2]); pm != nil {
				prefix = cleanPrefix(pm[1])
			}
			// A router can be mounted once; if seen twice keep the
			// first (deterministic).
			if _, ok := f.mountPrefix[child]; !ok {
				f.mountPrefix[child] = prefix
			}
			// Record the mount receiver as the parent so nested
			// includes chain (parent.include_router(child, ...)).
			parent := receiverOfInclude(text, m[0])
			if parent != "" && parent != child {
				f.mountParent[child] = parent
			}
		}

		// Flask: bp = Blueprint('api', __name__, url_prefix='/api')
		for _, m := range pyBlueprintDefRE.FindAllStringSubmatch(text, -1) {
			varName := m[1]
			if pm := urlPrefixKwargRE.FindStringSubmatch(m[2]); pm != nil {
				f.selfPrefix[varName] = cleanPrefix(pm[1])
			} else if _, ok := f.selfPrefix[varName]; !ok {
				f.selfPrefix[varName] = ""
			}
		}

		// Flask: app.register_blueprint(bp, url_prefix='/admin')
		for _, m := range pyRegisterBlueprintRE.FindAllStringSubmatch(text, -1) {
			child := lastAttr(m[1])
			prefix := ""
			if pm := urlPrefixKwargRE.FindStringSubmatch(m[2]); pm != nil {
				prefix = cleanPrefix(pm[1])
			}
			if _, ok := f.mountPrefix[child]; !ok {
				f.mountPrefix[child] = prefix
			}
		}

		// gin: v1 := r.Group("/api/v1"); admin := v1.Group("/admin")
		for _, m := range ginGroupRE.FindAllStringSubmatch(text, -1) {
			child, parent, prefix := m[1], m[2], cleanPrefix(m[3])
			f.selfPrefix[child] = prefix
			if parent != "" && parent != child {
				f.mountParent[child] = parent
			}
		}

		// axum: app.nest("/api", api_router)
		for _, m := range axumNestRE.FindAllStringSubmatch(text, -1) {
			prefix, child := cleanPrefix(m[1]), m[2]
			if _, ok := f.mountPrefix[child]; !ok {
				f.mountPrefix[child] = prefix
			}
		}

		// Express: app.use('/api', router)
		for _, m := range jsUseMountRE.FindAllStringSubmatch(text, -1) {
			parent, prefix, child := m[1], cleanPrefix(m[2]), m[3]
			if _, ok := f.mountPrefix[child]; !ok {
				f.mountPrefix[child] = prefix
			}
			if parent != "" && parent != child {
				f.mountParent[child] = parent
			}
		}

		return f
	}

	// Global maps merged across every contract file. Router var names
	// commonly collide ("router"), so cross-file resolution is
	// best-effort within a workspace: when exactly one file declares a
	// self-prefix or mount for a var, use it. This favours the common
	// monorepo / single-service layout the feature targets.
	globalSelf := map[string]string{}   // var -> self prefix (FastAPI)
	globalMount := map[string]string{}  // var -> mount prefix
	globalParent := map[string]string{} // var -> parent router var
	selfConflict := map[string]bool{}   // var seen with >1 distinct self prefix
	mountConflict := map[string]bool{}  // var seen with >1 distinct mount prefix

	// First pass: parse every scan file plus every route-contract file
	// (union) and merge router facts. Mount files (a FastAPI main.py
	// that only calls include_router) carry no route contracts, so they
	// must be reached via scanFiles; route-contract files supply the
	// APIRouter(prefix=...) self-prefixes.
	factFiles := make(map[string]bool, len(scanFiles))
	for _, p := range scanFiles {
		factFiles[p] = true
	}
	for _, c := range reg.All() {
		if c.Type != ContractHTTP {
			continue
		}
		fw, _ := c.Meta["framework"].(string)
		if !routePrefixFrameworks[fw] {
			continue
		}
		factFiles[c.FilePath] = true
	}
	for path := range factFiles {
		f := parseFile(path)
		for v, p := range f.selfPrefix {
			if existing, ok := globalSelf[v]; ok {
				if existing != p && existing != "" && p != "" {
					selfConflict[v] = true
				}
				if existing == "" && p != "" {
					globalSelf[v] = p
				}
			} else {
				globalSelf[v] = p
			}
		}
		for v, p := range f.mountPrefix {
			if existing, ok := globalMount[v]; ok {
				if existing != p {
					mountConflict[v] = true
				}
			} else {
				globalMount[v] = p
			}
		}
		for child, parent := range f.mountParent {
			if _, ok := globalParent[child]; !ok {
				globalParent[child] = parent
			}
		}
	}

	// chainPrefix resolves the full mount prefix for a router var,
	// walking parent includes so a router mounted into a router
	// inherits both prefixes. Self-prefix is the router's own
	// APIRouter(prefix=...); it is prepended by the per-route join, not
	// here, because only the route's owning router contributes its self
	// prefix.
	var chainPrefix func(varName string, seen map[string]bool) string
	chainPrefix = func(varName string, seen map[string]bool) string {
		if seen[varName] {
			return "" // cycle guard
		}
		seen[varName] = true
		mount := ""
		if mp, ok := globalMount[varName]; ok && !mountConflict[varName] {
			mount = mp
		}
		// The parent router's own mount prefix prepends this one. The
		// parent's *self* prefix also applies, since the parent is a
		// real router whose routes sit under parentSelf+parentMount.
		if parent, ok := globalParent[varName]; ok {
			parentChain := chainPrefix(parent, seen)
			parentSelf := ""
			if ps, ok := globalSelf[parent]; ok && !selfConflict[parent] {
				parentSelf = ps
			}
			return joinPaths(parentChain, parentSelf, mount)
		}
		return mount
	}

	// Second pass: rewrite each route contract.
	type rewrite struct {
		oldID string
		list  []Contract
	}
	var rewrites []rewrite
	processed := map[string]bool{} // contract ID -> already collected

	for _, c := range reg.All() {
		if c.Type != ContractHTTP || c.Meta == nil {
			continue
		}
		fw, _ := c.Meta["framework"].(string)
		if !routePrefixFrameworks[fw] {
			continue
		}
		// Idempotency: skip contracts already joined.
		if joined, _ := c.Meta[routePrefixJoinedMeta].(bool); joined {
			continue
		}
		if processed[c.ID] {
			continue
		}

		prefix := prefixForRoute(c, fw, srcFor, globalSelf, globalMount, selfConflict, mountConflict, chainPrefix)
		if prefix == "" {
			continue
		}

		// Rewrite every contract entry recorded for this ID (provider
		// and any same-ID consumer) consistently.
		items := reg.ByID(c.ID)
		changed := false
		for i := range items {
			if items[i].Type != ContractHTTP || items[i].Meta == nil {
				continue
			}
			ifw, _ := items[i].Meta["framework"].(string)
			if !routePrefixFrameworks[ifw] {
				continue
			}
			if joined, _ := items[i].Meta[routePrefixJoinedMeta].(bool); joined {
				continue
			}
			rp := prefixForRoute(items[i], ifw, srcFor, globalSelf, globalMount, selfConflict, mountConflict, chainPrefix)
			if rp == "" {
				continue
			}
			rawPath, _ := items[i].Meta["path"].(string)
			joinedRaw := joinPaths(rp, rawPath)
			newPath, names := NormalizeHTTPPathWithParams(joinedRaw)
			method, _ := items[i].Meta["method"].(string)
			if method == "" {
				method = "ANY"
			}
			newID := fmt.Sprintf("http::%s::%s", method, newPath)
			// Clone the Meta map before mutating: ByID returns value
			// copies whose Meta maps still alias the registry's other
			// secondary indexes (maps are reference types). Mutating in
			// place would leak the rewrite into stale index entries
			// before ReplaceByID purges them.
			items[i].Meta = cloneMeta(items[i].Meta)
			if newID == items[i].ID {
				// Already canonical (prefix was empty after norm).
				items[i].Meta[routePrefixJoinedMeta] = true
				changed = true
				continue
			}
			items[i].ID = newID
			items[i].Meta["path"] = newPath
			if len(names) > 0 {
				items[i].Meta["path_param_names"] = names
			}
			items[i].Meta[routePrefixJoinedMeta] = true
			changed = true
		}
		if changed {
			processed[c.ID] = true
			rewrites = append(rewrites, rewrite{oldID: c.ID, list: items})
		}
	}

	// Apply rewrites. ReplaceByID keys the byID bucket on the OLD id,
	// so it can't be used directly when the rewrite *changes* the ID
	// (it would file the new contract under the stale key and the
	// matcher, which buckets on the byID key, would never pair it).
	// Instead clear the old bucket (purging every secondary index) and
	// re-Add each contract under its own — possibly rewritten — ID.
	for _, rw := range rewrites {
		reg.ReplaceByID(rw.oldID, nil)
		for _, c := range rw.list {
			reg.Add(c)
		}
	}
}

// prefixForRoute computes the full joined prefix (mount chain + the
// route's own router self-prefix, or NestJS controller prefix) to
// prepend to a single route contract's declared path. Returns "" when
// no prefix applies (plain join no-op).
func prefixForRoute(
	c Contract,
	framework string,
	srcFor func(filePath string) []byte,
	globalSelf, globalMount map[string]string,
	selfConflict, mountConflict map[string]bool,
	chainPrefix func(string, map[string]bool) string,
) string {
	switch framework {
	case "fastapi/flask", "flask", "express":
		varName := routeReceiver(c, framework, srcFor)
		if varName == "" {
			return ""
		}
		mount := chainPrefix(varName, map[string]bool{})
		self := ""
		if framework == "fastapi/flask" || framework == "flask" {
			if ps, ok := globalSelf[varName]; ok && !selfConflict[varName] {
				self = ps
			}
		}
		return joinPaths(mount, self)
	case "gin/echo/chi", "fiber":
		// gin RouterGroup chain: the route's group var carries its own
		// .Group prefix, parents contribute theirs via the mount chain.
		varName := routeReceiver(c, framework, srcFor)
		if varName == "" {
			return ""
		}
		mount := chainPrefix(varName, map[string]bool{})
		self := ""
		if ps, ok := globalSelf[varName]; ok && !selfConflict[varName] {
			self = ps
		}
		return joinPaths(mount, self)
	case "axum":
		// axum: the route's router var is mounted under a .nest prefix.
		varName := routeReceiver(c, framework, srcFor)
		if varName == "" {
			return ""
		}
		return chainPrefix(varName, map[string]bool{})
	case "nestjs":
		// Class-level @Controller('cats') prefix, found by scanning
		// upward from the route line for the nearest preceding
		// decorator in the same file.
		return nestControllerPrefix(c, srcFor)
	case "spring":
		return springClassPrefix(c, srcFor)
	case "rails":
		return railsNamespacePrefix(c, srcFor)
	case "laravel":
		return laravelGroupPrefix(c, srcFor)
	}
	return ""
}

// routeReceiver recovers the router/app variable a route was declared
// on, by re-reading the contract's source line. Returns "" when the
// line can't be read or no receiver is found.
func routeReceiver(c Contract, framework string, srcFor func(filePath string) []byte) string {
	line := sourceLine(srcFor(c.FilePath), c.Line)
	if line == "" {
		return ""
	}
	switch framework {
	case "fastapi/flask":
		if m := pyRouteReceiverRE.FindStringSubmatch(line); m != nil {
			return m[1]
		}
	case "flask":
		if m := flaskRouteReceiverRE.FindStringSubmatch(line); m != nil {
			return m[1]
		}
	case "express":
		if m := jsRouteReceiverRE.FindStringSubmatch(line); m != nil {
			return m[1]
		}
	case "gin/echo/chi", "fiber":
		if m := ginRouteReceiverRE.FindStringSubmatch(line); m != nil {
			return m[1]
		}
	case "axum":
		if m := axumLetBindingRE.FindStringSubmatch(line); m != nil {
			return m[1]
		}
	}
	return ""
}

// springClassPrefix scans upward from a Spring route line for the
// class-level @RequestMapping("/api") prefix (skipping the route's own
// decorator line), or "" when the controller declares none.
func springClassPrefix(c Contract, srcFor func(filePath string) []byte) string {
	src := srcFor(c.FilePath)
	if src == nil {
		return ""
	}
	lines := strings.Split(string(src), "\n")
	idx := c.Line - 1
	if idx < 0 || idx >= len(lines) {
		return ""
	}
	for i := idx - 1; i >= 0; i-- {
		if m := springClassMappingRE.FindStringSubmatch(lines[i]); m != nil {
			return cleanPrefix(m[1])
		}
	}
	return ""
}

// railsNamespacePrefix walks upward from a Rails route line, balancing
// do/end blocks, and joins the prefixes of every enclosing `namespace :x`.
func railsNamespacePrefix(c Contract, srcFor func(filePath string) []byte) string {
	src := srcFor(c.FilePath)
	if src == nil {
		return ""
	}
	lines := strings.Split(string(src), "\n")
	idx := c.Line - 1
	if idx < 0 || idx >= len(lines) {
		return ""
	}
	depth := 0
	var parts []string
	for i := idx - 1; i >= 0; i-- {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed == "end" || strings.HasPrefix(trimmed, "end ") {
			depth++
			continue
		}
		if !railsLineOpensBlock(trimmed) {
			continue
		}
		if depth > 0 {
			depth--
			continue
		}
		// Enclosing block opener at depth 0.
		if m := railsNamespaceRE.FindStringSubmatch(lines[i]); m != nil {
			parts = append([]string{m[1]}, parts...)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "/" + strings.Join(parts, "/")
}

// railsLineOpensBlock reports whether a Ruby routes line opens a do-block
// (namespace / resources / scope / draw).
func railsLineOpensBlock(trimmed string) bool {
	return strings.HasSuffix(trimmed, " do") ||
		strings.HasSuffix(trimmed, "do") && strings.Contains(trimmed, " do") ||
		strings.Contains(trimmed, " do |") ||
		strings.Contains(trimmed, " do;")
}

// laravelGroupPrefix walks upward from a Laravel route line, balancing
// braces, and joins the prefixes of every enclosing
// Route::prefix('x')->group(... { block.
func laravelGroupPrefix(c Contract, srcFor func(filePath string) []byte) string {
	src := srcFor(c.FilePath)
	if src == nil {
		return ""
	}
	lines := strings.Split(string(src), "\n")
	idx := c.Line - 1
	if idx < 0 || idx >= len(lines) {
		return ""
	}
	depth := 0
	var parts []string
	for i := idx - 1; i >= 0; i-- {
		line := lines[i]
		for j := len(line) - 1; j >= 0; j-- {
			switch line[j] {
			case '}':
				depth++
			case '{':
				if depth > 0 {
					depth--
					continue
				}
				// Enclosing open brace: prepend its prefix clause, if any.
				if m := laravelPrefixRE.FindStringSubmatch(line); m != nil {
					parts = append([]string{cleanPrefix(m[1])}, parts...)
				}
			}
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return joinPaths(parts...)
}

// nestControllerPrefix scans upward from a NestJS route line for the
// nearest @Controller('prefix') decorator (its enclosing class) and
// returns that prefix, or "" when the controller carries none.
func nestControllerPrefix(c Contract, srcFor func(filePath string) []byte) string {
	src := srcFor(c.FilePath)
	if src == nil {
		return ""
	}
	lines := strings.Split(string(src), "\n")
	idx := c.Line - 1 // contract lines are 1-based
	if idx < 0 || idx >= len(lines) {
		return ""
	}
	for i := idx; i >= 0; i-- {
		if m := nestControllerRE.FindStringSubmatch(lines[i]); m != nil {
			return cleanPrefix(m[1])
		}
	}
	return ""
}

// sourceLine returns the 1-based line `n` from src, or "".
func sourceLine(src []byte, n int) string {
	if src == nil || n < 1 {
		return ""
	}
	lines := strings.Split(string(src), "\n")
	if n > len(lines) {
		return ""
	}
	return lines[n-1]
}

// receiverOfInclude returns the receiver variable in front of an
// `.include_router(` call given the matched fragment, e.g. for
// `parent.include_router(child, ...)` returns "parent". Returns "" for
// a bare `include_router(...)` (an imported function call with no
// receiver) — those default to the implicit app and don't chain.
func receiverOfInclude(text, fragment string) string {
	i := strings.Index(text, fragment)
	if i <= 0 {
		return ""
	}
	// Walk backwards over the receiver expression: <recv>.include_router
	j := i
	// Skip a leading '.' if the fragment started at include_router.
	if j > 0 && text[j-1] == '.' {
		j--
	}
	end := j
	for j > 0 {
		ch := text[j-1]
		if isIdentByte(ch) || ch == '.' {
			j--
			continue
		}
		break
	}
	recv := text[j:end]
	return lastAttr(recv)
}

func isIdentByte(b byte) bool {
	return b == '_' ||
		(b >= 'a' && b <= 'z') ||
		(b >= 'A' && b <= 'Z') ||
		(b >= '0' && b <= '9')
}

// lastAttr returns the final attribute of a dotted reference, e.g.
// "users.router" -> "router", "router" -> "router".
func lastAttr(ref string) string {
	if i := strings.LastIndex(ref, "."); i >= 0 {
		return ref[i+1:]
	}
	return ref
}

// cloneMeta returns a shallow copy of a contract Meta map so a rewrite
// doesn't mutate the map still aliased by the registry's other indexes.
func cloneMeta(m map[string]any) map[string]any {
	out := make(map[string]any, len(m)+1)
	for k, v := range m {
		out[k] = v
	}
	return out
}

// cleanPrefix normalises a raw prefix string: trims surrounding
// whitespace, drops a trailing slash, and ensures it is either empty or
// starts with a slash. "/api/" -> "/api"; "api" -> "/api"; "" -> "".
func cleanPrefix(p string) string {
	p = strings.TrimSpace(p)
	if p == "" || p == "/" {
		return ""
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	p = strings.TrimRight(p, "/")
	return p
}

// joinPaths concatenates path segments, skipping empties and collapsing
// duplicate slashes. joinPaths("/api", "/users", "/{id}") ->
// "/api/users/{id}"; joinPaths("", "/users") -> "/users".
func joinPaths(parts ...string) string {
	var b strings.Builder
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if !strings.HasPrefix(p, "/") {
			p = "/" + p
		}
		p = strings.TrimRight(p, "/")
		b.WriteString(p)
	}
	if b.Len() == 0 {
		return ""
	}
	return b.String()
}
