package languages

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
	"github.com/zzet/gortex/internal/parser/tsitter/elixir"
)

// elixirKeywords are call targets that represent language constructs, not user calls.
var elixirKeywords = map[string]bool{
	"defmodule": true, "def": true, "defp": true,
	"import": true, "alias": true, "use": true, "require": true,
	"defmacro": true, "defmacrop": true, "defguard": true,
	"defstruct": true, "defprotocol": true, "defimpl": true,
	"defdelegate": true, "defexception": true, "defoverridable": true,
	"test": true, "describe": true, "setup": true,
}

// ElixirExtractor extracts Elixir source files. Elixir's grammar
// represents nearly everything as `call` nodes, so extraction is a
// single manual walkNode cursor: structural constructs (modules,
// defs, imports, attributes) are dispatched as the walk descends, and
// call sites inside each def body are collected in that same pass.
type ElixirExtractor struct {
	lang *sitter.Language
}

func NewElixirExtractor() *ElixirExtractor {
	return &ElixirExtractor{lang: elixir.GetLanguage()}
}

func (e *ElixirExtractor) Language() string     { return "elixir" }
func (e *ElixirExtractor) Extensions() []string { return []string{".ex", ".exs"} }

func (e *ElixirExtractor) Extract(filePath string, src []byte) (*parser.ExtractionResult, error) {
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
		Language: "elixir",
	}
	result.Nodes = append(result.Nodes, fileNode)

	seen := make(map[string]bool)

	// Walk the AST manually to handle Elixir's call-based structure.
	// A single cursor: structural constructs are extracted as the walk
	// descends and call sites are collected from each def body.
	e.walkNode(root, src, filePath, fileNode.ID, "", result, seen)

	return result, nil
}

// walkNode recursively walks the AST to extract modules, functions, imports, and attributes.
func (e *ElixirExtractor) walkNode(node *sitter.Node, src []byte, filePath, fileID, currentModule string, result *parser.ExtractionResult, seen map[string]bool) {
	if node == nil {
		return
	}

	if node.Type() == "call" {
		target := e.getCallTarget(node, src)
		switch target {
		case "defmodule":
			e.handleDefmodule(node, src, filePath, fileID, result, seen)
			return // handleDefmodule recurses into the body
		case "def", "defp":
			e.handleDef(node, src, filePath, fileID, currentModule, target == "defp", result, seen)
			return
		case "import", "alias", "use", "require":
			e.handleImport(node, src, filePath, fileID, target, result)
		}
	}

	// Handle module attributes: @attr value
	if node.Type() == "unary_operator" {
		e.handleAttribute(node, src, filePath, fileID, currentModule, result, seen)
	}

	// Recurse into children.
	for i, _nc := 0, int(node.ChildCount()); i < _nc; i++ {
		child := node.Child(i)
		e.walkNode(child, src, filePath, fileID, currentModule, result, seen)
	}
}

// getCallTarget returns the identifier name of a call's target, or "".
func (e *ElixirExtractor) getCallTarget(callNode *sitter.Node, src []byte) string {
	for i, _nc := 0, int(callNode.ChildCount()); i < _nc; i++ {
		child := callNode.Child(i)
		if callNode.FieldNameForChild(i) == "target" && child.Type() == "identifier" {
			return child.Content(src)
		}
	}
	return ""
}

