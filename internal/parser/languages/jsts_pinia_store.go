package languages

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// Pinia store instance-method binding. A Pinia component reads a store via
// its getter (`const user = useUserStore()`) and calls actions on the
// instance (`user.login()`). The static call graph cannot see which action
// runs because the instance is a runtime object. This pass binds the local
// to its store getter and stamps the store-factory placeholder the existing
// ResolveStoreFactoryCalls already consumes — keyed by the getter name so a
// `useUserStore().login()` resolves to the user store's login, not another
// store's. Shared by the JS and TS extractors (and, through Vue's <script>
// delegation, by .vue single-file components).

// piniaStoreGetterName reports whether a callee name is a Pinia store
// getter by the framework convention: `use<Name>Store`.
func piniaStoreGetterName(name string) bool {
	return len(name) > len("useStore") && strings.HasPrefix(name, "use") && strings.HasSuffix(name, "Store")
}

// capturePiniaStoreCalls maps `const s = useXStore()` locals to their store
// getter, then stamps a store-factory placeholder per `s.member()` call.
// Runs at the tail of Extract so all enclosing function nodes exist.
func capturePiniaStoreCalls(result *parser.ExtractionResult, root *sitter.Node, filePath string, src []byte) {
	if root == nil || result == nil {
		return
	}
	// 0. Tag setup-store actions (returned functions) so they join the
	// store-factory index the options-API path already populates.
	tagPiniaSetupStoreActions(result, root, filePath, src)

	// 1. local var → store getter name.
	localToGetter := map[string]string{}
	piniaWalk(root, func(n *sitter.Node) {
		if n.Type() != "variable_declarator" {
			return
		}
		name := n.ChildByFieldName("name")
		init := n.ChildByFieldName("value")
		if name == nil || name.Type() != "identifier" || init == nil || init.Type() != "call_expression" {
			return
		}
		getter := jsCalleeLastName(init.ChildByFieldName("function"), src)
		if piniaStoreGetterName(getter) {
			localToGetter[name.Content(src)] = getter
		}
	})
	if len(localToGetter) == 0 {
		return
	}

	// 2. `<local>.member(...)` → store-factory placeholder.
	funcRanges := buildFuncRanges(result)
	seen := map[string]bool{}
	piniaWalk(root, func(call *sitter.Node) {
		if call.Type() != "call_expression" {
			return
		}
		fn := call.ChildByFieldName("function")
		if fn == nil || fn.Type() != "member_expression" {
			return
		}
		obj := fn.ChildByFieldName("object")
		if obj == nil || obj.Type() != "identifier" {
			return
		}
		getter, ok := localToGetter[obj.Content(src)]
		if !ok {
			return
		}
		prop := fn.ChildByFieldName("property")
		if prop == nil {
			return
		}
		member := prop.Content(src)
		if member == "" {
			return
		}
		line := int(call.StartPoint().Row) + 1
		from := findEnclosingFunc(funcRanges, line)
		if from == "" {
			return
		}
		k := from + "\x00" + getter + "\x00" + member
		if seen[k] {
			return
		}
		seen[k] = true
		result.Edges = append(result.Edges, &graph.Edge{
			From:     from,
			To:       "unresolved::*." + member,
			Kind:     graph.EdgeCalls,
			FilePath: filePath,
			Line:     line,
			Meta: map[string]any{
				"via":           "store-factory",
				"store_binding": getter,
				"store_action":  member,
			},
		})
	})
}

// tagPiniaSetupStoreActions stamps the functions a setup-style
// `defineStore('id', () => { function login(){} return { login } })`
// returns with Meta["store_factory"], so the same ResolveStoreFactoryCalls
// index that the options-API (`actions: {...}`) path feeds also covers
// setup stores. Options-API stores (object 2nd arg) are untouched here —
// they are already tagged by the store-factory extractor.
func tagPiniaSetupStoreActions(result *parser.ExtractionResult, root *sitter.Node, filePath string, src []byte) {
	piniaWalk(root, func(n *sitter.Node) {
		if n.Type() != "variable_declarator" {
			return
		}
		name := n.ChildByFieldName("name")
		init := n.ChildByFieldName("value")
		if name == nil || name.Type() != "identifier" || init == nil || init.Type() != "call_expression" {
			return
		}
		getter := name.Content(src)
		if !piniaStoreGetterName(getter) || jsCalleeLastName(init.ChildByFieldName("function"), src) != "defineStore" {
			return
		}
		setupFn := piniaSetupFn(init)
		if setupFn == nil {
			return
		}
		obj := rtkReturnedObject(setupFn)
		if obj == nil {
			return
		}
		returned := map[string]bool{}
		for i, _nc := 0, int(obj.NamedChildCount()); i < _nc; i++ {
			c := obj.NamedChild(i)
			if c == nil {
				continue
			}
			switch c.Type() {
			case "pair":
				if k := rtkPairKey(c, src); k != "" {
					returned[k] = true
				}
			case "shorthand_property_identifier", "shorthand_property_identifier_pattern":
				returned[c.Content(src)] = true
			}
		}
		if len(returned) == 0 {
			return
		}
		for _, nd := range result.Nodes {
			if nd == nil || nd.FilePath != filePath || !returned[nd.Name] {
				continue
			}
			switch nd.Kind {
			case graph.KindFunction, graph.KindMethod, graph.KindVariable:
				if nd.Meta == nil {
					nd.Meta = map[string]any{}
				}
				if _, has := nd.Meta["store_factory"]; !has {
					nd.Meta["store_factory"] = getter
					nd.Meta["store_member"] = nd.Name
				}
			}
		}
	})
}

// piniaSetupFn returns the setup function argument of a defineStore call —
// the arrow/function 2nd argument of a setup store. Returns nil for the
// options-API form (object 2nd argument).
func piniaSetupFn(call *sitter.Node) *sitter.Node {
	args := call.ChildByFieldName("arguments")
	if args == nil {
		return nil
	}
	for i, _nc := 0, int(args.NamedChildCount()); i < _nc; i++ {
		c := args.NamedChild(i)
		if c == nil {
			continue
		}
		switch c.Type() {
		case "arrow_function", "function_expression", "function":
			return c
		}
	}
	return nil
}

// piniaWalk visits n and all its named descendants.
func piniaWalk(n *sitter.Node, fn func(*sitter.Node)) {
	if n == nil {
		return
	}
	fn(n)
	for i, _nc := 0, int(n.NamedChildCount()); i < _nc; i++ {
		piniaWalk(n.NamedChild(i), fn)
	}
}
