package tstypes

import (
	"github.com/zzet/gortex/internal/graph"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// fileFacts is the pure-syntax output of one file's binder walk. It
// carries no graph references so the parse/walk phase can run on
// worker goroutines while the apply phase owns every store interaction.
type fileFacts struct {
	file       string // graph FilePath of the analyzed file
	repoPrefix string
	imports    []Import
	calls      []callFact
	supers     []superFact
	metas      []metaFact
	aliases    []aliasFact
}

// callFact is one receiver-qualified call site with whatever receiver
// evidence the binder could ground. Exactly one of recvType /
// recvPendingCallee / recvIdent is usually set; the apply phase
// resolves them in that priority order and skips the call when none
// lands on a verified graph type node.
type callFact struct {
	line   int // 1-based call line
	method string
	// recvType is the receiver's bound type name (annotation,
	// constructor inference, or propagation through locals).
	recvType string
	// recvPendingCallee is set when the receiver local was initialised
	// from a bare call (`u = build_user()`); the apply phase resolves
	// the callee's graph return_type.
	recvPendingCallee string
	// recvIdent is set when the receiver is an identifier with no
	// binding in scope — a type-qualified (static) call candidate. Only
	// used when it resolves to a real graph type node.
	recvIdent string
	// recvChain is set when the receiver is itself a method call
	// (`a.step().done()`): it carries the inner call's receiver evidence
	// (recvType / recvPendingCallee / recvIdent / recvChain) plus the
	// inner method name. The apply phase resolves the inner method's
	// return type — applying the fluent self / trait rewrite — to type
	// the outer call's receiver, and grades the outer edge as inferred.
	recvChain *callFact
}

// superFact is one declared supertype relation, pending graph
// resolution of both endpoints.
type superFact struct {
	typeName  string
	superName string
	kind      graph.EdgeKind // empty: decide by resolved target kind
	line      int
}

// metaFact is one Node.Meta fill: stamp key=value on the symbol node
// matched by (owner, name) or by declaration line.
type metaFact struct {
	key   string
	value string
	owner string // receiver type for field stamps; "" for line-matched
	name  string // field name; "" for line-matched
	line  int    // declaration line for line-matched stamps
}

// aliasFact is one trait-use alias adaptation pending graph resolution:
// on type typeName, the name `alias` resolves to method `method` of
// trait `trait` (trait "" when the adaptation is unqualified).
type aliasFact struct {
	typeName string
	alias    string
	trait    string
	method   string
	line     int
}

// bindingState tracks one name's type through the
// single-assignment-lite discipline: the first typed binding wins, a
// later conflicting (or unknowable) rebind poisons the binding so the
// engine never resolves through a type it cannot defend.
type bindingState struct {
	typ           string
	pendingCallee string
	poisoned      bool
}

type scopeKind int

const (
	scopeFile scopeKind = iota
	scopeType
	scopeFunc
)

type scopeEnv struct {
	parent   *scopeEnv
	kind     scopeKind
	typeName string // set on scopeType
	vars     map[string]*bindingState
}

func newScope(parent *scopeEnv, kind scopeKind) *scopeEnv {
	return &scopeEnv{parent: parent, kind: kind, vars: make(map[string]*bindingState)}
}

// lookup walks the scope chain for name.
func (s *scopeEnv) lookup(name string) *bindingState {
	for e := s; e != nil; e = e.parent {
		if st, ok := e.vars[name]; ok {
			return st
		}
	}
	return nil
}

// enclosingTypeName returns the nearest type scope's name.
func (s *scopeEnv) enclosingTypeName() string {
	for e := s; e != nil; e = e.parent {
		if e.kind == scopeType {
			return e.typeName
		}
	}
	return ""
}

// nearestTypeScope returns the nearest enclosing type scope.
func (s *scopeEnv) nearestTypeScope() *scopeEnv {
	for e := s; e != nil; e = e.parent {
		if e.kind == scopeType {
			return e
		}
	}
	return nil
}

// bind applies the single-assignment-lite rule: first binding wins; a
// rebind that does not provably preserve the type degrades the binding
// to unknown (poisoned), permanently for this scope chain.
func (s *scopeEnv) bind(name string, typ, pendingCallee string) {
	if name == "" {
		return
	}
	if st := s.lookup(name); st != nil {
		if st.poisoned {
			return
		}
		if typ != st.typ || pendingCallee != st.pendingCallee {
			st.typ = ""
			st.pendingCallee = ""
			st.poisoned = true
		}
		return
	}
	s.vars[name] = &bindingState{typ: typ, pendingCallee: pendingCallee}
}

// binder runs the scope-graph walk over one parsed file.
type binder struct {
	spec  *LangSpec
	src   []byte
	facts *fileFacts
	// fieldsByType is the file-level pre-pass result: declared (and
	// conventionally initialised) field types per type name. Seeding
	// every type scope from it lets a method body resolve fields
	// declared after it — and, for Rust, fields declared on the struct
	// while the method lives in a separate impl block.
	fieldsByType map[string]map[string]string
}

func newBinder(spec *LangSpec, src []byte, facts *fileFacts) *binder {
	return &binder{spec: spec, src: src, facts: facts, fieldsByType: make(map[string]map[string]string)}
}

func (b *binder) run(root *sitter.Node) {
	if root == nil {
		return
	}
	b.prepassFields(root)
	fileScope := newScope(nil, scopeFile)
	if b.spec.Imports != nil {
		b.facts.imports = b.spec.Imports(root, b.src)
	}
	b.walk(root, fileScope)
}

// prepassFields collects field types for every type declaration in the
// file before the main walk.
func (b *binder) prepassFields(n *sitter.Node) {
	if n == nil {
		return
	}
	if b.spec.TypeDeclTypes[n.Type()] && b.spec.TypeDeclName != nil {
		if name := b.spec.TypeDeclName(n, b.src); name != "" && b.spec.Fields != nil {
			fields := b.fieldsByType[name]
			if fields == nil {
				fields = make(map[string]string)
				b.fieldsByType[name] = fields
			}
			for _, f := range b.spec.Fields(n, b.src) {
				typ := b.spec.normalize(f.Type)
				if prev, ok := fields[f.Name]; ok && prev != typ {
					// Conflicting declarations degrade to unknown —
					// same rule as local rebinds.
					fields[f.Name] = ""
					continue
				}
				fields[f.Name] = typ
				if typ != "" {
					b.facts.metas = append(b.facts.metas, metaFact{
						key: "semantic_type", value: typ, owner: name, name: f.Name,
					})
				}
			}
		}
	}
	for c := range n.NamedChildren() {
		b.prepassFields(c)
	}
}

func (b *binder) walk(n *sitter.Node, env *scopeEnv) {
	if n == nil {
		return
	}
	t := n.Type()

	if b.spec.TypeDeclTypes[t] && b.spec.TypeDeclName != nil {
		name := b.spec.TypeDeclName(n, b.src)
		if name != "" {
			if b.spec.Supertypes != nil {
				for _, s := range b.spec.Supertypes(n, b.src) {
					super := b.spec.normalize(s.Name)
					if super == "" || super == name {
						continue
					}
					b.facts.supers = append(b.facts.supers, superFact{
						typeName: name, superName: super, kind: s.Kind, line: s.Line,
					})
				}
			}
			if b.spec.TraitAliases != nil {
				for _, al := range b.spec.TraitAliases(n, b.src) {
					if al.Alias == "" || al.Method == "" {
						continue
					}
					b.facts.aliases = append(b.facts.aliases, aliasFact{
						typeName: name, alias: al.Alias,
						trait: b.spec.normalize(al.Trait), method: al.Method, line: al.Line,
					})
				}
			}
			tEnv := newScope(env, scopeType)
			tEnv.typeName = name
			for fname, ftyp := range b.fieldsByType[name] {
				tEnv.vars[fname] = &bindingState{typ: ftyp}
			}
			b.walkChildren(n, tEnv)
			return
		}
	}

	if b.spec.FuncDeclTypes[t] {
		fEnv := newScope(env, scopeFunc)
		if b.spec.Params != nil {
			for _, p := range b.spec.Params(n, b.src) {
				fEnv.vars[p.Name] = &bindingState{typ: b.spec.normalize(p.Type)}
			}
		}
		if b.spec.ReturnType != nil {
			if rt := b.spec.normalize(b.spec.ReturnType(n, b.src)); rt != "" {
				b.facts.metas = append(b.facts.metas, metaFact{
					key: "return_type", value: rt, line: nodeLine(n),
				})
			}
		}
		b.walkChildren(n, fEnv)
		return
	}

	if b.spec.LocalBinding != nil {
		if lb, ok := b.spec.LocalBinding(n, b.src); ok {
			typ := b.spec.normalize(lb.DeclType)
			pending := ""
			if typ == "" || isInferenceKeyword(typ) {
				typ, pending = b.exprType(lb.Init, env)
			}
			if lb.Field {
				if ts := env.nearestTypeScope(); ts != nil {
					ts.bind(lb.Name, typ, pending)
				}
			} else {
				env.bind(lb.Name, typ, pending)
			}
			// Fall through: the initializer may contain calls worth
			// recording.
		}
	}

	if b.spec.Call != nil {
		if recv, method, ok := b.spec.Call(n, b.src); ok && method != "" {
			if cf, grounded := b.receiverFact(recv, env); grounded {
				cf.line = nodeLine(n)
				cf.method = method
				b.facts.calls = append(b.facts.calls, cf)
			}
		}
	}

	b.walkChildren(n, env)
}

func (b *binder) walkChildren(n *sitter.Node, env *scopeEnv) {
	for c := range n.NamedChildren() {
		b.walk(c, env)
	}
}

// exprType evaluates an initializer expression to (type name, pending
// bare callee). Both empty means unknown.
func (b *binder) exprType(init *sitter.Node, env *scopeEnv) (string, string) {
	if init == nil {
		return "", ""
	}
	if b.spec.NewExprType != nil {
		if t := b.spec.normalize(b.spec.NewExprType(init, b.src)); t != "" {
			return t, ""
		}
	}
	if identifierLike(init.Type()) {
		if st := env.lookup(init.Content(b.src)); st != nil && !st.poisoned {
			return st.typ, st.pendingCallee
		}
		return "", ""
	}
	if b.spec.FieldRef != nil {
		if fname, ok := b.spec.FieldRef(init, b.src); ok {
			if ts := env.nearestTypeScope(); ts != nil {
				if st, found := ts.vars[fname]; found && !st.poisoned {
					return st.typ, st.pendingCallee
				}
			}
			return "", ""
		}
	}
	if callee := bareCallee(init, b.src); callee != "" {
		return "", callee
	}
	return "", ""
}

// receiverFact grounds a call's receiver expression. Returns ok=false
// when the receiver is structurally outside what the engine can defend
// (chained expressions, poisoned bindings, unknown shapes).
func (b *binder) receiverFact(recv *sitter.Node, env *scopeEnv) (callFact, bool) {
	if recv == nil {
		return callFact{}, false
	}
	text := recv.Content(b.src)
	if b.spec.SelfName != "" && text == b.spec.SelfName {
		if tn := env.enclosingTypeName(); tn != "" {
			return callFact{recvType: tn}, true
		}
		return callFact{}, false
	}
	if b.spec.FieldRef != nil {
		if fname, ok := b.spec.FieldRef(recv, b.src); ok {
			if ts := env.nearestTypeScope(); ts != nil {
				if st, found := ts.vars[fname]; found && !st.poisoned && st.typ != "" {
					return callFact{recvType: st.typ}, true
				}
			}
			return callFact{}, false
		}
	}
	if identifierLike(recv.Type()) {
		if st := env.lookup(text); st != nil {
			if st.poisoned {
				return callFact{}, false
			}
			if st.typ != "" {
				return callFact{recvType: st.typ}, true
			}
			if st.pendingCallee != "" {
				return callFact{recvPendingCallee: st.pendingCallee}, true
			}
			return callFact{}, false
		}
		// Unbound identifier: a static / type-qualified call candidate.
		// The apply phase only acts when it resolves to a type node.
		return callFact{recvIdent: text}, true
	}
	if b.spec.NewExprType != nil {
		if t := b.spec.normalize(b.spec.NewExprType(recv, b.src)); t != "" {
			return callFact{recvType: t}, true
		}
	}
	// Chained receiver: the receiver is itself a method call
	// (`a.step().done()`). Ground the inner call's receiver and carry its
	// method name so the apply phase can type the outer receiver from the
	// inner method's (rewritten) return type.
	if b.spec.ChainedReceivers && b.spec.Call != nil {
		if innerRecv, innerMethod, ok := b.spec.Call(recv, b.src); ok && innerMethod != "" {
			if inner, grounded := b.receiverFact(innerRecv, env); grounded {
				inner.method = innerMethod
				return callFact{recvChain: &inner}, true
			}
		}
	}
	return callFact{}, false
}

// bareCallee returns the callee name when n is a call expression whose
// function is a bare identifier; "" otherwise. Handles the grammars'
// two common shapes (call / call_expression / invocation_expression
// with a `function` field).
func bareCallee(n *sitter.Node, src []byte) string {
	switch n.Type() {
	case "call", "call_expression", "invocation_expression":
		if fn := n.ChildByFieldName("function"); fn != nil && fn.Type() == "identifier" {
			return fn.Content(src)
		}
	}
	return ""
}

// isInferenceKeyword reports whether a written "type" is actually the
// language's inference keyword and should defer to the initializer.
func isInferenceKeyword(t string) bool {
	switch t {
	case "var", "let", "auto":
		return true
	}
	return false
}