// handleDefmodule extracts a module node and recurses into its body.
func (e *ElixirExtractor) handleDefmodule(callNode *sitter.Node, src []byte, filePath, fileID string, result *parser.ExtractionResult, seen map[string]bool) {
	modName := e.extractModuleName(callNode, src)
	if modName == "" {
		return
	}

	id := filePath + "::" + modName
	if seen[id] {
		return
	}
	seen[id] = true

	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindType, Name: modName,
		FilePath: filePath, StartLine: int(callNode.StartPoint().Row) + 1,
		EndLine: int(callNode.EndPoint().Row) + 1, Language: "elixir",
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines,
		FilePath: filePath, Line: int(callNode.StartPoint().Row) + 1,
	})

	// Walk children with module context so functions become methods.
	body := e.findDoBlock(callNode)
	if body != nil {
		for i, _nc := 0, int(body.ChildCount()); i < _nc; i++ {
			e.walkNode(body.Child(i), src, filePath, fileID, modName, result, seen)
		}
		// Phoenix plug dispatch: `plug :name` (optionally with
		// `when action in [...]`) declares a middleware that fires
		// before each action. Emit one EdgeCalls per (action, plug)
		// pair after the body walk so defs are already registered.
		e.emitPhoenixPlugBindings(body, src, filePath, modName, result)
		// Ecto model attribution: `schema "name" do ... end` macro
		// → EdgeModelsTable to a synthetic KindTable node.
		detectEcto(body, src, id, modName, filePath, result)
		// HEEx component attribution: `~H"""..."""` sigils render
		// uppercase-first-letter or dot-prefixed components.
		emitElixirHEExEdges(id, body, src, filePath, result)
	}
}

// handleDef extracts a function or method node, then collects the
// call sites inside its body in the same walk pass.
func (e *ElixirExtractor) handleDef(callNode *sitter.Node, src []byte, filePath, fileID, currentModule string, isPrivate bool, result *parser.ExtractionResult, seen map[string]bool) {
	funcName := e.extractFuncName(callNode, src)
	if funcName == "" {
		return
	}

	startLine := int(callNode.StartPoint().Row) + 1
	endLine := int(callNode.EndPoint().Row) + 1

	var id string
	if currentModule != "" {
		// Function inside a module -> method with MemberOf edge.
		id = filePath + "::" + currentModule + "." + funcName
		if seen[id] {
			return
		}
		seen[id] = true

		meta := map[string]any{
			"receiver":  currentModule,
			"signature": "def " + funcName + "(...)",
		}
		if isPrivate {
			meta["visibility"] = "private"
			meta["signature"] = "defp " + funcName + "(...)"
		}

		result.Nodes = append(result.Nodes, &graph.Node{
			ID: id, Kind: graph.KindMethod, Name: funcName,
			FilePath: filePath, StartLine: startLine, EndLine: endLine,
			Language: "elixir", Meta: meta,
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileID, To: id, Kind: graph.EdgeDefines,
			FilePath: filePath, Line: startLine,
		})
		typeID := filePath + "::" + currentModule
		result.Edges = append(result.Edges, &graph.Edge{
			From: id, To: typeID, Kind: graph.EdgeMemberOf,
			FilePath: filePath, Line: startLine,
		})
	} else {
		// Top-level function.
		id = filePath + "::" + funcName
		if seen[id] {
			return
		}
		seen[id] = true

		meta := map[string]any{"signature": "def " + funcName + "(...)"}
		if isPrivate {
			meta["visibility"] = "private"
			meta["signature"] = "defp " + funcName + "(...)"
		}

		result.Nodes = append(result.Nodes, &graph.Node{
			ID: id, Kind: graph.KindFunction, Name: funcName,
			FilePath: filePath, StartLine: startLine, EndLine: endLine,
			Language: "elixir", Meta: meta,
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileID, To: id, Kind: graph.EdgeDefines,
			FilePath: filePath, Line: startLine,
		})
	}

	// Call sites inside the body are attributed to this def directly —
	// exact attribution without a line-range lookup. walkNode does not
	// descend into def bodies, so the body region is scanned here and
	// every other region by walkNode: one cursor over the whole tree.
	if body := e.findDoBlock(callNode); body != nil {
		e.collectCalls(body, src, filePath, id, result)
	}
}

// handleImport extracts import/alias/use/require edges.
func (e *ElixirExtractor) handleImport(callNode *sitter.Node, src []byte, filePath, fileID, keyword string, result *parser.ExtractionResult) {
	modName := e.extractFirstArgText(callNode, src)
	if modName == "" {
		return
	}
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: "unresolved::import::" + modName,
		Kind: graph.EdgeImports, FilePath: filePath,
		Line: int(callNode.StartPoint().Row) + 1,
	})
}

