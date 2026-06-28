package tstypes

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
	"github.com/zzet/gortex/internal/parser/tsitter/php"
)

// PHPSpec adapts the engine to tree-sitter-php. PHP is gradually typed:
// typed properties (`private Foo $x`), typed parameters, and `: T`
// return declarations ground receivers directly, while constructor
// promotion (`__construct(private Foo $x)`), `new Foo()` constructor
// inference, and `$this->x = $typedParam` property-from-parameter
// inference fill the rest. `extends` becomes an extends edge and
// `implements` an implements edge; the static `Foo::bar()` form resolves
// against the named class.
func PHPSpec() *LangSpec {
	grammar := php.GetLanguage()
	return &LangSpec{
		ProviderName: "php-types",
		Languages:    []string{"php"},
		GrammarFor:   func(string) *sitter.Language { return grammar },
		TypeDeclTypes: map[string]bool{
			"class_declaration":     true,
			"interface_declaration": true,
			"trait_declaration":     true,
			"enum_declaration":      true,
		},
		FuncDeclTypes: map[string]bool{
			"method_declaration":  true,
			"function_definition": true,
		},
		SelfName:         "$this",
		TypeDeclName:     nameField,
		Supertypes:       phpSupertypes,
		Fields:           phpFields,
		Params:           phpParams,
		ReturnType:       phpReturnTypeSpec,
		LocalBinding:     phpLocalBinding,
		Call:             phpCall,
		NewExprType:      phpNewExprType,
		FieldRef:         phpFieldRef,
		Imports:          phpImports,
		NormalizeType:    normalizePHPType,
		ChainedReceivers: true,
		TraitAliases:     phpTraitAliases,
	}
}

// phpTraitAliases lists the trait-use alias adaptations of a PHP type:
// `use T { T::fn as renamed; }` maps the alias `renamed` on the using
// class to trait T's method `fn`. Conflict-resolution adaptations
// (`use A, B { A::fn insteadof B; }`) are intentionally skipped — the
// member they govern stays ambiguous and unresolved rather than being
// bound to one arbitrary side.
func phpTraitAliases(n *sitter.Node, src []byte) []AliasRef {
	body := phpTypeBody(n)
	if body == nil {
		return nil
	}
	var out []AliasRef
	for c := range body.NamedChildren() {
		if c.Type() != "use_declaration" {
			continue
		}
		for d := range c.NamedChildren() {
			if d.Type() != "use_list" {
				continue
			}
			for clause := range d.NamedChildren() {
				if clause.Type() != "use_as_clause" {
					continue
				}
				if ref, ok := phpUseAsClause(clause, src); ok {
					out = append(out, ref)
				}
			}
		}
	}
	return out
}

// phpUseAsClause decodes one `T::fn as renamed` / `fn as renamed`
// adaptation into an AliasRef. The source member is a
// class_constant_access_expression (`T::fn`, trait-qualified) or a bare
// leading name (`fn`, unqualified); the alias is the trailing name.
// Visibility-only adaptations (`T::fn as protected`) carry no alias name
// and yield ok=false.
func phpUseAsClause(clause *sitter.Node, src []byte) (AliasRef, bool) {
	var trait, method, alias string
	for c := range clause.NamedChildren() {
		switch c.Type() {
		case "class_constant_access_expression":
			var names []string
			for nm := range c.NamedChildren() {
				switch nm.Type() {
				case "name", "qualified_name":
					names = append(names, strings.TrimSpace(nm.Content(src)))
				}
			}
			if len(names) >= 2 {
				trait, method = names[0], names[len(names)-1]
			} else if len(names) == 1 {
				method = names[0]
			}
		case "name", "qualified_name":
			if method == "" {
				method = strings.TrimSpace(c.Content(src))
			} else {
				alias = strings.TrimSpace(c.Content(src))
			}
		}
	}
	if alias == "" || method == "" {
		return AliasRef{}, false
	}
	return AliasRef{Alias: alias, Trait: trait, Method: method, Line: nodeLine(clause)}, true
}

