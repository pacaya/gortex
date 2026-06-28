package languages

import (
	"strings"

	pascalforest "github.com/alexaandru/go-sitter-forest/pascal"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// Pascal / Object Pascal / Delphi. Built on the tree-sitter Pascal grammar so
// every call edge (paren `Foo(x)` AND paren-less `Foo;`), enum member, class
// member, field, constant, visibility section, and return type is recovered —
// where the prior regex extractor emitted zero call edges. Pascal identifiers
// are case-insensitive, so in-file call resolution keys on the lowercased name.
type PascalExtractor struct {
	lang *sitter.Language
}

func NewPascalExtractor() *PascalExtractor {
	return &PascalExtractor{lang: sitter.NewLanguage(pascalforest.GetLanguage())}
}

func (e *PascalExtractor) Language() string { return "pascal" }
func (e *PascalExtractor) Extensions() []string {
	return []string{".pas", ".pp", ".dpr", ".dpk", ".inc", ".lpr", ".lfm"}
}

func (e *PascalExtractor) Extract(filePath string, src []byte) (*parser.ExtractionResult, error) {
	tree, err := parser.ParseFile(src, e.lang)
	if err != nil {
		return nil, err
	}
	defer tree.Close()
	root := tree.RootNode()
	result := &parser.ExtractionResult{}

	fileNode := &graph.Node{
		ID: filePath, Kind: graph.KindFile, Name: filePath,
		FilePath: filePath, StartLine: 1, EndLine: int(root.EndPoint().Row) + 1,
		Language: "pascal",
	}
	result.Nodes = append(result.Nodes, fileNode)

	seen := map[string]bool{}
	// procIndex maps a lowercased callable name (bare and class-qualified) to
	// its node ID, so a same-file call resolves directly to the definition.
	procIndex := map[string]string{}

	// Pass 1 — declarations. Builds procIndex for pass 2.
	walkNodes(root, func(n *sitter.Node) {
		switch n.Type() {
		case "unit", "program", "library", "package":
			e.emitPascalModule(n, src, filePath, fileNode.ID, result, seen)
		case "declUses":
			e.emitPascalUses(n, src, filePath, fileNode.ID, result)
		case "declType":
			e.emitPascalType(n, src, filePath, fileNode.ID, result, seen, procIndex)
		case "declConst":
			e.emitPascalConst(n, src, filePath, fileNode.ID, result, seen)
		case "defProc":
			e.emitPascalProcDef(n, src, filePath, fileNode.ID, result, seen, procIndex)
		}
	})

	// Pass 2 — call edges from each implementation body, now that every
	// in-file callable is known.
	walkNodes(root, func(n *sitter.Node) {
		if n.Type() == "defProc" {
			e.emitPascalCalls(n, src, filePath, result, procIndex)
		}
	})

	// Same-file constant value references → impact-radius reads.
	captureValueRefCandidates(result, root, filePath, src)
	captureFnValueCandidates(result, root, filePath, src)

	return result, nil
}

// --- declaration emitters ------------------------------------------------

func (e *PascalExtractor) emitPascalModule(n *sitter.Node, src []byte, filePath, fileID string, result *parser.ExtractionResult, seen map[string]bool) {
	name := pascalChildText(n, src, "moduleName")
	if name == "" {
		return
	}
	id := filePath + "::" + name
	if seen[id] {
		return
	}
	seen[id] = true
	line := int(n.StartPoint().Row) + 1
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindPackage, Name: name,
		FilePath: filePath, StartLine: line, EndLine: int(n.EndPoint().Row) + 1, Language: "pascal",
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: line,
	})
}

func (e *PascalExtractor) emitPascalUses(n *sitter.Node, src []byte, filePath, fileID string, result *parser.ExtractionResult) {
	for i, _nc := 0, int(n.ChildCount()); i < _nc; i++ {
		c := n.Child(i)
		if c.Type() != "moduleName" {
			continue
		}
		mod := strings.TrimSpace(c.Content(src))
		if mod == "" {
			continue
		}
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileID, To: "unresolved::import::" + mod,
			Kind: graph.EdgeImports, FilePath: filePath, Line: int(c.StartPoint().Row) + 1,
		})
	}
}