// handleAttribute extracts module attributes (@attr value) as variables.
func (e *ElixirExtractor) handleAttribute(node *sitter.Node, src []byte, filePath, fileID, currentModule string, result *parser.ExtractionResult, seen map[string]bool) {
	if node.Type() != "unary_operator" {
		return
	}
	// Check if operator is "@".
	opText := ""
	for i, _nc := 0, int(node.ChildCount()); i < _nc; i++ {
		child := node.Child(i)
		if child.Type() == "@" || (node.FieldNameForChild(i) == "operator" && child.Content(src) == "@") {
			opText = "@"
			break
		}
	}
	if opText != "@" {
		return
	}

	// The operand is typically a call node with the attribute name as target.
	attrName := ""
	for i, _nc := 0, int(node.ChildCount()); i < _nc; i++ {
		child := node.Child(i)
		fieldName := node.FieldNameForChild(i)
		if fieldName == "operand" {
			if child.Type() == "call" {
				attrName = e.getCallTarget(child, src)
			} else if child.Type() == "identifier" {
				attrName = child.Content(src)
			}
			break
		}
	}
	if attrName == "" || attrName == "doc" || attrName == "moduledoc" || attrName == "spec" || attrName == "type" || attrName == "typep" || attrName == "callback" || attrName == "behaviour" || attrName == "behavior" {
		return
	}

	prefix := filePath + "::"
	if currentModule != "" {
		prefix = filePath + "::" + currentModule + "."
	}
	id := prefix + "@" + attrName
	if seen[id] {
		return
	}
	seen[id] = true

	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindVariable, Name: "@" + attrName,
		FilePath: filePath, StartLine: int(node.StartPoint().Row) + 1,
		EndLine: int(node.EndPoint().Row) + 1, Language: "elixir",
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines,
		FilePath: filePath, Line: int(node.StartPoint().Row) + 1,
	})
}

// collectCalls recursively scans a def body for call sites, emitting
// one EdgeCalls per non-keyword call. The caller is the enclosing def,
// passed in as callerID. Nested call nodes (`foo(bar())`) each emit
// their own edge, matching the previous whole-tree query behaviour.
func (e *ElixirExtractor) collectCalls(node *sitter.Node, src []byte, filePath, callerID string, result *parser.ExtractionResult) {
	if node == nil {
		return
	}
	if node.Type() == "call" {
		e.emitCallEdge(node, src, filePath, callerID, result)
	}
	for i, _nc := 0, int(node.ChildCount()); i < _nc; i++ {
		e.collectCalls(node.Child(i), src, filePath, callerID, result)
	}
}

// emitCallEdge inspects a call node's target and emits the appropriate
// EdgeCalls. A `dot` target is a qualified call (Module.fun ->
// unresolved::*.fun); a plain identifier target is a local call
// (unresolved::name), filtered against elixirKeywords / do / end.
func (e *ElixirExtractor) emitCallEdge(callNode *sitter.Node, src []byte, filePath, callerID string, result *parser.ExtractionResult) {
	line := int(callNode.StartPoint().Row) + 1
	for i, _nc := 0, int(callNode.ChildCount()); i < _nc; i++ {
		child := callNode.Child(i)
		if child == nil || callNode.FieldNameForChild(i) != "target" {
			continue
		}
		switch child.Type() {
		case "dot":
			method := e.dotCallMethod(child, src)
			if method == "" {
				return
			}
			result.Edges = append(result.Edges, &graph.Edge{
				From: callerID, To: "unresolved::*." + method,
				Kind: graph.EdgeCalls, FilePath: filePath, Line: line,
			})
		case "identifier":
			name := child.Content(src)
			if elixirKeywords[name] || name == "do" || name == "end" {
				return
			}
			result.Edges = append(result.Edges, &graph.Edge{
				From: callerID, To: "unresolved::" + name,
				Kind: graph.EdgeCalls, FilePath: filePath, Line: line,
			})
		}
		return
	}
}

