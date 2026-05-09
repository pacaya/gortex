package languages

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// detectRubyORMModel inspects a Ruby class for ActiveRecord
// inheritance (< ApplicationRecord or < ActiveRecord::Base) and emits
// an EdgeModelsTable to a synthetic KindTable node when one is found.
//
// Resolution:
//  1. `self.table_name = "..."` at class scope → explicit override.
//  2. ActiveRecord default (Rails inflection — same snake_case+plural
//     rule we use everywhere; mouse → mouses, child → childs is wrong
//     vs. Rails, but irregulars are rare and override-able via
//     self.table_name).
func detectRubyORMModel(classNode *sitter.Node, src []byte, classID, className, filePath string, result *parser.ExtractionResult) {
	if classNode == nil {
		return
	}
	if !rubyClassExtendsActiveRecord(classNode, src) {
		return
	}
	body := rubyClassBody(classNode)
	tableName := ""
	source := ""
	if body != nil {
		if t, src := rubyClassTableNameAssign(body, src); t != "" {
			tableName = t
			source = src
		}
	}
	derivation := "convention"
	if tableName != "" {
		derivation = "override"
	} else {
		tableName = defaultGormTableName(className)
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
			Language: "ruby",
			Meta: map[string]any{
				"dialect": "orm",
				"schema":  "",
				"source":  "ruby-orm",
			},
		})
	}
	startLine := int(classNode.StartPoint().Row) + 1
	meta := map[string]any{
		"orm":        "activerecord",
		"binding":    "subclass",
		"table_name": tableName,
		"derivation": derivation,
	}
	if source != "" {
		meta["source_attr"] = source
	}
	result.Edges = append(result.Edges, &graph.Edge{
		From:     classID,
		To:       tableID,
		Kind:     graph.EdgeModelsTable,
		FilePath: filePath,
		Line:     startLine,
		Origin:   graph.OriginASTResolved,
		Meta:     meta,
	})
}

// rubyClassExtendsActiveRecord checks the `superclass` field of a class
// node for `< ApplicationRecord` or `< ActiveRecord::Base`.
func rubyClassExtendsActiveRecord(classNode *sitter.Node, src []byte) bool {
	super := classNode.ChildByFieldName("superclass")
	if super == nil {
		return false
	}
	text := strings.TrimSpace(super.Content(src))
	// `superclass` content is ` < Foo::Bar`; strip the leading "<".
	text = strings.TrimSpace(strings.TrimPrefix(text, "<"))
	switch text {
	case "ApplicationRecord", "ActiveRecord::Base":
		return true
	}
	return false
}

// rubyClassBody returns the class's body_statement node, or nil when the
// class is empty or shaped unexpectedly.
func rubyClassBody(classNode *sitter.Node) *sitter.Node {
	for i := 0; i < int(classNode.NamedChildCount()); i++ {
		c := classNode.NamedChild(i)
		if c != nil && c.Type() == "body_statement" {
			return c
		}
	}
	return nil
}

// rubyClassTableNameAssign returns the literal of a top-level
// `self.table_name = "..."` assignment in body, or ("", "") when absent.
func rubyClassTableNameAssign(body *sitter.Node, src []byte) (string, string) {
	for i := 0; i < int(body.NamedChildCount()); i++ {
		stmt := body.NamedChild(i)
		if stmt == nil || stmt.Type() != "assignment" {
			continue
		}
		left := stmt.ChildByFieldName("left")
		right := stmt.ChildByFieldName("right")
		if left == nil || right == nil {
			continue
		}
		// Match `self.table_name`. tree-sitter Ruby exposes this as a
		// `call` node with receiver `self` and method `table_name`.
		if leftText := strings.TrimSpace(left.Content(src)); leftText != "self.table_name" {
			continue
		}
		if right.Type() == "string" {
			return rubyStringContent(right, src), "self.table_name"
		}
	}
	return "", ""
}

// rubyStringContent unwraps a tree-sitter ruby `string` node down to
// the literal characters. Strips surrounding quotes when no
// string_content child is present.
func rubyStringContent(node *sitter.Node, src []byte) string {
	for i := 0; i < int(node.NamedChildCount()); i++ {
		c := node.NamedChild(i)
		if c != nil && c.Type() == "string_content" {
			return c.Content(src)
		}
	}
	raw := node.Content(src)
	return strings.Trim(raw, "\"'")
}
