package languages

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// detectEcto walks a defmodule body for Ecto schema declarations and
// emits an EdgeModelsTable from the module to a synthetic KindTable
// node. Two shapes are recognised, mirroring Ecto's two scaffolding
// macros:
//
//   - schema "name" do …          (Ecto.Schema, the canonical case)
//   - embedded_schema do …        (Ecto.Schema embedded — no table; we
//     skip these because there's no DB
//     binding to surface)
//
// `use Ecto.Schema` and friends aren't required for detection — the
// presence of a `schema "..." do` macro is the load-bearing signal.
func detectEcto(body *sitter.Node, src []byte, modID, modName, filePath string, result *parser.ExtractionResult) {
	if body == nil {
		return
	}
	tableName, line, ok := elixirEctoSchemaCall(body, src)
	if !ok {
		return
	}
	if tableName == "" {
		return
	}
	tableID := ormTableNodeID(tableName)
	if !ormTableNodeAlreadyEmitted(result, tableID) {
		result.Nodes = append(result.Nodes, &graph.Node{
			ID:       tableID,
			Kind:     graph.KindTable,
			Name:     tableName,
			FilePath: filePath,
			Language: "elixir",
			Meta: map[string]any{
				"dialect": "orm",
				"schema":  "",
				"source":  "elixir-orm",
			},
		})
	}
	result.Edges = append(result.Edges, &graph.Edge{
		From:     modID,
		To:       tableID,
		Kind:     graph.EdgeModelsTable,
		FilePath: filePath,
		Line:     line,
		Origin:   graph.OriginASTResolved,
		Meta: map[string]any{
			"orm":         "ecto",
			"binding":     "schema-macro",
			"table_name":  tableName,
			"derivation":  "override",
			"source_attr": "schema/2",
		},
	})
}

// elixirEctoSchemaCall scans a do_block for `schema "<name>" do`
// macro calls and returns (tableName, lineNo, found). The Elixir
// tree-sitter grammar exposes a `schema "users" do … end` invocation
// as a `call` node whose target identifier is `schema` and whose
// first argument is a string literal — same shape every macro takes.
func elixirEctoSchemaCall(body *sitter.Node, src []byte) (string, int, bool) {
	var found string
	var foundLine int
	walkAST(body, func(n *sitter.Node) bool {
		if n == nil || found != "" {
			return false
		}
		if n.Type() != "call" {
			return true
		}
		target := n.ChildByFieldName("target")
		if target == nil {
			return true
		}
		name := strings.TrimSpace(target.Content(src))
		if name != "schema" {
			return true
		}
		// First arg of `schema/2` macro must be a string literal —
		// Ecto rejects anything else. Defensive: bail when shape
		// differs (e.g. dynamic schema name from a variable).
		args := elixirCallArgs(n)
		if args == nil {
			return true
		}
		first := elixirFirstArg(args)
		if first == nil {
			return true
		}
		if first.Type() != "string" {
			return true
		}
		lit := elixirStringLiteral(first, src)
		if lit == "" {
			return true
		}
		found = lit
		foundLine = int(n.StartPoint().Row) + 1
		return false
	})
	return found, foundLine, found != ""
}

// elixirCallArgs returns the `arguments` child of a call node, or nil
// when absent. Mirrors the tree-sitter-elixir convention.
func elixirCallArgs(call *sitter.Node) *sitter.Node {
	if call == nil {
		return nil
	}
	if a := call.ChildByFieldName("arguments"); a != nil {
		return a
	}
	for i, _nc := 0, int(call.NamedChildCount()); i < _nc; i++ {
		c := call.NamedChild(i)
		if c == nil {
			continue
		}
		if c.Type() == "arguments" {
			return c
		}
	}
	return nil
}

// elixirFirstArg returns the first non-do-block argument from an
// `arguments` node. Skips over `keywords`/`do_block` siblings the
// grammar may attach.
func elixirFirstArg(args *sitter.Node) *sitter.Node {
	if args == nil {
		return nil
	}
	for i, _nc := 0, int(args.NamedChildCount()); i < _nc; i++ {
		c := args.NamedChild(i)
		if c == nil {
			continue
		}
		switch c.Type() {
		case "do_block", "keywords":
			continue
		}
		return c
	}
	return nil
}

// elixirStringLiteral unwraps a tree-sitter-elixir `string` node to
// the literal characters. Strips quotes when no `quoted_content`
// child is present.
func elixirStringLiteral(node *sitter.Node, src []byte) string {
	for i, _nc := 0, int(node.NamedChildCount()); i < _nc; i++ {
		c := node.NamedChild(i)
		if c != nil && c.Type() == "quoted_content" {
			return c.Content(src)
		}
	}
	raw := node.Content(src)
	return strings.Trim(raw, "\"'")
}
