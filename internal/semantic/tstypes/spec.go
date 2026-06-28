// Package tstypes implements in-process, LSP-free semantic providers
// over the shared tree-sitter ASTs. One shared engine builds a per-file
// scope graph (params, locals, fields, imports), binds declared and
// constructor-inferred types, propagates them through local assignments
// (single-assignment-lite: a rebind to a different type degrades the
// binding to unknown), and resolves receiver-qualified calls plus
// declared supertype relations against the symbol nodes the graph
// already holds. Per-language LangSpec tables adapt the engine to each
// grammar's node vocabulary.
//
// Provenance: everything this package touches is tree-sitter-derived,
// not compiler-verified, so edges are stamped OriginASTResolved (never
// the lsp_* tiers ConfirmEdge uses) with Meta["semantic_source"] set to
// the provider name ("java-types", "python-types", ...). A resolution
// the engine cannot ground in graph evidence — ambiguous receiver,
// unresolvable type name, overloaded method set — is skipped rather
// than guessed: a false edge is worse than a missing one.
package tstypes

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// Binding is one named, optionally typed binding (param or field).
type Binding struct {
	Name string
	Type string // declared type name as written; "" when unannotated
	Line int    // 1-based declaration line
}

// LocalBind is one local-variable declaration or assignment the engine
// folds into the scope's type environment.
type LocalBind struct {
	Name     string
	DeclType string       // explicit annotation; "" when absent
	Init     *sitter.Node // initializer expression; nil when absent
	Field    bool         // binds in the enclosing type scope (e.g. Ruby @ivar)
}

// SuperRef is one declared supertype relation of a type declaration.
// Kind is EdgeExtends or EdgeImplements when the syntax declares it;
// an empty Kind defers the choice to the apply phase, which picks by
// the resolved target's node kind (used by C#, whose base list does
// not distinguish the base class from interfaces syntactically).
type SuperRef struct {
	Name string
	Kind graph.EdgeKind
	Line int // 1-based
}

// Import is one name-binding import: Local is the identifier the file
// sees; Path is a slash-separated location hint used to prefer the
// matching definition file when several nodes share the name.
type Import struct {
	Local string
	Path  string
}

// AliasRef is one trait-use adaptation that renames an aliased member
// onto the using type (PHP `use T { T::fn as renamed; }`): Alias is the
// new name the using type exposes, Method is the original member name,
// and Trait names the trait the member comes from ("" when the
// adaptation is unqualified, e.g. `use T { fn as renamed; }`).
// Conflict-resolution adaptations (`insteadof`) are deliberately NOT
// represented here — an ambiguous member stays unresolved rather than
// being bound to one arbitrary side.
type AliasRef struct {
	Alias  string
	Trait  string
	Method string
	Line   int
}

// LangSpec adapts the shared engine to one language's tree-sitter
// grammar. The node-type sets drive the generic walk; the hooks decode
// the handful of shapes that differ per grammar. Hooks may be nil when
// the language has no equivalent construct (e.g. Ruby has no type
// annotations, C# has no name-binding imports).
type LangSpec struct {
	ProviderName string
	Languages    []string

	// GrammarFor returns the grammar for a file path. Per-path because
	// one provider can span sibling grammars (typescript / tsx /
	// javascript).
	GrammarFor func(filePath string) *sitter.Language

	// TypeDeclTypes / FuncDeclTypes are the node types that open a type
	// or callable scope.
	TypeDeclTypes map[string]bool
	FuncDeclTypes map[string]bool

	// SelfName is the receiver keyword ("this", "self"); "" when the
	// language has none.
	SelfName string

	// TypeDeclName extracts the declared type name ("" skips the node).
	TypeDeclName func(n *sitter.Node, src []byte) string

	// Supertypes lists the declared supertype relations of a type decl.
	Supertypes func(n *sitter.Node, src []byte) []SuperRef

	// Fields lists the field bindings of a type decl (declared fields
	// plus whatever conventional initialisations the language grounds,
	// e.g. Python's `self.x = Foo()` or Ruby's `@x = Foo.new`).
	Fields func(n *sitter.Node, src []byte) []Binding

	// Params lists a callable's declared parameters.
	Params func(fn *sitter.Node, src []byte) []Binding

	// ReturnType extracts an explicit return-type annotation ("" when
	// absent or unsupported).
	ReturnType func(fn *sitter.Node, src []byte) string

	// LocalBinding decodes a local declaration / assignment node.
	LocalBinding func(n *sitter.Node, src []byte) (LocalBind, bool)

	// Call decodes a receiver-qualified call: the receiver expression
	// and the method name. ok=false for anything else (including
	// receiverless calls — those are the resolver's job already).
	Call func(n *sitter.Node, src []byte) (recv *sitter.Node, method string, ok bool)

	// NewExprType returns the constructed type name when n is a
	// constructor expression ("" otherwise). Conventional constructors
	// (Python `Foo()`, Ruby `Foo.new`, Rust `Foo::new`) may be
	// returned too — the apply phase verifies every receiver type
	// against a real graph type node before resolving through it.
	NewExprType func(n *sitter.Node, src []byte) string

	// FieldRef reports that n is a reference to an instance field of
	// the current receiver (`this.x`, `self.x`, `@x`) and returns the
	// field's binding name.
	FieldRef func(n *sitter.Node, src []byte) (string, bool)

	// Imports lists the file's name-binding imports.
	Imports func(root *sitter.Node, src []byte) []Import

	// SupertypeKinds widens the node kinds a declared supertype name
	// may resolve to. nil keeps the receiver default (type /
	// interface). Ruby adds packages: tree-sitter modules index as
	// KindPackage and `include M` targets them.
	SupertypeKinds map[graph.NodeKind]bool

	// InheritEdgeKinds lists the edge kinds methodOn climbs when it
	// looks up an inherited member. An empty slice defaults to
	// {EdgeExtends} — only the superclass / supertype chain. Languages
	// whose inheritance spans more than subclassing widen it: Ruby adds
	// EdgeImplements so the modules pulled in by `include` / `prepend`
	// / `extend` contribute their methods. PHP keeps the {EdgeExtends}
	// default: trait composition (`use T;`) is itself modeled as an
	// extends edge, so the default walk already climbs into used traits
	// once they resolve.
	InheritEdgeKinds []graph.EdgeKind

	// ChainedReceivers enables typing a call whose receiver is itself a
	// method call (`a.step().done()`). When set, the binder grounds the
	// inner call's receiver and method, and the apply phase resolves the
	// inner method's declared return type — applying the fluent self /
	// trait return rewrite — to type the outer call's receiver. Off by
	// default; languages with reliable return-type fidelity and fluent
	// chains opt in. The resulting outer edge is graded as inferred.
	ChainedReceivers bool

	// TraitAliases lists the trait-use adaptations that rename an aliased
	// member onto a using type (PHP `use T { T::fn as renamed; }`). nil
	// for languages without the construct. The apply phase routes a call
	// to the alias name through to the original trait member.
	TraitAliases func(n *sitter.Node, src []byte) []AliasRef

	// NormalizeType reduces a written type to the bare name the graph
	// indexes (strip generics / pointers / qualifiers). nil uses the
	// shared default.
	NormalizeType func(t string) string
}

