package languages

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// Return-usage classification: given a call node, walk its parent chain
// and decide how the call site consumes the callee's return value
// (graph.ReturnUsage* labels). One engine serves every language; the
// per-language differences — which node kinds mean "assignment",
// "return", "argument list", "condition", and which kinds the walk
// passes through — live in a returnUsageSpec table per grammar.
//
// The walk is conservative: a parent kind no table covers stops the
// classification and the call edge carries no label, rather than a
// wrong one. Two deliberate label foldings: a call whose result is the
// receiver of a chained call (`f().g()`) is classified as argument
// (the value feeds another call, same as a literal argument), and an
// assignment whose every sink is blank (`_, _ = f()`) is classified as
// discarded — partially_ignored is reserved for the mixed case.

// returnUsageSpec drives classifyReturnUsage for one language. All kind
// sets refer to tree-sitter node kinds seen on the call's parent chain.
type returnUsageSpec struct {
	// goroutine / deferred: statement kinds that launch or defer the
	// call (Go-only — other languages leave these nil and never produce
	// the corresponding labels).
	goroutine map[string]bool
	deferred  map[string]bool
	// returns: return statements plus expression-bodied callable kinds
	// (arrow functions, lambdas, Rust fn tails) whose value becomes the
	// enclosing callable's return value.
	returns map[string]bool
	// assign: assignment-like kind → field name of its sink side. An
	// empty field name means "no countable sinks" (e.g. a Go channel
	// send) and classifies directly as assigned.
	assign map[string]string
	// sinkLists: kinds whose children are individual binding targets
	// (Go expression_list, Python pattern_list, Rust tuple_pattern, …).
	sinkLists map[string]bool
	// argument: argument-list kinds. argOwners guards grammars (Ruby)
	// that reuse the list kind under non-call constructs — when the
	// list's parent is not an owner the walk continues instead.
	argument  map[string]bool
	argOwners map[string]bool
	// condition: conditional-statement kind → field names whose child
	// means "inside the condition". An empty field list matches any
	// direct child that is not a block (Go's while-style for statement
	// carries its condition without a field name).
	condition map[string][]string
	// conditionExcludeKinds names child node kinds the empty-field
	// positional condition fallback must reject in addition to blocks —
	// the branch/arm containers a conditional shares its child list with
	// (C# switch_expression mixes its governing expression and its
	// fieldless switch_expression_arm children at the same level). Only
	// the governing expression is the condition; the arms are not.
	conditionExcludeKinds map[string]bool
	// chain: member-access kind → field name of the receiver/object
	// position. A call in that position feeds another expression and
	// classifies as argument; any other position continues the walk.
	chain map[string]string
	// discard: bare expression-statement kinds.
	discard map[string]bool
	// body: statement-container kinds for grammars without an
	// expression-statement wrapper (Ruby). A call in tail position
	// continues the walk (implicit-return languages route it to the
	// enclosing returns kind); any other position is discarded.
	body map[string]bool
	// closures: closure-boundary kinds (Ruby block / do_block) that the
	// walk must NOT cross. A call that reaches one is the closure's tail
	// value — returned from the closure, not from the enclosing method or
	// bound by the enclosing assignment. Classifying it as returned and
	// stopping keeps a block-internal call from inheriting the label of
	// whatever consumes the block (`x.map { f() }` must not mark f as
	// assigned to the map's receiver, nor as the method's return). Other
	// grammars register their closure forms (arrow_function, lambda,
	// closure_expression) in returns, which already halts the walk at the
	// boundary; Ruby's blocks have no such kind, so they live here.
	closures map[string]bool
	// transparent: kinds the walk passes straight through (parenthesised
	// and binary expressions, await, casts, literal containers, …).
	transparent map[string]bool
	// blocks: block/body kinds — used by the empty-field condition rule
	// to avoid classifying a branch body as a condition.
	blocks map[string]bool
}

// returnUsageMaxHops bounds the parent walk. Real classifications
// resolve within a handful of hops; the cap only guards degenerate
// trees (deeply nested transparent expressions).
const returnUsageMaxHops = 32

