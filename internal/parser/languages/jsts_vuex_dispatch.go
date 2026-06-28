package languages

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// Vuex string-keyed dispatch/commit binding. A Vuex store declares actions
// and mutations inside a `new Vuex.Store({ modules, actions, mutations })`
// config, namespaced by module path, and they are invoked by string key
// (`this.$store.dispatch('user/login')`, `commit('user/SET_TOKEN')`). This
// pass tags each action/mutation node with its namespace + name + kind and
// stamps a placeholder EdgeCalls per dispatch/commit site, which the
// resolver's ResolveVuexDispatchCalls binds with namespace disambiguation.
// Shared by the JS and TS extractors (and .vue SFCs via Vue delegation).

// vuexDispatchViaTag must match resolver.vuexDispatchVia — the languages
// package does not import resolver, so the two agree by value.
const vuexDispatchViaTag = "vuex-dispatch"

// captureVuexDispatch tags Vuex action/mutation nodes and stamps the
// dispatch/commit placeholders. Runs at the tail of Extract so the action
// function nodes already exist.
func captureVuexDispatch(result *parser.ExtractionResult, root *sitter.Node, filePath string, src []byte) {
	if root == nil || result == nil {
		return
	}
	// Index function/method nodes by start line, so a Vuex action found in
	// the config AST can be matched back to its node.
	nodesByLine := map[int][]*graph.Node{}
	for _, n := range result.Nodes {
		if n != nil && (n.Kind == graph.KindFunction || n.Kind == graph.KindMethod) {
			nodesByLine[n.StartLine] = append(nodesByLine[n.StartLine], n)
		}
	}

	tagged := false
	vuexWalk(root, func(n *sitter.Node) {
		obj := vuexConfigObject(n, src)
		if obj == nil {
			return
		}
		processVuexConfig(obj, "", src, nodesByLine)
		tagged = true
	})
	if !tagged {
		return
	}

	// Dispatch / commit call sites.
	funcRanges := buildFuncRanges(result)
	seen := map[string]bool{}
	vuexWalk(root, func(call *sitter.Node) {
		if call.Type() != "call_expression" {
			return
		}
		callee := jsCalleeLastName(call.ChildByFieldName("function"), src)
		switch callee {
		case "dispatch", "commit":
			vuexEmitDispatch(result, call, callee, src, funcRanges, filePath, seen)
		case "mapActions", "mapMutations":
			vuexEmitMapHelper(result, call, callee, src, funcRanges, filePath, seen)
		}
	})
}

// vuexConfigObject returns the Vuex store config object of a
// `new Vuex.Store({...})` / `new Store({...})` / `createStore({...})`
// expression, gated on the object carrying a Vuex-specific `mutations` or
// `modules` key (so a Redux `createStore(reducer)` is not mistaken for one).
func vuexConfigObject(n *sitter.Node, src []byte) *sitter.Node {
	var callee string
	var args *sitter.Node
	switch n.Type() {
	case "new_expression":
		callee = jsCalleeLastName(n.ChildByFieldName("constructor"), src)
		args = n.ChildByFieldName("arguments")
	case "call_expression":
		callee = jsCalleeLastName(n.ChildByFieldName("function"), src)
		args = n.ChildByFieldName("arguments")
	default:
		return nil
	}
	if callee != "Store" && callee != "createStore" {
		return nil
	}
	if args == nil {
		return nil
	}
	for i, _nc := 0, int(args.NamedChildCount()); i < _nc; i++ {
		obj := args.NamedChild(i)
		if obj == nil || obj.Type() != "object" {
			continue
		}
		if vuexHasKey(obj, "mutations", src) || vuexHasKey(obj, "modules", src) {
			return obj
		}
	}
	return nil
}

// processVuexConfig walks a store/module config object, tagging its actions
// and mutations with the running namespace prefix and recursing into nested
// modules (whose key joins the prefix only when `namespaced: true`).
func processVuexConfig(obj *sitter.Node, prefix string, src []byte, nodesByLine map[int][]*graph.Node) {
	namespace := strings.TrimSuffix(prefix, "/")
	for i, _nc := 0, int(obj.NamedChildCount()); i < _nc; i++ {
		pair := obj.NamedChild(i)
		if pair == nil || pair.Type() != "pair" {
			continue
		}
		key := rtkPairKey(pair, src)
		val := pair.ChildByFieldName("value")
		if val == nil {
			continue
		}
		switch key {
		case "actions", "mutations":
			if val.Type() == "object" {
				kind := "action"
				if key == "mutations" {
					kind = "mutation"
				}
				vuexTagMembers(val, namespace, kind, src, nodesByLine)
			}
		case "modules":
			if val.Type() == "object" {
				for j, _nc := 0, int(val.NamedChildCount()); j < _nc; j++ {
					mod := val.NamedChild(j)
					if mod == nil || mod.Type() != "pair" {
						continue
					}
					mobj := mod.ChildByFieldName("value")
					if mobj == nil || mobj.Type() != "object" {
						continue
					}
					sub := prefix
					if vuexIsNamespaced(mobj, src) {
						sub = prefix + rtkPairKey(mod, src) + "/"
					}
					processVuexConfig(mobj, sub, src, nodesByLine)
				}
			}
		}
	}
}

