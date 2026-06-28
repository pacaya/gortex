package languages

import (
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// captureAppleUIRoles classifies Apple-UI types from their inheritance clause:
// a SwiftUI `View` conformer is a component, an `@main` `App` is the app entry
// point, and a `UIViewController` / `UIView` / `UITableViewCell` subclass is a
// UIKit view-controller / view / cell. The role is stamped on the type node's
// Meta["swiftui_role"] or Meta["uikit_role"]; an app entry additionally carries
// Meta["entry_point"]=true so the dead-code and process analyzers treat it as a
// root. Runs at the tail of Extract so the type nodes already exist.
func captureAppleUIRoles(result *parser.ExtractionResult, root *sitter.Node, filePath string, src []byte) {
	if root == nil || result == nil {
		return
	}
	swiftUIWalk(root, func(n *sitter.Node) {
		if n.Type() != "class_declaration" {
			return
		}
		name := swiftUITypeName(n, src)
		if name == "" {
			return
		}
		conf := swiftUIConformances(n, src)
		swiftRole := ""
		switch {
		case swiftUIHasMainAttr(n, src) && conf["App"]:
			swiftRole = "app_entry"
		case conf["View"]:
			swiftRole = "component"
		}
		uikitRole := uikitRoleFor(conf)
		fluentModel := conf["Model"]
		if swiftRole == "" && uikitRole == "" && !fluentModel {
			return
		}
		nd := findSwiftUITypeNode(result.Nodes, name, int(n.StartPoint().Row)+1)
		if nd == nil {
			return
		}
		if nd.Meta == nil {
			nd.Meta = map[string]any{}
		}
		if swiftRole != "" {
			nd.Meta["swiftui_role"] = swiftRole
			if swiftRole == "app_entry" {
				nd.Meta["entry_point"] = true
			}
		}
		if uikitRole != "" {
			nd.Meta["uikit_role"] = uikitRole
		}
		if fluentModel {
			nd.Meta["fluent_model"] = true
		}
	})
}

// uikitRoleFor classifies a UIKit subclass by the framework base type in its
// inheritance clause. Cell base types are checked before UIView since they are
// themselves UIView subclasses by name shape only.
func uikitRoleFor(conf map[string]bool) string {
	switch {
	case conf["UIViewController"], conf["UITableViewController"], conf["UICollectionViewController"], conf["UINavigationController"], conf["UITabBarController"]:
		return "view_controller"
	case conf["UITableViewCell"], conf["UICollectionViewCell"]:
		return "cell"
	case conf["UIView"]:
		return "view"
	}
	return ""
}

// swiftUITypeName returns the declared name of a class_declaration — its first
// direct type_identifier child (the modifiers / inheritance type_identifiers
// are nested deeper, not direct children).
func swiftUITypeName(decl *sitter.Node, src []byte) string {
	for i, _nc := 0, int(decl.NamedChildCount()); i < _nc; i++ {
		if c := decl.NamedChild(i); c != nil && c.Type() == "type_identifier" {
			return c.Content(src)
		}
	}
	return ""
}

// swiftUIConformances returns the set of protocol / base-type names in a
// declaration's inheritance clause.
func swiftUIConformances(decl *sitter.Node, src []byte) map[string]bool {
	out := map[string]bool{}
	for i, _nc := 0, int(decl.NamedChildCount()); i < _nc; i++ {
		c := decl.NamedChild(i)
		if c == nil || c.Type() != "inheritance_specifier" {
			continue
		}
		swiftUIWalk(c, func(t *sitter.Node) {
			if t.Type() == "type_identifier" {
				out[t.Content(src)] = true
			}
		})
	}
	return out
}

// swiftUIHasMainAttr reports whether a declaration carries the `@main` attribute.
func swiftUIHasMainAttr(decl *sitter.Node, src []byte) bool {
	for i, _nc := 0, int(decl.NamedChildCount()); i < _nc; i++ {
		c := decl.NamedChild(i)
		if c == nil || c.Type() != "modifiers" {
			continue
		}
		found := false
		swiftUIWalk(c, func(t *sitter.Node) {
			if t.Type() == "type_identifier" && t.Content(src) == "main" {
				found = true
			}
		})
		if found {
			return true
		}
	}
	return false
}

// findSwiftUITypeNode returns the type node for name, preferring the one whose
// start line matches the declaration.
func findSwiftUITypeNode(nodes []*graph.Node, name string, line int) *graph.Node {
	var byName *graph.Node
	for _, n := range nodes {
		if n == nil || n.Kind != graph.KindType || n.Name != name {
			continue
		}
		if n.StartLine == line {
			return n
		}
		if byName == nil {
			byName = n
		}
	}
	return byName
}

// swiftUIWalk visits n and all its named descendants.
func swiftUIWalk(n *sitter.Node, fn func(*sitter.Node)) {
	if n == nil {
		return
	}
	fn(n)
	for i, _nc := 0, int(n.NamedChildCount()); i < _nc; i++ {
		swiftUIWalk(n.NamedChild(i), fn)
	}
}