// classifyReturnUsage walks up from a call node and returns the
// graph.ReturnUsage* label for the call site, or "" when the parent
// chain doesn't match any classification the spec covers.
func classifyReturnUsage(call *sitter.Node, src []byte, spec *returnUsageSpec) string {
	if call == nil || spec == nil {
		return ""
	}
	child := call
	parent := call.Parent()
	for hops := 0; parent != nil && hops < returnUsageMaxHops; hops++ {
		kind := parent.Type()
		switch {
		case spec.goroutine[kind]:
			return graph.ReturnUsageGoroutine
		case spec.deferred[kind]:
			return graph.ReturnUsageDeferred
		case spec.returns[kind]:
			return graph.ReturnUsageReturned
		case spec.closures[kind]:
			// Closure boundary (Ruby block / do_block): a call reaches
			// one only as the closure's tail value, so it is returned from
			// the closure. Stop here — the walk must not continue into the
			// statement that consumes the closure, or a block-internal
			// call would inherit that statement's label.
			return graph.ReturnUsageReturned
		default:
		}
		if field, ok := spec.assign[kind]; ok {
			return classifyAssign(parent, child, field, src, spec)
		}
		if spec.argument[kind] {
			owner := parent.Parent()
			if len(spec.argOwners) == 0 || (owner != nil && spec.argOwners[owner.Type()]) {
				return graph.ReturnUsageArgument
			}
			// Argument-list kind under a non-call owner (Ruby `return
			// f()`): treat as transparent and keep walking.
			child, parent = parent, owner
			continue
		}
		if fields, ok := spec.condition[kind]; ok {
			if matchesConditionField(parent, child, fields, spec) {
				return graph.ReturnUsageCondition
			}
			// We arrived from a non-condition position (a branch body,
			// an init clause): keep walking — the statement's own fate
			// decides (e.g. `let x = if c { f() } else …` → assigned).
			child, parent = parent, parent.Parent()
			continue
		}
		if field, ok := spec.chain[kind]; ok {
			if c := parent.ChildByFieldName(field); c != nil && sameSpan(c, child) {
				return graph.ReturnUsageArgument
			}
			child, parent = parent, parent.Parent()
			continue
		}
		if spec.discard[kind] {
			return graph.ReturnUsageDiscarded
		}
		if spec.body[kind] {
			if isLastNamedChild(parent, child) {
				// Tail position in an implicit-return language: the
				// container is transparent; the enclosing callable (in
				// spec.returns) or outer statement decides.
				child, parent = parent, parent.Parent()
				continue
			}
			return graph.ReturnUsageDiscarded
		}
		if spec.transparent[kind] {
			child, parent = parent, parent.Parent()
			continue
		}
		return ""
	}
	return ""
}

// classifyAssign decides between assigned / partially_ignored /
// discarded for a call on the value side of an assignment-like node.
// A call inside the sink side itself (`m[f()] = v`) is not a binding
// of the call's result and stays unclassified.
func classifyAssign(parent, child *sitter.Node, sinkField string, src []byte, spec *returnUsageSpec) string {
	if sinkField == "" {
		return graph.ReturnUsageAssigned
	}
	total, blank := 0, 0
	for i, _nc := 0, int(parent.ChildCount()); i < _nc; i++ {
		if parent.FieldNameForChild(i) != sinkField {
			continue
		}
		c := parent.Child(i)
		if c == nil {
			continue
		}
		if sameSpan(c, child) || containsSpan(c, child) {
			return ""
		}
		t, b := countSinkLeaves(c, src, spec)
		total += t
		blank += b
	}
	switch {
	case total == 0:
		// The sink field is named but absent in this tree — a binding
		// form with no targets at all, e.g. Go `for range f()`. Nothing
		// captures the result; it is consumed without being bound. (The
		// genuinely sinkless forms that should read as assigned — a Go
		// channel send — take the sinkField == "" path above and never
		// reach here.)
		return graph.ReturnUsageDiscarded
	case blank == 0:
		return graph.ReturnUsageAssigned
	case blank == total:
		return graph.ReturnUsageDiscarded
	default:
		return graph.ReturnUsagePartiallyIgnored
	}
}