// phpSupertypes lists the declared supertype relations of a PHP type
// declaration: `extends` (base_clause) becomes extends, `implements`
// (class_interface_clause) becomes implements. On an interface
// declaration the same class_interface_clause node spells
// `interface X extends A, B`, so its members are extends there. Trait
// composition (`use T;` in the body) is also reported as an extends
// relation — matching the edge kind the PHP extractor emits for trait
// use — so the inherited-member climb reaches a used trait's methods
// once the edge resolves.
func phpSupertypes(n *sitter.Node, src []byte) []SuperRef {
	isInterface := n.Type() == "interface_declaration"
	var out []SuperRef
	for c := range n.NamedChildren() {
		switch c.Type() {
		case "base_clause":
			for _, name := range phpClauseNames(c, src) {
				out = append(out, SuperRef{Name: name, Kind: graph.EdgeExtends, Line: nodeLine(c)})
			}
		case "class_interface_clause":
			kind := graph.EdgeImplements
			if isInterface {
				kind = graph.EdgeExtends
			}
			for _, name := range phpClauseNames(c, src) {
				out = append(out, SuperRef{Name: name, Kind: kind, Line: nodeLine(c)})
			}
		}
	}
	out = append(out, phpTraitUses(n, src)...)
	return out
}

// phpTraitUses lists the traits composed into a type via `use T;` /
// `use A, B { ... }` statements in its body. Each names a trait the
// extractor already linked with an (unresolved) extends edge; reporting
// it here lets the apply phase resolve that edge to the trait node so
// the trait's methods climb. The adaptation block (`{ ... }`) is not a
// trait name, so only the leading name / qualified_name children are
// collected.
func phpTraitUses(n *sitter.Node, src []byte) []SuperRef {
	body := phpTypeBody(n)
	if body == nil {
		return nil
	}
	var out []SuperRef
	for c := range body.NamedChildren() {
		if c.Type() != "use_declaration" {
			continue
		}
		for nm := range c.NamedChildren() {
			switch nm.Type() {
			case "name", "qualified_name":
				if t := strings.TrimSpace(nm.Content(src)); t != "" {
					out = append(out, SuperRef{Name: t, Kind: graph.EdgeExtends, Line: nodeLine(nm)})
				}
			}
		}
	}
	return out
}

// phpClauseNames returns the type names listed in an inheritance clause
// (base_clause / class_interface_clause): each `name` / `qualified_name`
// child naming a supertype.
func phpClauseNames(clause *sitter.Node, src []byte) []string {
	var out []string
	for c := range clause.NamedChildren() {
		switch c.Type() {
		case "name", "qualified_name":
			if t := strings.TrimSpace(c.Content(src)); t != "" {
				out = append(out, t)
			}
		}
	}
	return out
}

// phpFields grounds the instance-field types of a PHP type: typed
// property declarations, constructor-promoted properties, and the
// `$this->x = $typedParam` / `$this->x = new Foo()` initialisations a
// constructor performs. Untyped properties contribute nothing here —
// their type, if any, is left to the assignment inference so a declared
// `private $x;` does not conflict with the type the constructor assigns.
func phpFields(n *sitter.Node, src []byte) []Binding {
	body := phpTypeBody(n)
	if body == nil {
		return nil
	}
	var out []Binding
	for c := range body.NamedChildren() {
		switch c.Type() {
		case "property_declaration":
			out = append(out, phpPropertyFields(c, src)...)
		case "method_declaration":
			out = append(out, phpPromotedFields(c, src)...)
			out = append(out, phpAssignedFields(c, src)...)
		}
	}
	return out
}

// phpTypeBody returns the member list of a class / interface / trait
// (declaration_list, the `body` field) or an enum (enum_declaration_list).
func phpTypeBody(n *sitter.Node) *sitter.Node {
	if b := n.ChildByFieldName("body"); b != nil {
		return b
	}
	if b := firstChildOfType(n, "declaration_list"); b != nil {
		return b
	}
	return firstChildOfType(n, "enum_declaration_list")
}