// dotCallMethod returns the right-hand identifier of a `dot` node
// (the method name in `Module.method`), or "" when absent.
func (e *ElixirExtractor) dotCallMethod(dotNode *sitter.Node, src []byte) string {
	for i, _nc := 0, int(dotNode.ChildCount()); i < _nc; i++ {
		child := dotNode.Child(i)
		if child != nil && dotNode.FieldNameForChild(i) == "right" && child.Type() == "identifier" {
			return child.Content(src)
		}
	}
	return ""
}

// --- AST helpers ---

// extractModuleName gets the module name from a defmodule call node.
func (e *ElixirExtractor) extractModuleName(callNode *sitter.Node, src []byte) string {
	// Look for (arguments (alias) @name) or just the first argument text.
	args := e.findArguments(callNode)
	if args == nil {
		return ""
	}
	for i, _nc := 0, int(args.NamedChildCount()); i < _nc; i++ {
		child := args.NamedChild(i)
		t := child.Type()
		if t == "alias" || t == "dot" {
			return child.Content(src)
		}
	}
	// Fallback: first named child.
	if args.NamedChildCount() > 0 {
		text := args.NamedChild(0).Content(src)
		text = strings.TrimSpace(text)
		if text != "" && text != "do" {
			return text
		}
	}
	return ""
}

// extractFuncName gets the function name from a def/defp call node.
// The first argument of def is itself a call node whose target is the function name.
func (e *ElixirExtractor) extractFuncName(callNode *sitter.Node, src []byte) string {
	args := e.findArguments(callNode)
	if args == nil {
		return ""
	}
	for i, _nc := 0, int(args.NamedChildCount()); i < _nc; i++ {
		child := args.NamedChild(i)
		if child.Type() == "call" {
			// def func_name(args) -> call target is func_name
			return e.getCallTarget(child, src)
		}
		if child.Type() == "identifier" {
			// def func_name (no args)
			return child.Content(src)
		}
		if child.Type() == "binary_operator" {
			// Pattern: def func_name(args) when guard -> binary_operator with "when"
			// The left side should be the call with the function name.
			for j, _nc := 0, int(child.NamedChildCount()); j < _nc; j++ {
				sub := child.NamedChild(j)
				if sub.Type() == "call" {
					name := e.getCallTarget(sub, src)
					if name != "" {
						return name
					}
				}
			}
		}
	}
	return ""
}

// extractFirstArgText gets the text of the first argument (for import/alias/use/require).
func (e *ElixirExtractor) extractFirstArgText(callNode *sitter.Node, src []byte) string {
	args := e.findArguments(callNode)
	if args == nil {
		return ""
	}
	if args.NamedChildCount() > 0 {
		child := args.NamedChild(0)
		text := child.Content(src)
		text = strings.TrimSpace(text)
		return text
	}
	return ""
}

// findArguments locates the arguments node within a call node.
// In Elixir's tree-sitter grammar, the arguments node has no field name,
// so we find it by its node type.
func (e *ElixirExtractor) findArguments(callNode *sitter.Node) *sitter.Node {
	for i, _nc := 0, int(callNode.ChildCount()); i < _nc; i++ {
		child := callNode.Child(i)
		if child.Type() == "arguments" {
			return child
		}
	}
	return nil
}

// findDoBlock locates the do-block body within a call node.
func (e *ElixirExtractor) findDoBlock(callNode *sitter.Node) *sitter.Node {
	for i, _nc := 0, int(callNode.ChildCount()); i < _nc; i++ {
		child := callNode.Child(i)
		if child.Type() == "do_block" {
			return child
		}
	}
	// Also check inside arguments for inline do blocks.
	args := e.findArguments(callNode)
	if args != nil {
		for i, _nc := 0, int(args.ChildCount()); i < _nc; i++ {
			child := args.Child(i)
			if child.Type() == "do_block" {
				return child
			}
		}
	}
	return nil
}