// countSinkLeaves counts the individual binding targets under a sink
// node and how many of them are blank (`_`). List/tuple kinds count
// each element; anything else is a single sink. Rust's `_` wildcard is
// an unnamed node, so the element loop keeps unnamed children whose
// text is exactly "_".
func countSinkLeaves(n *sitter.Node, src []byte, spec *returnUsageSpec) (total, blank int) {
	isBlank := func(c *sitter.Node) bool {
		return strings.TrimSpace(c.Content(src)) == "_"
	}
	if spec.sinkLists[n.Type()] {
		for i, _nc := 0, int(n.ChildCount()); i < _nc; i++ {
			c := n.Child(i)
			if c == nil {
				continue
			}
			if !c.IsNamed() && !isBlank(c) {
				continue
			}
			total++
			if isBlank(c) {
				blank++
			}
		}
		return total, blank
	}
	if isBlank(n) {
		return 1, 1
	}
	return 1, 0
}

// matchesConditionField reports whether child sits in one of the
// statement's condition fields. An empty field list applies the
// positional fallback: any direct non-block child is the condition
// (Go's `for f() { … }` carries the condition without a field).
func matchesConditionField(parent, child *sitter.Node, fields []string, spec *returnUsageSpec) bool {
	if len(fields) == 0 {
		ck := child.Type()
		return !spec.blocks[ck] && !spec.conditionExcludeKinds[ck]
	}
	for _, f := range fields {
		if c := parent.ChildByFieldName(f); c != nil && sameSpan(c, child) {
			return true
		}
	}
	return false
}

// sameSpan reports whether two nodes are the same node, compared by
// byte span (wrapper *Node pointers are not identity-stable).
func sameSpan(a, b *sitter.Node) bool {
	return a.StartByte() == b.StartByte() && a.EndByte() == b.EndByte()
}

// containsSpan reports whether outer strictly contains inner.
func containsSpan(outer, inner *sitter.Node) bool {
	return outer.StartByte() <= inner.StartByte() && inner.EndByte() <= outer.EndByte() &&
		(outer.StartByte() != inner.StartByte() || outer.EndByte() != inner.EndByte())
}

// isLastNamedChild reports whether child is parent's last named child.
func isLastNamedChild(parent, child *sitter.Node) bool {
	n := int(parent.NamedChildCount())
	if n == 0 {
		return false
	}
	last := parent.NamedChild(n - 1)
	return last != nil && sameSpan(last, child)
}

// stampReturnUsage records the return-usage label on a call edge's
// Meta. No-op for an empty label so unclassifiable sites carry no key.
func stampReturnUsage(e *graph.Edge, usage string) {
	if e == nil || usage == "" {
		return
	}
	if e.Meta == nil {
		e.Meta = map[string]any{}
	}
	e.Meta[graph.MetaReturnUsage] = usage
}

func kindSet(kinds ...string) map[string]bool {
	m := make(map[string]bool, len(kinds))
	for _, k := range kinds {
		m[k] = true
	}
	return m
}

// --- Per-language tables --------------------------------------------

var goReturnUsageSpec = &returnUsageSpec{
	goroutine: kindSet("go_statement"),
	deferred:  kindSet("defer_statement"),
	returns:   kindSet("return_statement"),
	assign: map[string]string{
		"assignment_statement":  "left",
		"short_var_declaration": "left",
		"var_spec":              "name",
		"range_clause":          "left",
		// A channel send binds the value into the channel; there is no
		// countable sink list.
		"send_statement": "",
	},
	sinkLists: kindSet("expression_list"),
	argument:  kindSet("argument_list", "special_argument_list"),
	argOwners: kindSet("call_expression"),
	condition: map[string][]string{
		"if_statement":                {"condition"},
		"for_statement":               {}, // while-style condition has no field
		"for_clause":                  {"condition"},
		"expression_switch_statement": {"value"},
		"type_switch_statement":       {"value"},
	},
	chain: map[string]string{
		"selector_expression": "operand",
		"call_expression":     "function", // curried `f()()`
	},
	discard: kindSet("expression_statement"),
	transparent: kindSet(
		"expression_list", "parenthesized_expression", "binary_expression",
		"unary_expression", "index_expression", "slice_expression",
		"type_assertion_expression", "type_conversion_expression",
		"literal_element", "keyed_element", "literal_value", "composite_literal",
	),
	blocks: kindSet("block"),
}