// phpPropertyFields decodes a typed `private Foo $x, $y;` property
// declaration into one binding per declared element. Untyped properties
// yield nothing.
func phpPropertyFields(prop *sitter.Node, src []byte) []Binding {
	typ := phpTypeText(prop.ChildByFieldName("type"), src)
	if typ == "" {
		return nil
	}
	var out []Binding
	for c := range prop.NamedChildren() {
		if c.Type() != "property_element" {
			continue
		}
		name := phpFieldName(c.ChildByFieldName("name"), src)
		if name == "" {
			continue
		}
		out = append(out, Binding{Name: name, Type: typ, Line: nodeLine(c)})
	}
	return out
}

// phpPromotedFields decodes constructor-promoted properties — typed
// `property_promotion_parameter`s carry both a parameter binding and a
// field binding; this returns the field side.
func phpPromotedFields(method *sitter.Node, src []byte) []Binding {
	params := method.ChildByFieldName("parameters")
	if params == nil {
		return nil
	}
	var out []Binding
	for p := range params.NamedChildren() {
		if p.Type() != "property_promotion_parameter" {
			continue
		}
		typ := phpTypeText(p.ChildByFieldName("type"), src)
		if typ == "" {
			continue
		}
		name := phpFieldName(p.ChildByFieldName("name"), src)
		if name == "" {
			continue
		}
		out = append(out, Binding{Name: name, Type: typ, Line: nodeLine(p)})
	}
	return out
}

// phpAssignedFields infers a field's type from a `$this->x = <expr>`
// assignment inside a method body: a typed parameter assigned to the
// property gives the property that type, as does a `new Foo()`. Stops at
// nested type / function scopes, which own their own `$this`.
func phpAssignedFields(method *sitter.Node, src []byte) []Binding {
	body := method.ChildByFieldName("body")
	if body == nil {
		return nil
	}
	paramTypes := phpParamTypeMap(method, src)
	var out []Binding
	var visit func(node *sitter.Node)
	visit = func(node *sitter.Node) {
		if node == nil {
			return
		}
		switch node.Type() {
		case "class_declaration", "interface_declaration", "trait_declaration",
			"enum_declaration", "function_definition", "method_declaration",
			"anonymous_function_creation_expression", "arrow_function":
			return
		case "assignment_expression":
			if b, ok := phpThisFieldAssign(node, src, paramTypes); ok {
				out = append(out, b)
			}
		}
		for c := range node.NamedChildren() {
			visit(c)
		}
	}
	for c := range body.NamedChildren() {
		visit(c)
	}
	return out
}

// phpThisFieldAssign decodes `$this->field = <expr>` into the field's
// inferred binding, or ok=false for any other assignment shape.
func phpThisFieldAssign(assign *sitter.Node, src []byte, paramTypes map[string]string) (Binding, bool) {
	left := assign.ChildByFieldName("left")
	right := assign.ChildByFieldName("right")
	if left == nil || right == nil || left.Type() != "member_access_expression" {
		return Binding{}, false
	}
	obj := left.ChildByFieldName("object")
	if obj == nil || strings.TrimSpace(obj.Content(src)) != "$this" {
		return Binding{}, false
	}
	nameNode := left.ChildByFieldName("name")
	if nameNode == nil {
		return Binding{}, false
	}
	field := strings.TrimSpace(nameNode.Content(src))
	if field == "" {
		return Binding{}, false
	}
	typ := phpAssignedType(right, src, paramTypes)
	if typ == "" {
		return Binding{}, false
	}
	return Binding{Name: field, Type: typ, Line: nodeLine(left)}, true
}

// phpAssignedType resolves the right-hand side of a property assignment
// to a type name: a typed parameter reference or a `new Foo()`.
func phpAssignedType(right *sitter.Node, src []byte, paramTypes map[string]string) string {
	switch right.Type() {
	case "variable_name":
		return paramTypes[strings.TrimSpace(right.Content(src))]
	case "object_creation_expression":
		return phpCreationName(right, src)
	case "parenthesized_expression":
		if inner := phpUnwrapParen(right); inner != right {
			return phpAssignedType(inner, src, paramTypes)
		}
	}
	return ""
}