func (e *PascalExtractor) emitPascalConst(n *sitter.Node, src []byte, filePath, fileID string, result *parser.ExtractionResult, seen map[string]bool) {
	name := pascalChildText(n, src, "identifier")
	if name == "" {
		return
	}
	id, ok := disambiguateID(seen, filePath+"::"+name, int(n.StartPoint().Row)+1)
	if !ok {
		return
	}
	line := int(n.StartPoint().Row) + 1
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindConstant, Name: name,
		FilePath: filePath, StartLine: line, EndLine: int(n.EndPoint().Row) + 1, Language: "pascal",
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: line,
	})
}

// emitPascalType handles `TName = <type>`: an enum, a class/object/record, an
// interface, or a plain alias. Enum members, class fields and methods, the base
// type, and visibility are emitted; class methods are indexed for call
// resolution.
func (e *PascalExtractor) emitPascalType(n *sitter.Node, src []byte, filePath, fileID string, result *parser.ExtractionResult, seen map[string]bool, procIndex map[string]string) {
	name := pascalChildText(n, src, "identifier")
	if name == "" {
		return
	}
	typeID := filePath + "::" + name
	if seen[typeID] {
		return
	}
	body := pascalTypeBody(n)
	kind := graph.KindType
	if body != nil && body.Type() == "declInterface" {
		kind = graph.KindInterface
	}
	seen[typeID] = true
	line := int(n.StartPoint().Row) + 1
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: typeID, Kind: kind, Name: name,
		FilePath: filePath, StartLine: line, EndLine: int(n.EndPoint().Row) + 1, Language: "pascal",
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: typeID, Kind: graph.EdgeDefines, FilePath: filePath, Line: line,
	})
	if body == nil {
		return
	}
	switch body.Type() {
	case "declEnum":
		e.emitPascalEnumMembers(body, src, filePath, typeID, result, seen)
	case "declClass", "declRecord", "declObject", "declInterface":
		e.emitPascalClassBody(body, src, filePath, name, typeID, result, seen, procIndex)
	}
}

func (e *PascalExtractor) emitPascalEnumMembers(enum *sitter.Node, src []byte, filePath, typeID string, result *parser.ExtractionResult, seen map[string]bool) {
	for i, _nc := 0, int(enum.ChildCount()); i < _nc; i++ {
		c := enum.Child(i)
		if c.Type() != "declEnumValue" {
			continue
		}
		mname := pascalChildText(c, src, "identifier")
		if mname == "" {
			mname = strings.TrimSpace(c.Content(src))
		}
		if mname == "" {
			continue
		}
		line := int(c.StartPoint().Row) + 1
		id, ok := disambiguateID(seen, filePath+"::"+mname, line)
		if !ok {
			continue
		}
		result.Nodes = append(result.Nodes, &graph.Node{
			ID: id, Kind: graph.KindEnumMember, Name: mname,
			FilePath: filePath, StartLine: line, EndLine: line, Language: "pascal",
			Meta: map[string]any{"enum": typeID},
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From: id, To: typeID, Kind: graph.EdgeMemberOf, FilePath: filePath, Line: line,
		})
	}
}

func (e *PascalExtractor) emitPascalClassBody(body *sitter.Node, src []byte, filePath, typeName, typeID string, result *parser.ExtractionResult, seen map[string]bool, procIndex map[string]string) {
	// Base type(s): typeref children directly under the class header.
	for i, _nc := 0, int(body.ChildCount()); i < _nc; i++ {
		c := body.Child(i)
		if c.Type() == "typeref" {
			base := strings.TrimSpace(c.Content(src))
			if base != "" {
				result.Edges = append(result.Edges, &graph.Edge{
					From: typeID, To: "unresolved::" + base, Kind: graph.EdgeExtends,
					FilePath: filePath, Line: int(c.StartPoint().Row) + 1,
				})
			}
		}
	}
	visibility := VisibilityPublic
	var walk func(n *sitter.Node)
	walk = func(n *sitter.Node) {
		for i, _nc := 0, int(n.ChildCount()); i < _nc; i++ {
			c := n.Child(i)
			switch c.Type() {
			case "declSection":
				walk(c)
			case "kPrivate":
				visibility = VisibilityPrivate
			case "kProtected":
				visibility = VisibilityProtected
			case "kPublic", "kPublished":
				visibility = VisibilityPublic
			case "declField":
				e.emitPascalField(c, src, filePath, typeName, typeID, visibility, result, seen)
			case "declProc":
				e.emitPascalMethodDecl(c, src, filePath, typeName, typeID, visibility, result, seen, procIndex)
			}
		}
	}
	walk(body)
}