var pyReturnUsageSpec = &returnUsageSpec{
	returns: kindSet("return_statement", "lambda"),
	assign: map[string]string{
		"assignment":           "left",
		"augmented_assignment": "left",
	},
	sinkLists: kindSet("pattern_list", "tuple_pattern"),
	argument:  kindSet("argument_list"),
	argOwners: kindSet("call"),
	condition: map[string][]string{
		"if_statement":    {"condition"},
		"elif_clause":     {"condition"},
		"while_statement": {"condition"},
		"for_statement":   {"right"},
	},
	chain: map[string]string{
		"attribute": "object",
		"call":      "function", // curried `f()()`
	},
	discard: kindSet("expression_statement"),
	transparent: kindSet(
		"await", "parenthesized_expression", "binary_operator",
		"boolean_operator", "comparison_operator", "not_operator",
		"unary_operator", "conditional_expression", "subscript",
		"expression_list", "tuple", "list", "set", "dictionary", "pair",
	),
	blocks: kindSet("block"),
}

// jsTSReturnUsageSpec covers the javascript, typescript, and tsx
// grammars — the kinds the walk touches are shared; the TS-only
// wrapper expressions (as / satisfies / non-null) are transparent
// no-ops under the JS grammar.
var jsTSReturnUsageSpec = &returnUsageSpec{
	returns: kindSet("return_statement", "arrow_function"),
	assign: map[string]string{
		"variable_declarator":             "name",
		"assignment_expression":           "left",
		"augmented_assignment_expression": "left",
		"public_field_definition":         "name",
	},
	sinkLists: kindSet("array_pattern"),
	argument:  kindSet("arguments"),
	argOwners: kindSet("call_expression", "new_expression"),
	condition: map[string][]string{
		"if_statement":     {"condition"},
		"while_statement":  {"condition"},
		"do_statement":     {"condition"},
		"for_statement":    {"condition"},
		"for_in_statement": {"right"},
		"switch_statement": {"value"},
	},
	chain: map[string]string{
		"member_expression": "object",
		"call_expression":   "function", // curried `f()()`
	},
	discard: kindSet("expression_statement"),
	transparent: kindSet(
		"parenthesized_expression", "binary_expression", "unary_expression",
		"ternary_expression", "await_expression", "subscript_expression",
		"template_substitution", "spread_element", "object", "pair", "array",
		"sequence_expression",
		// TypeScript-only wrappers; absent from the JS grammar.
		"as_expression", "satisfies_expression", "non_null_expression",
		"type_assertion",
	),
	blocks: kindSet("statement_block"),
}

var javaReturnUsageSpec = &returnUsageSpec{
	returns: kindSet("return_statement", "lambda_expression"),
	assign: map[string]string{
		"variable_declarator":   "name",
		"assignment_expression": "left",
	},
	argument:  kindSet("argument_list"),
	argOwners: kindSet("method_invocation", "object_creation_expression", "explicit_constructor_invocation"),
	condition: map[string][]string{
		"if_statement":           {"condition"},
		"while_statement":        {"condition"},
		"do_statement":           {"condition"},
		"for_statement":          {"condition"},
		"enhanced_for_statement": {"value"},
		"switch_expression":      {"condition"},
	},
	chain: map[string]string{
		"method_invocation": "object",
		"field_access":      "object",
	},
	discard: kindSet("expression_statement"),
	transparent: kindSet(
		"parenthesized_expression", "binary_expression", "unary_expression",
		"ternary_expression", "cast_expression", "array_access",
		"array_initializer", "argument", "instanceof_expression",
	),
	blocks: kindSet("block"),
}

