package languages

import (
	"regexp"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// detectTypeScriptORMModel inspects a TS/JS class for TypeORM
// persistence decorators (@Entity, optional name argument) and emits
// an EdgeModelsTable to a synthetic KindTable node.
//
// TypeORM exposes two name-resolution shapes:
//   - @Entity("orders") — positional string literal
//   - @Entity({ name: "orders" }) — options object
//
// When neither is present, falls back to the gorm-style snake_case+
// plural default — kept consistent with the Go / Python / Java
// extractors so analyze tools can answer the same question with one
// rule.
func detectTypeScriptORMModel(classNode *sitter.Node, src []byte, classID, className, filePath string, result *parser.ExtractionResult) {
	if classNode == nil {
		return
	}
	tableName, source, hasEntity := tsEntityDecoratorTableName(classNode, src)
	if !hasEntity {
		return
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
			Language: "typescript",
			Meta: map[string]any{
				"dialect": "orm",
				"schema":  "",
				"source":  "typescript-orm",
			},
		})
	}
	startLine := int(classNode.StartPoint().Row) + 1
	meta := map[string]any{
		"orm":        "typeorm",
		"binding":    "decorator",
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

// tsEntityDecoratorTableName scans a class_declaration's decorator
// children for `@Entity(...)` and returns (tableName, sourceTag,
// hasEntity). hasEntity is true even when the decorator has no
// arguments — convention naming kicks in there.
func tsEntityDecoratorTableName(classNode *sitter.Node, src []byte) (string, string, bool) {
	for _, dec := range classDecorators(classNode) {
		call := nestDecoratorCall(dec)
		if call == nil {
			continue
		}
		fn := call.ChildByFieldName("function")
		if fn == nil || fn.Type() != "identifier" || fn.Content(src) != "Entity" {
			continue
		}
		argsNode := call.ChildByFieldName("arguments")
		if argsNode == nil {
			return "", "", true
		}
		// Look at the first argument: string literal or object.
		for i := 0; i < int(argsNode.NamedChildCount()); i++ {
			arg := argsNode.NamedChild(i)
			if arg == nil {
				continue
			}
			switch arg.Type() {
			case "string":
				return tsEntityStringArg(arg, src), "@Entity(name)", true
			case "object":
				if name := tsObjectStringField(arg, src, "name"); name != "" {
					return name, "@Entity({name})", true
				}
				return "", "", true
			}
		}
		return "", "", true
	}
	return "", "", false
}

// tsEntityStringArg unwraps a TS string literal to its content. Falls
// back to stripping surrounding quotes when no fragment child exists.
func tsEntityStringArg(node *sitter.Node, src []byte) string {
	for i := 0; i < int(node.NamedChildCount()); i++ {
		c := node.NamedChild(i)
		if c != nil && c.Type() == "string_fragment" {
			return c.Content(src)
		}
	}
	raw := node.Content(src)
	return strings.Trim(raw, "\"'`")
}

// tsObjectStringField returns the string value of an object literal's
// `<key>: "..."` pair, or "" when absent. Used to extract @Entity({
// name: "..." }) and similar options-object shapes.
func tsObjectStringField(obj *sitter.Node, src []byte, key string) string {
	keyRE := regexp.MustCompile(`^\s*` + regexp.QuoteMeta(key) + `\s*:\s*$`)
	for i := 0; i < int(obj.NamedChildCount()); i++ {
		pair := obj.NamedChild(i)
		if pair == nil || (pair.Type() != "pair" && pair.Type() != "property_name_value_pair") {
			continue
		}
		keyNode := pair.ChildByFieldName("key")
		valueNode := pair.ChildByFieldName("value")
		if keyNode == nil || valueNode == nil {
			continue
		}
		// Some grammars name the field-pair "name" differently; be
		// defensive and match by content.
		keyText := strings.TrimSpace(keyNode.Content(src))
		if keyText != key && !keyRE.MatchString(keyText+":") {
			continue
		}
		if valueNode.Type() == "string" {
			return tsEntityStringArg(valueNode, src)
		}
	}
	return ""
}

// classDecorators / nestDecoratorCall are reused from typescript.go so
// we don't duplicate them here. Both live in the same package, so the
// references resolve at compile time. Adding `var _ = ...` lines would
// only be needed for cross-package use.
//
// The two helper names this file relies on:
//   - classDecorators(classNode) → []*sitter.Node    (typescript.go)
//   - nestDecoratorCall(dec) → *sitter.Node          (typescript.go)
//
// Both already handle the experimental and stage-3 decorator AST
// variants TypeORM users compile with.
var _ = parser.ParseFile
