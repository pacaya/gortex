package languages

import (
	"regexp"
	"strings"

	rforest "github.com/alexaandru/go-sitter-forest/r"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/forest"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// R extractor uses forest's tree-sitter grammar (with bundled
// tags.scm) for definitions and call edges, then layers regex passes
// for the R-specific idioms tags.scm doesn't categorize: `library()`
// / `require()` / `source()` calls become EdgeImports rather than
// EdgeCalls. Top-level assignments are also rescued by regex —
// tags.scm captures functions but doesn't always tag plain value
// bindings as variables.
var (
	rLibraryRe   = regexp.MustCompile(`(?m)\blibrary\(\s*"?'?(\w+)"?'?\s*\)`)
	rRequireRe   = regexp.MustCompile(`(?m)\brequire\(\s*"?'?(\w+)"?'?\s*\)`)
	rSourceRe    = regexp.MustCompile(`(?m)\bsource\(\s*["']([^"']+)["']\s*\)`)
	rVarAssignRe = regexp.MustCompile(`(?m)^(\w[\w.]*)\s*(?:<-|=)\s*(?:[^f]|f[^u]|fu[^n])`)
)

// RExtractor extracts R source via forest + regex idiom layer.
type RExtractor struct {
	forest *forest.Extractor
	lang   *sitter.Language
}

func NewRExtractor() *RExtractor {
	return &RExtractor{
		forest: forest.New("r", []string{".R", ".r", ".Rmd"}, rforest.GetLanguage, rforest.GetQuery),
		lang:   sitter.NewLanguage(rforest.GetLanguage()),
	}
}

// rS3Generics are the common base-R S3 generic functions; a function named
// `<generic>.<class>` is treated as an S3 method dispatched from one of these.
var rS3Generics = map[string]bool{
	"print": true, "format": true, "summary": true, "plot": true, "predict": true,
	"as.character": true, "as.data.frame": true, "as.list": true, "as.numeric": true,
	"length": true, "names": true, "str": true, "c": true, "mean": true, "median": true,
	"toString": true, "update": true, "coef": true, "residuals": true, "fitted": true,
}

func (e *RExtractor) Language() string     { return "r" }
func (e *RExtractor) Extensions() []string { return []string{".R", ".r", ".Rmd"} }

func (e *RExtractor) Extract(filePath string, src []byte) (*parser.ExtractionResult, error) {
	res, err := e.forest.Extract(filePath, src)
	if err != nil {
		return nil, err
	}

	seen := make(map[string]bool)
	for _, n := range res.Nodes {
		seen[n.ID] = true
	}

	// R class systems (S4 setClass/setGeneric/setMethod, R6Class /
	// setRefClass, S3 generic.class methods) and generic→method dispatch.
	// Runs before the var-assign rescue so a class binding is typed as a
	// class, not a plain variable.
	e.extractRClassSystems(src, filePath, res, seen)

	// Namespace-qualified (`dplyr::filter`) and `$`-dispatch (`obj$method`)
	// calls: preserve the package qualifier / receiver the tag pass strips.
	e.extractRNamespaceCalls(src, filePath, res)

	// Idiom imports: library(X) / require(X) / source("X.R").
	for _, re := range []*regexp.Regexp{rLibraryRe, rRequireRe} {
		for _, m := range re.FindAllSubmatchIndex(src, -1) {
			mod := string(src[m[2]:m[3]])
			line := lineAt(src, m[0])
			res.Edges = append(res.Edges, &graph.Edge{
				From: filePath, To: "unresolved::import::" + mod,
				Kind: graph.EdgeImports, FilePath: filePath, Line: line,
			})
		}
	}
	for _, m := range rSourceRe.FindAllSubmatchIndex(src, -1) {
		path := string(src[m[2]:m[3]])
		line := lineAt(src, m[0])
		res.Edges = append(res.Edges, &graph.Edge{
			From: filePath, To: "unresolved::import::" + path,
			Kind: graph.EdgeImports, FilePath: filePath, Line: line,
		})
	}

	// Top-level value bindings (`name <- value` / `name = value`)
	// that aren't function assignments — tags.scm typically only
	// captures the function-binding shape.
	for _, m := range rVarAssignRe.FindAllSubmatchIndex(src, -1) {
		name := string(src[m[2]:m[3]])
		if isRKeyword(name) {
			continue
		}
		line := lineAt(src, m[0])
		id := filePath + "::" + name
		if seen[id] {
			continue
		}
		seen[id] = true
		res.Nodes = append(res.Nodes, &graph.Node{
			ID: id, Kind: graph.KindVariable, Name: name,
			FilePath: filePath, StartLine: line, EndLine: line,
			Language: "r",
		})
		res.Edges = append(res.Edges, &graph.Edge{
			From: filePath, To: id, Kind: graph.EdgeDefines,
			FilePath: filePath, Line: line,
		})
	}

	// Databricks source-format `.R` / `.r` notebooks: emit cell-level
	// nodes alongside the regular R symbol nodes. No-op for ordinary
	// R scripts.
	MaybeEnrichDatabricks(filePath, filePath, src, res)

	return res, nil
}

// extractRClassSystems parses the R tree and models the three class systems and
// their dispatch: S4 (setClass + `contains` inheritance, setGeneric, setMethod
// → a generic→method dispatch edge), R6/Reference classes (the bound name typed
// as a class), and S3 (`generic.class` methods dispatched from a base generic).
// The dispatch edges are the win: a call to a generic reaches its methods, which
// plain symbol extraction leaves disconnected.
func (e *RExtractor) extractRClassSystems(src []byte, filePath string, res *parser.ExtractionResult, seen map[string]bool) {
	tree, err := parser.ParseFile(src, e.lang)
	if err != nil {
		return
	}
	defer tree.Close()

	emitType := func(name string, line int, system string) string {
		if name == "" {
			return ""
		}
		id := filePath + "::" + name
		if seen[id] {
			return id
		}
		seen[id] = true
		res.Nodes = append(res.Nodes, &graph.Node{
			ID: id, Kind: graph.KindType, Name: name,
			FilePath: filePath, StartLine: line, EndLine: line, Language: "r",
			Meta: map[string]any{"class_system": system},
		})
		res.Edges = append(res.Edges, &graph.Edge{
			From: filePath, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: line,
		})
		return id
	}
	emitGeneric := func(name string, line int) string {
		id := filePath + "::" + name
		if !seen[id] {
			seen[id] = true
			res.Nodes = append(res.Nodes, &graph.Node{
				ID: id, Kind: graph.KindFunction, Name: name,
				FilePath: filePath, StartLine: line, EndLine: line, Language: "r",
				Meta: map[string]any{"r_generic": true},
			})
			res.Edges = append(res.Edges, &graph.Edge{
				From: filePath, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: line,
			})
		}
		return id
	}
	dispatch := func(genericID, methodID string, line int, via string) {
		res.Edges = append(res.Edges, &graph.Edge{
			From: genericID, To: methodID, Kind: graph.EdgeCalls, FilePath: filePath, Line: line,
			Origin: graph.OriginASTInferred,
			Meta:   map[string]any{"via": via, "dispatch": true},
		})
	}

	var walk func(n *sitter.Node)
	walk = func(n *sitter.Node) {
		switch n.Type() {
		case "call":
			callee := rCallee(n, src)
			args := rCallArgs(n, src)
			line := int(n.StartPoint().Row) + 1
			switch callee {
			case "setClass":
				if cls := rFirstPositional(args); cls != "" {
					id := emitType(cls, line, "S4")
					if base := rNamedArg(args, "contains"); base != "" && id != "" {
						res.Edges = append(res.Edges, &graph.Edge{
							From: id, To: "unresolved::" + base, Kind: graph.EdgeExtends,
							FilePath: filePath, Line: line, Meta: map[string]any{"class_system": "S4"},
						})
					}
				}
			case "setGeneric":
				if g := rFirstPositional(args); g != "" {
					emitGeneric(g, line)
				}
			case "setMethod":
				pos := rPositionals(args)
				if len(pos) >= 2 && pos[0] != "" && pos[1] != "" {
					generic, cls := pos[0], pos[1]
					mid := filePath + "::" + generic + "." + cls
					if !seen[mid] {
						seen[mid] = true
						res.Nodes = append(res.Nodes, &graph.Node{
							ID: mid, Kind: graph.KindMethod, Name: generic,
							FilePath: filePath, StartLine: line, EndLine: int(n.EndPoint().Row) + 1, Language: "r",
							Meta: map[string]any{"receiver": cls, "class_system": "S4", "dispatch_class": cls},
						})
						res.Edges = append(res.Edges, &graph.Edge{
							From: filePath, To: mid, Kind: graph.EdgeDefines, FilePath: filePath, Line: line,
						})
						res.Edges = append(res.Edges, &graph.Edge{
							From: mid, To: "unresolved::" + cls, Kind: graph.EdgeMemberOf, FilePath: filePath, Line: line,
						})
					}
					dispatch(emitGeneric(generic, line), mid, line, "s4_dispatch")
				}
			case "R6Class", "setRefClass":
				cls := rFirstPositional(args)
				if cls == "" {
					cls = rBindingName(n, src) // fall back to the LHS binding
				}
				emitType(cls, line, rClassSystemName(callee))
			}
		case "binary_operator":
			// S3 method: `generic.class <- function(...)`.
			if lhs := rBindingName(n, src); lhs != "" {
				if rhs := rBindingValueType(n); rhs == "function_definition" {
					if dot := strings.IndexByte(lhs, '.'); dot > 0 {
						generic := lhs[:dot]
						if rS3Generics[generic] {
							mid := filePath + "::" + lhs
							line := int(n.StartPoint().Row) + 1
							dispatch(emitGeneric(generic, line), mid, line, "s3_dispatch")
						}
					}
				}
			}
		}
		for i, _nc := 0, int(n.ChildCount()); i < _nc; i++ {
			walk(n.Child(i))
		}
	}
	walk(tree.RootNode())
}

// extractRNamespaceCalls upgrades the calls the tag pass records with a
// stripped, bare callee. A `pkg::fn(...)` namespace call is rewritten from the
// bare `unresolved::fn` to `unresolved::pkg::fn` (carrying r_namespace) so the
// package qualifier is preserved — a `dplyr::filter` call stays distinguishable
// from a base-R `filter`. An `obj$method(...)` extract-dispatch call, which the
// tag pass drops entirely, gets its own edge carrying the receiver. This is the
// call provenance a tags.scm-only path discards.
func (e *RExtractor) extractRNamespaceCalls(src []byte, filePath string, res *parser.ExtractionResult) {
	tree, err := parser.ParseFile(src, e.lang)
	if err != nil {
		return
	}
	defer tree.Close()

	// Index the tag pass's bare call edges by (callee, line) so a namespace
	// call can claim and rewrite the matching edge in place rather than
	// emitting a duplicate.
	type callKey struct {
		name string
		line int
	}
	bare := map[callKey]*graph.Edge{}
	for _, ed := range res.Edges {
		if ed.Kind == graph.EdgeCalls && strings.HasPrefix(ed.To, "unresolved::") {
			name := strings.TrimPrefix(ed.To, "unresolved::")
			bare[callKey{name, ed.Line}] = ed
		}
	}

	var walk func(n *sitter.Node)
	walk = func(n *sitter.Node) {
		if n.Type() == "call" {
			fn := n.ChildByFieldName("function")
			if fn == nil && n.ChildCount() > 0 {
				fn = n.Child(0)
			}
			line := int(n.StartPoint().Row) + 1
			if fn != nil {
				switch fn.Type() {
				case "namespace_operator":
					if pkg, name := rNamespaceParts(fn, src); pkg != "" && name != "" {
						qualified := "unresolved::" + pkg + "::" + name
						if ed := bare[callKey{name, line}]; ed != nil {
							ed.To = qualified
							if ed.Meta == nil {
								ed.Meta = map[string]any{}
							}
							ed.Meta["r_namespace"] = pkg
						} else {
							res.Edges = append(res.Edges, &graph.Edge{
								From: filePath, To: qualified, Kind: graph.EdgeCalls,
								FilePath: filePath, Line: line, Origin: graph.OriginASTResolved,
								Meta: map[string]any{"r_namespace": pkg},
							})
						}
					}
				case "extract_operator":
					if recv, name := rNamespaceParts(fn, src); name != "" {
						res.Edges = append(res.Edges, &graph.Edge{
							From: filePath, To: "unresolved::" + name, Kind: graph.EdgeCalls,
							FilePath: filePath, Line: line, Origin: graph.OriginASTInferred,
							Meta: map[string]any{"via": "dollar_dispatch", "r_receiver": recv},
						})
					}
				}
			}
		}
		for i, _nc := 0, int(n.ChildCount()); i < _nc; i++ {
			walk(n.Child(i))
		}
	}
	walk(tree.RootNode())
}

// rNamespaceParts returns the (lhs, rhs) identifier text of an R
// namespace_operator (`pkg::fn`, `pkg:::fn`) or extract_operator (`obj$field`)
// node — the package + function, or the receiver + member.
func rNamespaceParts(op *sitter.Node, src []byte) (string, string) {
	var ids []string
	for i, _nc := 0, int(op.ChildCount()); i < _nc; i++ {
		if c := op.Child(i); c != nil && c.Type() == "identifier" {
			ids = append(ids, c.Content(src))
		}
	}
	if len(ids) >= 2 {
		return ids[0], ids[1]
	}
	return "", ""
}

// rCallee returns the function name of an R call (its leading identifier).
func rCallee(call *sitter.Node, src []byte) string {
	for i, _nc := 0, int(call.ChildCount()); i < _nc; i++ {
		c := call.Child(i)
		if c.Type() == "identifier" {
			return c.Content(src)
		}
		if c.Type() == "arguments" {
			break
		}
	}
	return ""
}

// rArg is one parsed call argument: Name is "" for positional; Str is the
// string-literal value when the argument is a string; ValueType is the value
// node's type.
type rArg struct {
	Name      string
	Str       string
	ValueType string
}

// rCallArgs parses an R call's argument list into name/value pairs.
func rCallArgs(call *sitter.Node, src []byte) []rArg {
	var argsNode *sitter.Node
	for i, _nc := 0, int(call.ChildCount()); i < _nc; i++ {
		if call.Child(i).Type() == "arguments" {
			argsNode = call.Child(i)
			break
		}
	}
	if argsNode == nil {
		return nil
	}
	var out []rArg
	for i, _nc := 0, int(argsNode.NamedChildCount()); i < _nc; i++ {
		arg := argsNode.NamedChild(i)
		if arg.Type() != "argument" {
			continue
		}
		var kids []*sitter.Node
		for j, _nc := 0, int(arg.NamedChildCount()); j < _nc; j++ {
			kids = append(kids, arg.NamedChild(j))
		}
		var a rArg
		var value *sitter.Node
		if len(kids) == 2 && kids[0].Type() == "identifier" {
			a.Name = kids[0].Content(src)
			value = kids[1]
		} else if len(kids) == 1 {
			value = kids[0]
		}
		if value != nil {
			a.ValueType = value.Type()
			if value.Type() == "string" {
				a.Str = rStringContent(value, src)
			}
		}
		out = append(out, a)
	}
	return out
}

func rStringContent(strNode *sitter.Node, src []byte) string {
	for i, _nc := 0, int(strNode.NamedChildCount()); i < _nc; i++ {
		if c := strNode.NamedChild(i); c.Type() == "string_content" {
			return c.Content(src)
		}
	}
	return strings.Trim(strNode.Content(src), `"'`)
}

// rFirstPositional returns the first positional string argument.
func rFirstPositional(args []rArg) string {
	for _, a := range args {
		if a.Name == "" && a.Str != "" {
			return a.Str
		}
	}
	return ""
}

// rPositionals returns the string values of the positional arguments in order.
func rPositionals(args []rArg) []string {
	var out []string
	for _, a := range args {
		if a.Name == "" {
			out = append(out, a.Str)
		}
	}
	return out
}

// rNamedArg returns the string value of the named argument, or "".
func rNamedArg(args []rArg, name string) string {
	for _, a := range args {
		if a.Name == name {
			return a.Str
		}
	}
	return ""
}

// rBindingName returns the LHS identifier of an `x <- ...` / `x = ...` /
// `... -> x` assignment expressed as a binary_operator, or "".
func rBindingName(bin *sitter.Node, src []byte) string {
	if bin.NamedChildCount() < 2 {
		return ""
	}
	lhs := bin.NamedChild(0)
	if lhs != nil && lhs.Type() == "identifier" {
		return lhs.Content(src)
	}
	return ""
}

// rBindingValueType returns the type of the RHS value of an assignment.
func rBindingValueType(bin *sitter.Node) string {
	if bin.NamedChildCount() < 2 {
		return ""
	}
	if rhs := bin.NamedChild(int(bin.NamedChildCount()) - 1); rhs != nil {
		return rhs.Type()
	}
	return ""
}

func rClassSystemName(callee string) string {
	if callee == "R6Class" {
		return "R6"
	}
	return "R5"
}

func isRKeyword(s string) bool {
	switch s {
	case "if", "else", "for", "while", "repeat", "in", "next", "break",
		"return", "function", "TRUE", "FALSE", "NULL", "NA", "Inf", "NaN",
		"library", "require", "source":
		return true
	}
	return false
}

var _ parser.Extractor = (*RExtractor)(nil)