func (e *PascalExtractor) emitPascalField(n *sitter.Node, src []byte, filePath, typeName, typeID, visibility string, result *parser.ExtractionResult, seen map[string]bool) {
	fname := pascalChildText(n, src, "identifier")
	if fname == "" {
		return
	}
	line := int(n.StartPoint().Row) + 1
	id, ok := disambiguateID(seen, filePath+"::"+typeName+"."+fname, line)
	if !ok {
		return
	}
	meta := map[string]any{"receiver": typeName, "visibility": visibility}
	if ft := pascalChildText(n, src, "type"); ft != "" {
		meta["field_type"] = ft
	}
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindField, Name: fname,
		FilePath: filePath, StartLine: line, EndLine: line, Language: "pascal", Meta: meta,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: id, To: typeID, Kind: graph.EdgeMemberOf, FilePath: filePath, Line: line,
	})
}

// emitPascalMethodDecl emits a method node from a class-body declProc signature.
func (e *PascalExtractor) emitPascalMethodDecl(n *sitter.Node, src []byte, filePath, typeName, typeID, visibility string, result *parser.ExtractionResult, seen map[string]bool, procIndex map[string]string) {
	mname := pascalProcLocalName(n, src)
	if mname == "" {
		return
	}
	line := int(n.StartPoint().Row) + 1
	id := filePath + "::" + typeName + "." + mname
	pascalIndex(procIndex, mname, id)
	pascalIndex(procIndex, typeName+"."+mname, id)
	if seen[id] {
		return
	}
	seen[id] = true
	meta := map[string]any{"receiver": typeName, "visibility": visibility}
	if rt := pascalReturnType(n, src); rt != "" {
		meta["return_type"] = rt
	} else if pascalFirstChild(n, "kConstructor") != nil {
		// A constructor yields an instance of its class — seed the
		// chained-factory walker (`TFoo.Create.Configure`) from the type.
		meta["return_type"] = typeName
	}
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindMethod, Name: mname,
		FilePath: filePath, StartLine: line, EndLine: int(n.EndPoint().Row) + 1, Language: "pascal", Meta: meta,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: filePath, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: line,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: id, To: typeID, Kind: graph.EdgeMemberOf, FilePath: filePath, Line: line,
	})
}

// emitPascalProcDef emits a node for an implementation proc/function. A
// class-qualified one (`function TFoo.Bar`) whose class declared it earlier is
// not re-emitted, but is indexed so its body's calls resolve; a free proc or an
// out-of-file method is emitted here.
func (e *PascalExtractor) emitPascalProcDef(n *sitter.Node, src []byte, filePath, fileID string, result *parser.ExtractionResult, seen map[string]bool, procIndex map[string]string) {
	decl := pascalFirstChild(n, "declProc")
	if decl == nil {
		return
	}
	qualified, owner, bare := pascalProcQualifiedName(decl, src)
	if bare == "" {
		return
	}
	id := filePath + "::" + qualified
	line := int(n.StartPoint().Row) + 1
	pascalIndex(procIndex, bare, id)
	pascalIndex(procIndex, qualified, id)
	if seen[id] {
		return
	}
	seen[id] = true
	kind := graph.KindFunction
	meta := map[string]any{}
	if owner != "" {
		kind = graph.KindMethod
		meta["receiver"] = owner
	}
	if rt := pascalReturnType(decl, src); rt != "" {
		meta["return_type"] = rt
	}
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: kind, Name: bare,
		FilePath: filePath, StartLine: line, EndLine: int(n.EndPoint().Row) + 1, Language: "pascal", Meta: meta,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: line,
	})
	if owner != "" {
		result.Edges = append(result.Edges, &graph.Edge{
			From: id, To: filePath + "::" + owner, Kind: graph.EdgeMemberOf, FilePath: filePath, Line: line,
		})
	}
}