// phpParamTypeMap maps a callable's parameter names (sigil-included, e.g.
// `$seed`) to their declared types, so an assignment from a parameter can
// be typed.
func phpParamTypeMap(method *sitter.Node, src []byte) map[string]string {
	out := map[string]string{}
	params := method.ChildByFieldName("parameters")
	if params == nil {
		return out
	}
	for p := range params.NamedChildren() {
		switch p.Type() {
		case "simple_parameter", "variadic_parameter", "property_promotion_parameter":
			typ := phpTypeText(p.ChildByFieldName("type"), src)
			if typ == "" {
				continue
			}
			nm := p.ChildByFieldName("name")
			if nm == nil {
				continue
			}
			out[strings.TrimSpace(nm.Content(src))] = typ
		}
	}
	return out
}

// phpParams lists a callable's parameters. Names keep the `$` sigil so a
// `$param->method()` receiver lookup matches the variable_name the call
// site carries.
func phpParams(fn *sitter.Node, src []byte) []Binding {
	params := fn.ChildByFieldName("parameters")
	if params == nil {
		return nil
	}
	var out []Binding
	for p := range params.NamedChildren() {
		switch p.Type() {
		case "simple_parameter", "variadic_parameter", "property_promotion_parameter":
			nm := p.ChildByFieldName("name")
			if nm == nil {
				continue
			}
			name := strings.TrimSpace(nm.Content(src))
			if name == "" {
				continue
			}
			out = append(out, Binding{Name: name, Type: phpTypeText(p.ChildByFieldName("type"), src), Line: nodeLine(p)})
		}
	}
	return out
}

// phpReturnTypeSpec extracts the `: T` return-type declaration of a
// method / function (the `return_type` field), "" when absent.
func phpReturnTypeSpec(fn *sitter.Node, src []byte) string {
	return phpTypeText(fn.ChildByFieldName("return_type"), src)
}

// phpLocalBinding decodes `$local = <expr>` — a sigil-named local
// assignment whose initializer the engine may type. Member-target
// assignments (`$this->x = …`) are not locals; they are handled as
// field inference in phpFields, so they return ok=false here.
func phpLocalBinding(n *sitter.Node, src []byte) (LocalBind, bool) {
	if n.Type() != "assignment_expression" {
		return LocalBind{}, false
	}
	left := n.ChildByFieldName("left")
	if left == nil || left.Type() != "variable_name" {
		return LocalBind{}, false
	}
	return LocalBind{Name: strings.TrimSpace(left.Content(src)), Init: n.ChildByFieldName("right")}, true
}

// phpCall decodes a receiver-qualified call: `$obj->method()`
// (member_call_expression) and the static `Foo::method()`
// (scoped_call_expression). The relative scopes (`self::` / `parent::` /
// `static::`) carry a relative_scope node the engine cannot ground to a
// distinct type, so they are skipped.
func phpCall(n *sitter.Node, src []byte) (*sitter.Node, string, bool) {
	switch n.Type() {
	case "member_call_expression":
		obj := n.ChildByFieldName("object")
		name := n.ChildByFieldName("name")
		if obj == nil || name == nil {
			return nil, "", false
		}
		return obj, strings.TrimSpace(name.Content(src)), true
	case "scoped_call_expression":
		scope := n.ChildByFieldName("scope")
		name := n.ChildByFieldName("name")
		if scope == nil || name == nil || scope.Type() == "relative_scope" {
			return nil, "", false
		}
		return scope, strings.TrimSpace(name.Content(src)), true
	}
	return nil, "", false
}

// phpNewExprType returns the constructed type name when n is a `new Foo()`
// (object_creation_expression), unwrapping a parenthesised
// `(new Foo())` first so a `(new Foo())->bar()` chain types its receiver.
func phpNewExprType(n *sitter.Node, src []byte) string {
	n = phpUnwrapParen(n)
	if n == nil || n.Type() != "object_creation_expression" {
		return ""
	}
	return phpCreationName(n, src)
}

// phpCreationName returns the leading type name of an
// object_creation_expression (`new Foo()` / `new \App\Foo()`), "" for the
// `new $klass()` / `new class {}` shapes that name no type.
func phpCreationName(n *sitter.Node, src []byte) string {
	for c := range n.NamedChildren() {
		switch c.Type() {
		case "name", "qualified_name":
			return strings.TrimSpace(c.Content(src))
		case "arguments":
			return ""
		}
	}
	return ""
}