// emitPhoenixPlugBindings walks the body of a defmodule for
// `plug :name` / `plug :name when action in [...]` macro calls and
// emits synthetic EdgeCalls from each matching action function to
// the named plug function. Phoenix dispatches plugs via module
// metadata so there's no explicit call site in source — the edges
// make `callers:plug_name` return the guarded actions.
func (e *ElixirExtractor) emitPhoenixPlugBindings(body *sitter.Node, src []byte, filePath, modName string, result *parser.ExtractionResult) {
	type plugEntry struct {
		name   string
		line   int
		filter map[string]struct{} // empty = applies to all actions
	}
	var plugs []plugEntry
	actions := make(map[string]int) // name → start line
	allPlugs := make(map[string]struct{})

	for i, _nc := 0, int(body.ChildCount()); i < _nc; i++ {
		c := body.Child(i)
		if c == nil || c.Type() != "call" {
			continue
		}
		target := e.getCallTarget(c, src)
		switch target {
		case "plug":
			entry := parsePhoenixPlugCall(c, src)
			if entry.name == "" {
				continue
			}
			plugs = append(plugs, plugEntry{
				name:   entry.name,
				line:   int(c.StartPoint().Row) + 1,
				filter: entry.filter,
			})
			allPlugs[entry.name] = struct{}{}
		case "def":
			if name := e.extractFuncName(c, src); name != "" {
				actions[name] = int(c.StartPoint().Row) + 1
			}
		}
	}
	if len(plugs) == 0 {
		return
	}
	for _, p := range plugs {
		plugID := filePath + "::" + modName + "." + p.name
		for action := range actions {
			// Plug functions themselves aren't actions — don't guard
			// them with other plugs.
			if _, isPlug := allPlugs[action]; isPlug {
				continue
			}
			if len(p.filter) > 0 {
				if _, ok := p.filter[action]; !ok {
					continue
				}
			}
			actionID := filePath + "::" + modName + "." + action
			result.Edges = append(result.Edges, &graph.Edge{
				From:     actionID,
				To:       plugID,
				Kind:     graph.EdgeCalls,
				FilePath: filePath,
				Line:     p.line,
				Meta: map[string]any{
					"dispatch_macro": "plug",
					"phoenix_plug":   p.name,
				},
			})
		}
	}
}

// parsePhoenixPlugCall extracts the plug function name and an optional
// set of action names (from `when action in [:a, :b, :c]`) from a plug
// call node. Returns zero values when the call doesn't parse.
type phoenixPlugParsed struct {
	name   string
	filter map[string]struct{}
}

func parsePhoenixPlugCall(callNode *sitter.Node, src []byte) phoenixPlugParsed {
	var out phoenixPlugParsed
	var args *sitter.Node
	for i, _nc := 0, int(callNode.NamedChildCount()); i < _nc; i++ {
		c := callNode.NamedChild(i)
		if c != nil && c.Type() == "arguments" {
			args = c
			break
		}
	}
	if args == nil || args.NamedChildCount() == 0 {
		return out
	}
	arg := args.NamedChild(0)
	switch arg.Type() {
	case "atom":
		out.name = strings.TrimPrefix(arg.Content(src), ":")
	case "binary_operator":
		// `:name when action in [...]` — the outer op is `when`,
		// left is the plug atom, right is an `in` expression whose
		// right side is a list of atoms.
		left := arg.NamedChild(0)
		right := arg.NamedChild(1)
		if left == nil || left.Type() != "atom" || right == nil {
			return out
		}
		out.name = strings.TrimPrefix(left.Content(src), ":")
		if right.Type() == "binary_operator" {
			list := right.NamedChild(1)
			if list != nil && list.Type() == "list" {
				out.filter = make(map[string]struct{})
				for i, _nc := 0, int(list.NamedChildCount()); i < _nc; i++ {
					item := list.NamedChild(i)
					if item != nil && item.Type() == "atom" {
						out.filter[strings.TrimPrefix(item.Content(src), ":")] = struct{}{}
					}
				}
			}
		}
	}
	return out
}