// --- call edges ----------------------------------------------------------

func (e *PascalExtractor) emitPascalCalls(n *sitter.Node, src []byte, filePath string, result *parser.ExtractionResult, procIndex map[string]string) {
	decl := pascalFirstChild(n, "declProc")
	if decl == nil {
		return
	}
	qualified, _, bare := pascalProcQualifiedName(decl, src)
	if bare == "" {
		return
	}
	fromID := filePath + "::" + qualified
	block := pascalFirstChild(n, "block")
	if block == nil {
		return
	}
	walkNodes(block, func(c *sitter.Node) {
		switch c.Type() {
		case "exprCall":
			// First named child is the callee (identifier or genericDot).
			if callee := pascalCallTargetName(c, src); callee != "" {
				e.emitPascalCallEdge(fromID, callee, filePath, int(c.StartPoint().Row)+1, result, procIndex)
			} else if dot := pascalFirstChild(c, "exprDot"); dot != nil {
				// A chained `recv.Method()` whose receiver is itself a call --
				// a factory chain Builder().WithX().Build(). The bare-callee path
				// above misses these; emit the method with the receiver text so
				// the shared walker can type the chain.
				method, receiver := pascalDotMethodReceiver(dot, src)
				if method != "" && strings.Contains(receiver, "(") {
					edge := &graph.Edge{
						From: fromID, To: "unresolved::*." + method,
						Kind: graph.EdgeCalls, FilePath: filePath, Line: int(c.StartPoint().Row) + 1,
					}
					stampFactoryChainReceiver(edge, receiver, resolveChainType(receiver, nil, result))
					result.Edges = append(result.Edges, edge)
				}
			}
		case "statement":
			// A statement whose only named child is a bare identifier /
			// genericDot is a paren-less procedure call (`Baz;`, `Self.Init;`).
			if callee := pascalParenlessCallee(c, src); callee != "" {
				e.emitPascalCallEdge(fromID, callee, filePath, int(c.StartPoint().Row)+1, result, procIndex)
			}
		}
	})
}

func (e *PascalExtractor) emitPascalCallEdge(fromID, callee, filePath string, line int, result *parser.ExtractionResult, procIndex map[string]string) {
	bare := callee
	if i := strings.LastIndexByte(bare, '.'); i >= 0 {
		bare = bare[i+1:]
	}
	if bare == "" {
		return
	}
	if target, ok := procIndex[strings.ToLower(bare)]; ok {
		if target == fromID {
			return // self-reference noise
		}
		result.Edges = append(result.Edges, &graph.Edge{
			From: fromID, To: target, Kind: graph.EdgeCalls, FilePath: filePath, Line: line,
			Origin: graph.OriginASTResolved,
		})
		return
	}
	result.Edges = append(result.Edges, &graph.Edge{
		From: fromID, To: "unresolved::*." + bare, Kind: graph.EdgeCalls, FilePath: filePath, Line: line,
		Origin: graph.OriginTextMatched,
	})
}

// --- small helpers -------------------------------------------------------

// pascalFirstChild returns the first direct child of n with the given type.
func pascalFirstChild(n *sitter.Node, typ string) *sitter.Node {
	for i, _nc := 0, int(n.ChildCount()); i < _nc; i++ {
		if c := n.Child(i); c.Type() == typ {
			return c
		}
	}
	return nil
}

// pascalChildText returns the trimmed content of n's first child of typ.
func pascalChildText(n *sitter.Node, src []byte, typ string) string {
	if c := pascalFirstChild(n, typ); c != nil {
		return strings.TrimSpace(c.Content(src))
	}
	return ""
}