// phpUnwrapParen peels parenthesized_expression layers to the inner
// expression.
func phpUnwrapParen(n *sitter.Node) *sitter.Node {
	for n != nil && n.Type() == "parenthesized_expression" {
		var inner *sitter.Node
		for c := range n.NamedChildren() {
			inner = c
			break
		}
		if inner == nil {
			break
		}
		n = inner
	}
	return n
}

// phpFieldRef reports that n is a `$this->field` access and returns the
// field name (sigil-free `name`, matching the property binding key).
func phpFieldRef(n *sitter.Node, src []byte) (string, bool) {
	if n.Type() != "member_access_expression" {
		return "", false
	}
	obj := n.ChildByFieldName("object")
	if obj == nil || strings.TrimSpace(obj.Content(src)) != "$this" {
		return "", false
	}
	name := n.ChildByFieldName("name")
	if name == nil {
		return "", false
	}
	field := strings.TrimSpace(name.Content(src))
	if field == "" {
		return "", false
	}
	return field, true
}

// phpImports lists the file's `use App\Foo;` name bindings, recursively
// (a braced namespace nests its uses). Local is the bound short name
// (alias-aware); Path is the slash-separated FQN used as the cross-file
// definition hint.
func phpImports(root *sitter.Node, src []byte) []Import {
	var out []Import
	var visit func(n *sitter.Node)
	visit = func(n *sitter.Node) {
		if n == nil {
			return
		}
		if n.Type() == "namespace_use_declaration" {
			out = append(out, phpUseImports(n, src)...)
			return
		}
		for c := range n.NamedChildren() {
			visit(c)
		}
	}
	visit(root)
	return out
}

// phpUseImports decodes one namespace_use_declaration's clauses into
// name bindings.
func phpUseImports(decl *sitter.Node, src []byte) []Import {
	var out []Import
	for clause := range decl.NamedChildren() {
		if clause.Type() != "namespace_use_clause" {
			continue
		}
		nameNode := firstChildOfType(clause, "qualified_name")
		if nameNode == nil {
			nameNode = firstChildOfType(clause, "namespace_name")
		}
		if nameNode == nil {
			continue
		}
		fqn := strings.TrimSpace(nameNode.Content(src))
		if fqn == "" {
			continue
		}
		local := fqn
		if i := strings.LastIndex(local, `\`); i >= 0 {
			local = local[i+1:]
		}
		// `use App\Foo as Bar` renames the local binding.
		if alias := firstChildOfType(clause, "namespace_aliasing_clause"); alias != nil {
			if a := firstChildOfType(alias, "name"); a != nil {
				if t := strings.TrimSpace(a.Content(src)); t != "" {
					local = t
				}
			}
		}
		path := strings.ReplaceAll(strings.TrimLeft(fqn, `\`), `\`, "/")
		out = append(out, Import{Local: local, Path: path})
	}
	return out
}

// phpTypeText returns a type node's source text, trimmed; "" for nil.
func phpTypeText(n *sitter.Node, src []byte) string {
	if n == nil {
		return ""
	}
	return strings.TrimSpace(n.Content(src))
}

// phpFieldName strips the `$` sigil off a property element / promoted
// parameter's variable_name so the field key matches the sigil-free
// `$this->name` access form.
func phpFieldName(vn *sitter.Node, src []byte) string {
	if vn == nil {
		return ""
	}
	return strings.TrimPrefix(strings.TrimSpace(vn.Content(src)), "$")
}

// normalizePHPType reduces a written PHP type to the bare class name the
// graph indexes: the nullable `?` shorthand and a leading / embedded
// namespace qualification (`\App\Http\Client`) are stripped to the last
// segment, then the shared reduction handles any residual generics.
func normalizePHPType(t string) string {
	t = strings.TrimSpace(t)
	if t == "" {
		return ""
	}
	t = strings.TrimPrefix(t, "?")
	t = strings.TrimSpace(t)
	t = strings.TrimPrefix(t, `\`)
	if i := strings.LastIndex(t, `\`); i >= 0 {
		t = t[i+1:]
	}
	return NormalizeTypeName(t)
}
