package languages

import sitter "github.com/zzet/gortex/internal/parser/tsitter"

// fnRefSpec is the per-grammar value-position capture spec for the
// function-as-value walk. It names which AST node types constitute a callable
// token in value position (idNodeTypes), how to peel address-of / eta wrappers
// (unwrapForms), how a call is disambiguated from a value (dispatch), and
// whether the gate may resolve the captured name cross-module (ungated).
//
// The default spec reproduces the original grammar-agnostic behaviour — a bare
// `identifier` leaf disambiguated by the next source byte — so a language with
// no entry regresses to nothing.
type fnRefSpec struct {
	// idNodeTypes are the node types whose content (or trailing segment, for a
	// qualified path) names a function in value position.
	idNodeTypes []string
	// unwrapForms maps a parent node type that wraps the callable token to the
	// `fn_ref_form` tag it records (`address_of` for `&fn`/`@fn`, `eta` for
	// Scala's `f _`).
	unwrapForms map[string]string
	// dispatch selects the call-vs-value disambiguator.
	dispatch fnRefDispatch
	// ungated lets the gate resolve the captured name cross-module (for a
	// qualified path like Rust `m::f` or Go `pkg.Fn`), not just same-file.
	ungated bool
}

type fnRefDispatch int

const (
	// dispatchByteParen is the original disambiguator: an identifier whose next
	// non-whitespace source byte is '(' or '`' is a callee, not a value.
	dispatchByteParen fnRefDispatch = iota
)

func (s fnRefSpec) matchesIDNode(t string) bool {
	for _, x := range s.idNodeTypes {
		if x == t {
			return true
		}
	}
	return false
}

var defaultFnRefSpec = fnRefSpec{idNodeTypes: []string{"identifier"}, dispatch: dispatchByteParen}

// fnRefSpecFor returns the value-position capture spec for a language, falling
// back to the default bare-identifier spec.
func fnRefSpecFor(lang string) fnRefSpec {
	switch lang {
	case "rust":
		// `let f = m::func;` — a path value whose trailing segment names the fn.
		return fnRefSpec{
			idNodeTypes: []string{"identifier", "scoped_identifier"},
			dispatch:    dispatchByteParen,
			ungated:     true,
		}
	case "go":
		// `cb := pkg.Fn` — a selector value whose field names the fn. Cross-pkg,
		// so ungated.
		return fnRefSpec{
			idNodeTypes: []string{"identifier", "selector_expression"},
			dispatch:    dispatchByteParen,
			ungated:     true,
		}
	case "swift", "kotlin":
		// The callable token is `simple_identifier`, not `identifier`.
		return fnRefSpec{idNodeTypes: []string{"identifier", "simple_identifier"}, dispatch: dispatchByteParen}
	case "scala":
		// Eta-expansion `f _` wraps the identifier in a postfix expression.
		return fnRefSpec{
			idNodeTypes: []string{"identifier"},
			unwrapForms: map[string]string{"postfix_expression": "eta"},
			dispatch:    dispatchByteParen,
		}
	case "pascal":
		// `@Fn` address-of: the `@` is an exprUnary wrapper around the ident.
		return fnRefSpec{
			idNodeTypes: []string{"identifier"},
			unwrapForms: map[string]string{"exprUnary": "address_of", "unary_expression": "address_of"},
			dispatch:    dispatchByteParen,
		}
	}
	return defaultFnRefSpec
}

// fnRefNodeName returns the function name a value-position node refers to: the
// node's own content for a leaf identifier, or the trailing segment for a
// qualified path (Rust `scoped_identifier`, Go `selector_expression`). Returns
// "" when no name can be recovered.
func fnRefNodeName(n *sitter.Node, src []byte) string {
	switch n.Type() {
	case "scoped_identifier":
		if name := n.ChildByFieldName("name"); name != nil {
			return name.Content(src)
		}
	case "selector_expression":
		if field := n.ChildByFieldName("field"); field != nil {
			return field.Content(src)
		}
	}
	return n.Content(src)
}

// fnRefForm reports the wrapper form (address_of / eta) a captured node sits in
// per the spec's unwrapForms, or "" for a plain value position.
func (s fnRefSpec) fnRefForm(n *sitter.Node) string {
	if len(s.unwrapForms) == 0 || n == nil {
		return ""
	}
	if p := n.Parent(); p != nil {
		if form, ok := s.unwrapForms[p.Type()]; ok {
			return form
		}
	}
	return ""
}