// pascalTypeBody returns the declClass/declRecord/declEnum/declInterface node a
// declType resolves to, unwrapping the intermediate `type` wrapper.
func pascalTypeBody(declType *sitter.Node) *sitter.Node {
	for i, _nc := 0, int(declType.ChildCount()); i < _nc; i++ {
		c := declType.Child(i)
		switch c.Type() {
		case "declClass", "declRecord", "declObject", "declInterface", "declEnum":
			return c
		case "type":
			for j, _nc := 0, int(c.ChildCount()); j < _nc; j++ {
				if inner := c.Child(j); inner.IsNamed() {
					return inner
				}
			}
		}
	}
	return nil
}

// pascalProcLocalName returns the bare name of a declProc (the identifier; for a
// class-body signature there is no genericDot).
func pascalProcLocalName(decl *sitter.Node, src []byte) string {
	if gd := pascalFirstChild(decl, "genericDot"); gd != nil {
		_, _, bare := splitPascalDotted(strings.TrimSpace(gd.Content(src)))
		return bare
	}
	return pascalChildText(decl, src, "identifier")
}

// pascalProcQualifiedName returns (qualifiedName, owner, bareName) for a
// declProc: `TFoo.Bar` → ("TFoo.Bar","TFoo","Bar"); `Hello` → ("Hello","","Hello").
func pascalProcQualifiedName(decl *sitter.Node, src []byte) (qualified, owner, bare string) {
	if gd := pascalFirstChild(decl, "genericDot"); gd != nil {
		owner, _, bare = splitPascalDotted(strings.TrimSpace(gd.Content(src)))
		if owner != "" {
			return owner + "." + bare, owner, bare
		}
		return bare, "", bare
	}
	bare = pascalChildText(decl, src, "identifier")
	return bare, "", bare
}

// splitPascalDotted splits `A.B.C` into (owner="A.B", parent="A.B", last="C").
func splitPascalDotted(s string) (owner, parent, last string) {
	if i := strings.LastIndexByte(s, '.'); i >= 0 {
		return s[:i], s[:i], s[i+1:]
	}
	return "", "", s
}

// pascalReturnType returns the declProc's typeref return type (functions only).
func pascalReturnType(decl *sitter.Node, src []byte) string {
	if tr := pascalFirstChild(decl, "typeref"); tr != nil {
		return strings.TrimSpace(tr.Content(src))
	}
	return ""
}

// pascalCallTargetName returns the callee name of an exprCall (its first named
// identifier / genericDot child).
// pascalDotMethodReceiver splits an exprDot `recv.Method` into the method
// name (its trailing identifier) and the receiver text (everything before).
func pascalDotMethodReceiver(dot *sitter.Node, src []byte) (method, receiver string) {
	for i := int(dot.ChildCount()) - 1; i >= 0; i-- {
		if dot.Child(i).Type() == "identifier" {
			method = strings.TrimSpace(dot.Child(i).Content(src))
			break
		}
	}
	if obj := dot.NamedChild(0); obj != nil {
		receiver = strings.TrimSpace(obj.Content(src))
	}
	return method, receiver
}

func pascalCallTargetName(call *sitter.Node, src []byte) string {
	for i, _nc := 0, int(call.ChildCount()); i < _nc; i++ {
		c := call.Child(i)
		if c.Type() == "identifier" || c.Type() == "genericDot" {
			return strings.TrimSpace(c.Content(src))
		}
	}
	return ""
}

// pascalParenlessCallee returns the callee of a paren-less call statement — a
// statement whose sole named child is an identifier / genericDot — or "".
func pascalParenlessCallee(stmt *sitter.Node, src []byte) string {
	var named []*sitter.Node
	for i, _nc := 0, int(stmt.ChildCount()); i < _nc; i++ {
		if c := stmt.Child(i); c.IsNamed() {
			named = append(named, c)
		}
	}
	if len(named) != 1 {
		return ""
	}
	if t := named[0].Type(); t == "identifier" || t == "genericDot" {
		return strings.TrimSpace(named[0].Content(src))
	}
	return ""
}

// pascalIndex records a lowercased callable name → id mapping (first writer
// wins, so a definition never shadows an earlier class declaration's id).
func pascalIndex(idx map[string]string, name, id string) {
	if name == "" {
		return
	}
	k := strings.ToLower(name)
	if _, ok := idx[k]; !ok {
		idx[k] = id
	}
}

var _ parser.Extractor = (*PascalExtractor)(nil)