// vuexTagMembers stamps the action/mutation functions of an object literal
// with their Vuex namespace, name, and kind.
func vuexTagMembers(obj *sitter.Node, namespace, kind string, src []byte, nodesByLine map[int][]*graph.Node) {
	for i, _nc := 0, int(obj.NamedChildCount()); i < _nc; i++ {
		c := obj.NamedChild(i)
		if c == nil {
			continue
		}
		var member string
		switch c.Type() {
		case "method_definition":
			if name := c.ChildByFieldName("name"); name != nil {
				member = name.Content(src)
			}
		case "pair":
			val := c.ChildByFieldName("value")
			if val == nil || !vuexIsFnLike(val.Type()) {
				continue
			}
			member = rtkPairKey(c, src)
		default:
			continue
		}
		if member == "" {
			continue
		}
		line := int(c.StartPoint().Row) + 1
		for _, n := range nodesByLine[line] {
			if n.Name != member && !strings.HasSuffix(n.Name, "."+member) {
				continue
			}
			if n.Meta == nil {
				n.Meta = map[string]any{}
			}
			n.Meta["vuex_action"] = member
			n.Meta["vuex_namespace"] = namespace
			n.Meta["vuex_kind"] = kind
			break
		}
	}
}

// vuexEmitDispatch stamps the placeholder for a dispatch('ns/name') or
// commit('ns/NAME') call with a string-literal key.
func vuexEmitDispatch(result *parser.ExtractionResult, call *sitter.Node, callee string, src []byte, funcRanges []funcRange, filePath string, seen map[string]bool) {
	key := vuexFirstStringArg(call, src)
	if key == "" {
		return
	}
	kind := "action"
	if callee == "commit" {
		kind = "mutation"
	}
	name := key
	if i := strings.LastIndex(key, "/"); i >= 0 {
		name = key[i+1:]
	}
	line := int(call.StartPoint().Row) + 1
	from := findEnclosingFunc(funcRanges, line)
	vuexAppendPlaceholder(result, from, name, key, kind, filePath, line, seen)
}

// vuexEmitMapHelper stamps placeholders for a mapActions/mapMutations
// helper: an optional namespace string followed by an array of names.
func vuexEmitMapHelper(result *parser.ExtractionResult, call *sitter.Node, callee string, src []byte, funcRanges []funcRange, filePath string, seen map[string]bool) {
	kind := "action"
	if callee == "mapMutations" {
		kind = "mutation"
	}
	args := call.ChildByFieldName("arguments")
	if args == nil {
		return
	}
	namespace := ""
	var names []string
	for i, _nc := 0, int(args.NamedChildCount()); i < _nc; i++ {
		a := args.NamedChild(i)
		if a == nil {
			continue
		}
		switch a.Type() {
		case "string":
			namespace = jsStringLiteralContent(a, src)
		case "array":
			for j, _nc := 0, int(a.NamedChildCount()); j < _nc; j++ {
				if el := a.NamedChild(j); el != nil && el.Type() == "string" {
					if s := jsStringLiteralContent(el, src); s != "" {
						names = append(names, s)
					}
				}
			}
		}
	}
	if len(names) == 0 {
		return
	}
	line := int(call.StartPoint().Row) + 1
	from := findEnclosingFunc(funcRanges, line)
	for _, nm := range names {
		key := nm
		if namespace != "" {
			key = namespace + "/" + nm
		}
		vuexAppendPlaceholder(result, from, nm, key, kind, filePath, line, seen)
	}
}

func vuexAppendPlaceholder(result *parser.ExtractionResult, from, name, key, kind, filePath string, line int, seen map[string]bool) {
	if from == "" || name == "" {
		return
	}
	k := from + "\x00" + key + "\x00" + kind
	if seen[k] {
		return
	}
	seen[k] = true
	result.Edges = append(result.Edges, &graph.Edge{
		From:     from,
		To:       "unresolved::*." + name,
		Kind:     graph.EdgeCalls,
		FilePath: filePath,
		Line:     line,
		Meta: map[string]any{
			"via":       vuexDispatchViaTag,
			"vuex_key":  key,
			"vuex_kind": kind,
		},
	})
}

func vuexHasKey(obj *sitter.Node, key string, src []byte) bool {
	for i, _nc := 0, int(obj.NamedChildCount()); i < _nc; i++ {
		pair := obj.NamedChild(i)
		if pair != nil && pair.Type() == "pair" && rtkPairKey(pair, src) == key {
			return true
		}
	}
	return false
}

func vuexIsNamespaced(obj *sitter.Node, src []byte) bool {
	for i, _nc := 0, int(obj.NamedChildCount()); i < _nc; i++ {
		pair := obj.NamedChild(i)
		if pair == nil || pair.Type() != "pair" || rtkPairKey(pair, src) != "namespaced" {
			continue
		}
		if val := pair.ChildByFieldName("value"); val != nil {
			return strings.TrimSpace(val.Content(src)) == "true"
		}
	}
	return false
}

func vuexIsFnLike(t string) bool {
	switch t {
	case "arrow_function", "function_expression", "function":
		return true
	}
	return false
}

func vuexFirstStringArg(call *sitter.Node, src []byte) string {
	args := call.ChildByFieldName("arguments")
	if args == nil || args.NamedChildCount() == 0 {
		return ""
	}
	first := args.NamedChild(0)
	if first != nil && first.Type() == "string" {
		return jsStringLiteralContent(first, src)
	}
	return ""
}

// vuexWalk visits n and all its named descendants.
func vuexWalk(n *sitter.Node, fn func(*sitter.Node)) {
	if n == nil {
		return
	}
	fn(n)
	for i, _nc := 0, int(n.NamedChildCount()); i < _nc; i++ {
		vuexWalk(n.NamedChild(i), fn)
	}
}