var rustReturnUsageSpec = &returnUsageSpec{
	// function_item / closure_expression terminate the tail-expression
	// walk: a call in block-tail position reaches them through the
	// transparent block and is the implicit return value.
	returns: kindSet("return_expression", "function_item", "closure_expression"),
	assign: map[string]string{
		"let_declaration":       "pattern",
		"assignment_expression": "left",
	},
	sinkLists: kindSet("tuple_pattern"),
	argument:  kindSet("arguments"),
	argOwners: kindSet("call_expression"),
	condition: map[string][]string{
		"if_expression":    {"condition"},
		"while_expression": {"condition"},
		"match_expression": {"value"},
		"for_expression":   {"value"},
	},
	chain: map[string]string{
		"field_expression": "value",
		"call_expression":  "function", // curried `f()()`
	},
	discard: kindSet("expression_statement"),
	transparent: kindSet(
		"block", "parenthesized_expression", "binary_expression",
		"unary_expression", "reference_expression", "try_expression",
		"await_expression", "let_condition", "match_arm", "match_block",
		"else_clause", "tuple_expression", "array_expression",
		"struct_expression", "field_initializer", "field_initializer_list",
		"index_expression", "range_expression", "type_cast_expression",
	),
	blocks: kindSet("block"),
}

var rubyReturnUsageSpec = &returnUsageSpec{
	// method / singleton_method terminate the tail-position walk
	// through body containers — Ruby's implicit return.
	returns: kindSet("return", "method", "singleton_method"),
	assign: map[string]string{
		"assignment":          "left",
		"operator_assignment": "left",
	},
	sinkLists: kindSet("left_assignment_list"),
	argument:  kindSet("argument_list"),
	argOwners: kindSet("call"),
	condition: map[string][]string{
		"if":              {"condition"},
		"unless":          {"condition"},
		"elsif":           {"condition"},
		"while":           {"condition"},
		"until":           {"condition"},
		"case":            {"value"},
		"if_modifier":     {"condition"},
		"unless_modifier": {"condition"},
		"while_modifier":  {"condition"},
		"until_modifier":  {"condition"},
	},
	chain: map[string]string{
		"call": "receiver",
	},
	// Ruby has no expression-statement wrapper: statements sit directly
	// in body containers, and the tail expression is the implicit
	// return value.
	body: kindSet("body_statement", "then", "else", "do", "block_body", "program"),
	// block / do_block are closure boundaries, not transparent wrappers:
	// a call in a block's tail position is the block's value, not the
	// enclosing method's return nor a binding of whatever consumes the
	// block. Crossing them mislabels `x.map { f() }` (f would read as
	// assigned to x.map's receiver) and `return x.each { f() }` (f would
	// read as the method's return).
	closures: kindSet("block", "do_block"),
	transparent: kindSet(
		"parenthesized_statements", "binary", "unary", "conditional",
		"begin", "argument_list_with_parens",
		"right_assignment_list", "element_reference", "pair", "array", "hash",
	),
	blocks: kindSet("then", "do", "block_body", "body_statement"),
}

var csharpReturnUsageSpec = &returnUsageSpec{
	returns: kindSet("return_statement", "lambda_expression", "arrow_expression_clause"),
	assign: map[string]string{
		"variable_declarator":   "name",
		"assignment_expression": "left",
	},
	argument:  kindSet("argument_list"),
	argOwners: kindSet("invocation_expression", "object_creation_expression", "implicit_object_creation_expression"),
	condition: map[string][]string{
		"if_statement":      {"condition"},
		"while_statement":   {"condition"},
		"do_statement":      {"condition"},
		"for_statement":     {"condition"},
		"foreach_statement": {"right"},
		"switch_statement":  {"value"},
		// A switch_expression carries no `value` field: its governing
		// expression is the first child, ahead of the `switch` keyword,
		// and the arms are fieldless switch_expression_arm siblings. The
		// empty field list selects it positionally; conditionExcludeKinds
		// keeps an arm from ever reading as the condition.
		"switch_expression": {},
	},
	conditionExcludeKinds: kindSet("switch_expression_arm"),
	chain: map[string]string{
		"member_access_expression": "expression",
	},
	discard: kindSet("expression_statement"),
	transparent: kindSet(
		"argument", "parenthesized_expression", "binary_expression",
		"prefix_unary_expression", "postfix_unary_expression",
		"cast_expression", "conditional_expression", "await_expression",
		"element_access_expression", "interpolation",
		"conditional_access_expression",
	),
	blocks: kindSet("block"),
}
