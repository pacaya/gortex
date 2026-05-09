package languages

import (
	"regexp"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// detectJavaORMModel inspects a Java class for JPA persistence
// annotations (@Entity / @Table) and emits an EdgeModelsTable to a
// synthetic KindTable node when one is found.
//
// Resolution:
//  1. @Table(name = "...") wins.
//  2. @Entity(name = "...") wins next (entity-name doubles as table-
//     name when @Table is absent).
//  3. Bare @Entity falls back to the gorm-style snake_case+plural
//     default (kept consistent with the Go and Python extractors so
//     analyze tools can answer the same question with one rule).
func detectJavaORMModel(classNode *sitter.Node, src []byte, classID, className, filePath string) []*graph.Edge {
	if classNode == nil {
		return nil
	}
	annotations := javaCollectAnnotations(classNode, src)
	hasEntity := false
	tableName := ""
	source := ""
	for _, ann := range annotations {
		switch ann.name {
		case "Entity":
			hasEntity = true
			if name := javaAnnotationStringArg(ann.args, "name"); name != "" && tableName == "" {
				tableName = name
				source = "@Entity(name)"
			}
		case "Table":
			if name := javaAnnotationStringArg(ann.args, "name"); name != "" {
				tableName = name
				source = "@Table(name)"
			}
		}
	}
	if !hasEntity {
		return nil
	}
	derivation := "convention"
	if tableName != "" {
		derivation = "override"
	} else {
		tableName = defaultGormTableName(className)
	}
	if tableName == "" {
		return nil
	}
	tableID := ormTableNodeID(tableName)
	startLine := int(classNode.StartPoint().Row) + 1
	out := []*graph.Edge{
		{
			From:     classID,
			To:       tableID,
			Kind:     graph.EdgeModelsTable,
			FilePath: filePath,
			Line:     startLine,
			Origin:   graph.OriginASTResolved,
			Meta: map[string]any{
				"orm":         "jpa",
				"binding":     "annotation",
				"table_name":  tableName,
				"derivation":  derivation,
				"source_attr": source,
			},
		},
	}
	return out
}

// javaAnnotationStringArg parses the inside-the-parens text of a Java
// annotation looking for a `key = "value"` pair. Returns the value
// (with quotes stripped) or "".
//
// The annotation arg surface is small in practice — JPA's @Entity and
// @Table take simple key=value lists — so a regex is enough; a full
// parser would buy nothing.
func javaAnnotationStringArg(args, key string) string {
	if args == "" || key == "" {
		return ""
	}
	re := regexp.MustCompile(regexp.QuoteMeta(key) + `\s*=\s*"([^"]*)"`)
	m := re.FindStringSubmatch(args)
	if len(m) < 2 {
		return ""
	}
	return strings.TrimSpace(m[1])
}

// emitJavaORMEdges materialises the KindTable node + EdgeModelsTable
// edges for a Java class. Wraps detectJavaORMModel so the per-call
// dedup against a class's tables happens in one place.
func emitJavaORMEdges(classNode *sitter.Node, src []byte, classID, className, filePath string, result *parser.ExtractionResult) {
	edges := detectJavaORMModel(classNode, src, classID, className, filePath)
	for _, e := range edges {
		if e == nil {
			continue
		}
		if !ormTableNodeAlreadyEmitted(result, e.To) {
			tableName := e.Meta["table_name"].(string)
			result.Nodes = append(result.Nodes, &graph.Node{
				ID:       e.To,
				Kind:     graph.KindTable,
				Name:     tableName,
				FilePath: filePath,
				Language: "java",
				Meta: map[string]any{
					"dialect": "orm",
					"schema":  "",
					"source":  "java-orm",
				},
			})
		}
		result.Edges = append(result.Edges, e)
	}
}
