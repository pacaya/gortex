package languages

import (
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// React higher-order-component classification. A PascalCase const assigned
// `memo(...)`, `forwardRef(...)`, `React.memo(...)`, `styled.tag`...`` or
// `styled(Base)`...`` is a component, not a plain variable — but the TS/JS
// extractor mints it as a bare KindVariable. reactHOCComponentKind recognizes
// the initializer shape so the variable visitor can stamp Meta["component"]
// and re-attribute the inline render function's JSX to the outer const.

// reactHOCComponentKind classifies a variable_declarator's initializer as a
// React HOC component. It returns the component_kind ("memo" / "forwardRef" /
// "styled") and, for memo/forwardRef, the inline render function whose body's
// JSX should be attributed to the outer component (nil for styled, whose
// argument is a CSS template). Returns "" when the initializer is not a HOC.
func reactHOCComponentKind(node *sitter.Node, src []byte) (kind string, renderFn *sitter.Node) {
	declarator := node
	if declarator != nil && declarator.Type() != "variable_declarator" {
		// The variable visitor passes the enclosing lexical_declaration; descend
		// to its declarator.
		declarator = firstChildOfType(node, "variable_declarator")
	}
	if declarator == nil {
		return "", nil
	}
	val := declarator.ChildByFieldName("value")
	if val == nil || val.Type() != "call_expression" {
		return "", nil
	}
	fn := val.ChildByFieldName("function")
	if fn == nil {
		return "", nil
	}
	switch fn.Type() {
	case "identifier":
		switch fn.Content(src) {
		case "memo":
			return "memo", reactHOCRenderFn(val)
		case "forwardRef":
			return "forwardRef", reactHOCRenderFn(val)
		}
	case "member_expression":
		obj := fn.ChildByFieldName("object")
		if obj != nil && obj.Content(src) == "styled" {
			return "styled", nil // styled.div`...`
		}
		if obj != nil && obj.Content(src) == "React" {
			if prop := fn.ChildByFieldName("property"); prop != nil {
				switch prop.Content(src) {
				case "memo":
					return "memo", reactHOCRenderFn(val)
				case "forwardRef":
					return "forwardRef", reactHOCRenderFn(val)
				}
			}
		}
	case "call_expression":
		// styled(Base)`...` — a tagged template whose tag is a styled() call.
		if inner := fn.ChildByFieldName("function"); inner != nil && inner.Content(src) == "styled" {
			return "styled", nil
		}
	}
	return "", nil
}

// reactHOCRenderFn returns the first argument of a call when it is an inline
// arrow / function expression (the HOC's render function), else nil.
func reactHOCRenderFn(call *sitter.Node) *sitter.Node {
	args := call.ChildByFieldName("arguments")
	if args == nil {
		return nil
	}
	for i, _nc := 0, int(args.NamedChildCount()); i < _nc; i++ {
		a := args.NamedChild(i)
		if a == nil {
			continue
		}
		switch a.Type() {
		case "arrow_function", "function_expression", "function":
			return a
		}
		return nil // first argument is not an inline function
	}
	return nil
}
