package languages

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// detectPythonORMModel inspects a Python class for ORM signals
// (SQLAlchemy __tablename__, Django Meta.db_table, base-class
// inheritance from Base / db.Model / models.Model) and emits an
// EdgeModelsTable to a synthetic KindTable node when one is found.
//
// Resolution order for the table name:
//  1. Explicit `__tablename__ = "..."` (SQLAlchemy)
//  2. `class Meta: db_table = "..."` (Django)
//  3. Inferred via SQLAlchemy/Django defaults from the class name
//     (snake_case + plural). Same convention gorm uses, kept consistent
//     across language extractors.
//
// classNode is the tree-sitter `class_definition` node.
func detectPythonORMModel(classNode *sitter.Node, src []byte, classID, className, filePath string, result *parser.ExtractionResult) {
	if classNode == nil {
		return
	}
	if pyClassLooksLikeAbstractBase(className) {
		// `class Base(DeclarativeBase): pass` and similar abstract
		// markers — they're scaffolding, not models. Filtering by
		// class name rather than body shape keeps the detector
		// lexical (no semantic analysis required) and matches the
		// universal SQLAlchemy convention of naming the marker
		// `Base` or `Model`.
		return
	}
	bases := pyClassBaseNames(classNode, src)
	if !pyClassLooksLikeORM(bases) {
		return
	}
	body := classNode.ChildByFieldName("body")
	if body == nil {
		return
	}

	tableName, source := pyClassExplicitTableName(body, src)
	derivation := "convention"
	if tableName == "" {
		tableName = defaultGormTableName(className)
	} else {
		derivation = "override"
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
			Language: "python",
			Meta: map[string]any{
				"dialect": "orm",
				"schema":  "",
				"source":  "python-orm",
			},
		})
	}

	startLine := int(classNode.StartPoint().Row) + 1
	orm := pyORMFlavor(bases)
	meta := map[string]any{
		"orm":        orm,
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

// pyClassLooksLikeAbstractBase reports whether name is a conventional
// abstract base-class marker (Base / Model / DeclarativeBase /
// SQLModel) that should NOT be treated as a model itself, even
// though it might inherit from another ORM marker. Matches the
// universal SQLAlchemy / SQLModel naming convention.
func pyClassLooksLikeAbstractBase(name string) bool {
	switch name {
	case "Base", "Model", "DeclarativeBase", "SQLModel":
		return true
	}
	return false
}

// pyClassBaseNames returns the bare base-class identifiers from a
// class_definition's superclasses list. Strips `module.Base` to `Base`
// for the recognition heuristic.
func pyClassBaseNames(classNode *sitter.Node, src []byte) []string {
	supers := classNode.ChildByFieldName("superclasses")
	if supers == nil {
		return nil
	}
	var out []string
	for i := 0; i < int(supers.NamedChildCount()); i++ {
		c := supers.NamedChild(i)
		if c == nil {
			continue
		}
		text := strings.TrimSpace(c.Content(src))
		// Strip generic params and call args: `Base[T]` → `Base`,
		// `db.Model()` → `db.Model`.
		if i := strings.Index(text, "("); i > 0 {
			text = text[:i]
		}
		if i := strings.Index(text, "["); i > 0 {
			text = text[:i]
		}
		// Strip module qualifier: `db.Model` → `Model`,
		// `sqlalchemy.orm.DeclarativeBase` → `DeclarativeBase`.
		if i := strings.LastIndex(text, "."); i >= 0 {
			text = text[i+1:]
		}
		if text != "" {
			out = append(out, text)
		}
	}
	return out
}

// pyClassLooksLikeORM reports whether any of bases names a known ORM
// base. Covers SQLAlchemy (Base / DeclarativeBase / db.Model) and
// Django (models.Model). False positives ("MyBase") are accepted as a
// tradeoff for not having to parse the full module-import graph;
// the EdgeModelsTable on a non-ORM base is still useful when the
// codebase actually uses that base for persistence.
func pyClassLooksLikeORM(bases []string) bool {
	for _, b := range bases {
		switch b {
		case "Base", "DeclarativeBase", "Model", "db.Model", "models.Model":
			return true
		}
	}
	return false
}

// pyORMFlavor returns "sqlalchemy" or "django" based on the base-class
// names. Defaults to "sqlalchemy" when both signals are absent — the
// caller has already passed pyClassLooksLikeORM.
func pyORMFlavor(bases []string) string {
	for _, b := range bases {
		if b == "models.Model" || b == "Model" {
			return "django"
		}
	}
	return "sqlalchemy"
}

// pyClassExplicitTableName returns (name, source) where source names
// the attribute the table came from (`__tablename__` / `db_table`).
// Empty name when neither is set.
func pyClassExplicitTableName(body *sitter.Node, src []byte) (string, string) {
	for i := 0; i < int(body.NamedChildCount()); i++ {
		stmt := body.NamedChild(i)
		if stmt == nil {
			continue
		}
		// SQLAlchemy: `__tablename__ = "..."` at class scope.
		if name, ok := pyAssignmentTarget(stmt, src, "__tablename__"); ok {
			if lit, lok := pyAssignmentStringLiteral(stmt, src); lok {
				return lit, "__tablename__"
			}
			_ = name
		}
		// Django: `class Meta: db_table = "..."` nested class.
		if stmt.Type() == "class_definition" {
			nameNode := stmt.ChildByFieldName("name")
			if nameNode == nil || nameNode.Content(src) != "Meta" {
				continue
			}
			metaBody := stmt.ChildByFieldName("body")
			if metaBody == nil {
				continue
			}
			for j := 0; j < int(metaBody.NamedChildCount()); j++ {
				sub := metaBody.NamedChild(j)
				if sub == nil {
					continue
				}
				if _, ok := pyAssignmentTarget(sub, src, "db_table"); !ok {
					continue
				}
				if lit, lok := pyAssignmentStringLiteral(sub, src); lok {
					return lit, "Meta.db_table"
				}
			}
		}
	}
	return "", ""
}

// pyAssignmentTarget reports whether stmt is `<name> = ...` and returns
// the target identifier text. Returns ("", false) for non-assignments
// or assignments to a different name.
func pyAssignmentTarget(stmt *sitter.Node, src []byte, want string) (string, bool) {
	if stmt == nil {
		return "", false
	}
	// Tree-sitter Python wraps top-level assigns in expression_statement.
	target := stmt
	if stmt.Type() == "expression_statement" && stmt.NamedChildCount() > 0 {
		target = stmt.NamedChild(0)
	}
	if target == nil || target.Type() != "assignment" {
		return "", false
	}
	left := target.ChildByFieldName("left")
	if left == nil {
		return "", false
	}
	text := strings.TrimSpace(left.Content(src))
	if text == want {
		return text, true
	}
	return "", false
}

// pyAssignmentStringLiteral returns the string literal on the right-
// hand side of stmt (an expression_statement wrapping an assignment).
// Returns ("", false) when the RHS isn't a single string literal.
func pyAssignmentStringLiteral(stmt *sitter.Node, src []byte) (string, bool) {
	if stmt == nil {
		return "", false
	}
	target := stmt
	if stmt.Type() == "expression_statement" && stmt.NamedChildCount() > 0 {
		target = stmt.NamedChild(0)
	}
	if target == nil || target.Type() != "assignment" {
		return "", false
	}
	right := target.ChildByFieldName("right")
	if right == nil {
		return "", false
	}
	if right.Type() != "string" {
		return "", false
	}
	// Walk the string node to find the string_content child.
	var content string
	for i := 0; i < int(right.NamedChildCount()); i++ {
		c := right.NamedChild(i)
		if c != nil && c.Type() == "string_content" {
			content = c.Content(src)
			break
		}
	}
	if content == "" {
		// Fall back to the raw string — strip surrounding quotes.
		raw := right.Content(src)
		raw = strings.Trim(raw, "\"'")
		return raw, raw != ""
	}
	return content, true
}