// inheritEdgeKinds returns the edge kinds methodOn climbs when looking
// up an inherited member: the spec's explicit set, or {EdgeExtends}
// when it leaves the field empty — preserving the legacy
// superclass-only walk for every language that does not widen it.
func (s *LangSpec) inheritEdgeKinds() []graph.EdgeKind {
	if len(s.InheritEdgeKinds) > 0 {
		return s.InheritEdgeKinds
	}
	return []graph.EdgeKind{graph.EdgeExtends}
}

func (s *LangSpec) normalize(t string) string {
	if s.NormalizeType != nil {
		return s.NormalizeType(t)
	}
	return NormalizeTypeName(t)
}

// handles reports whether the spec serves the given language code.
func (s *LangSpec) handles(lang string) bool {
	for _, l := range s.Languages {
		if l == lang {
			return true
		}
	}
	return false
}

// NormalizeTypeName is the shared written-type → bare-name reduction:
// strips generic arguments, array suffixes, nullability markers,
// reference sigils, and namespace qualifiers, leaving the identifier
// the graph indexes type nodes under.
func NormalizeTypeName(t string) string {
	t = strings.TrimSpace(t)
	if t == "" {
		return ""
	}
	// Reference / pointer / ownership sigils and prefix keywords.
	for {
		switch {
		case strings.HasPrefix(t, "&"), strings.HasPrefix(t, "*"):
			t = strings.TrimSpace(t[1:])
			continue
		case strings.HasPrefix(t, "mut "):
			t = strings.TrimSpace(t[4:])
			continue
		case strings.HasPrefix(t, "dyn "):
			t = strings.TrimSpace(t[4:])
			continue
		case strings.HasPrefix(t, "impl "):
			t = strings.TrimSpace(t[5:])
			continue
		}
		break
	}
	// Generic arguments and array / nullability suffixes.
	if i := strings.IndexAny(t, "<(["); i >= 0 {
		t = t[:i]
	}
	t = strings.TrimSuffix(strings.TrimSuffix(t, "?"), "!")
	// Namespace / module qualifiers — keep the last segment.
	if i := strings.LastIndex(t, "::"); i >= 0 {
		t = t[i+2:]
	}
	if i := strings.LastIndex(t, "."); i >= 0 {
		t = t[i+1:]
	}
	return strings.TrimSpace(t)
}

// nodeLine returns the 1-based start line of n.
func nodeLine(n *sitter.Node) int {
	return int(n.StartPoint().Row) + 1
}

// fieldText returns the text of a named field child, "" when absent.
func fieldText(n *sitter.Node, field string, src []byte) string {
	c := n.ChildByFieldName(field)
	if c == nil {
		return ""
	}
	return c.Content(src)
}

// nameField extracts the `name` field's text — the TypeDeclName shape
// every grammar here shares.
func nameField(n *sitter.Node, src []byte) string {
	return fieldText(n, "name", src)
}

// firstChildOfType returns the first named child with the given type.
func firstChildOfType(n *sitter.Node, t string) *sitter.Node {
	for c := range n.NamedChildren() {
		if c.Type() == t {
			return c
		}
	}
	return nil
}

// identifierLike reports whether the node is a bare single-token name
// usable for scope lookup. "name" is tree-sitter-php's bare identifier
// node — it is the scope of a static `Foo::bar()` call, so it must be
// recognised for the type-qualified receiver path; no other registered
// grammar emits a "name" node in receiver / initializer position.
func identifierLike(t string) bool {
	switch t {
	case "identifier", "constant", "type_identifier", "variable_name", "local_variable", "name":
		return true
	}
	return false
}
