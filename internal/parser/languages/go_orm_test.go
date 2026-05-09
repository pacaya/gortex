package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

func extractGoORM(t *testing.T, src string) *extractedFixture {
	t.Helper()
	return runGoExtract(t, src)
}

func TestGoORM_GormStructWithTags(t *testing.T) {
	res := extractGoORM(t, `package model

type User struct {
	ID    uint   `+"`gorm:\"primaryKey\"`"+`
	Email string `+"`gorm:\"uniqueIndex\"`"+`
}
`)
	models := res.edgesByKind[graph.EdgeModelsTable]
	require.Len(t, models, 1, "expected one EdgeModelsTable for gorm-tagged struct")
	assert.Equal(t, "pkg/foo.go::User", models[0].From)
	assert.Equal(t, "db::orm::users", models[0].To)
	assert.Equal(t, graph.OriginASTResolved, models[0].Origin)
	require.NotNil(t, models[0].Meta)
	assert.Equal(t, "gorm", models[0].Meta["orm"])
	assert.Equal(t, "users", models[0].Meta["table_name"])
	assert.Equal(t, "convention", models[0].Meta["derivation"])

	// KindTable node must materialise.
	tableNode := res.nodesByID["db::orm::users"]
	require.NotNil(t, tableNode, "users table node must be created")
	assert.Equal(t, graph.KindTable, tableNode.Kind)
	assert.Equal(t, "orm", tableNode.Meta["dialect"])
}

func TestGoORM_GormEmbedded(t *testing.T) {
	res := extractGoORM(t, `package model

import "gorm.io/gorm"

type Order struct {
	gorm.Model
	Total float64
}
`)
	models := res.edgesByKind[graph.EdgeModelsTable]
	require.Len(t, models, 1, "embedded gorm.Model must produce a model edge")
	assert.Equal(t, "db::orm::orders", models[0].To)
	assert.Equal(t, "gorm-embed", models[0].Meta["binding"])
}

func TestGoORM_TableNameOverride(t *testing.T) {
	res := extractGoORM(t, `package model

type Customer struct {
	ID   uint   `+"`gorm:\"primaryKey\"`"+`
	Name string
}

func (Customer) TableName() string { return "buyers" }
`)
	models := res.edgesByKind[graph.EdgeModelsTable]
	require.Len(t, models, 1)
	assert.Equal(t, "db::orm::buyers", models[0].To, "TableName() override must rewrite the convention default")
	assert.Equal(t, "buyers", models[0].Meta["table_name"])
	assert.Equal(t, "override", models[0].Meta["derivation"])

	// The explicit table node must exist.
	foundExplicit := res.nodesByID["db::orm::buyers"]
	require.NotNil(t, foundExplicit, "explicit table node must be materialised on override")
	assert.Equal(t, graph.KindTable, foundExplicit.Kind)
}

func TestGoORM_NonModelStructIgnored(t *testing.T) {
	res := extractGoORM(t, `package model

type Config struct {
	Host string
	Port int
}
`)
	models := res.edgesByKind[graph.EdgeModelsTable]
	assert.Empty(t, models, "structs without ORM signals must not produce model edges")
}

func TestGoORM_PluralizationCases(t *testing.T) {
	cases := map[string]string{
		"User":           "users",          // plain
		"OrderLine":      "order_lines",    // camel split
		"HTTPHandler":    "http_handlers",  // acronym
		"Address":        "addresses",      // ends in s — gets es
		"Box":            "boxes",          // ends in x
		"Quiz":           "quizes",         // ends in z — simple rule appends "es" (use TableName() override for irregulars like quizzes)
		"Story":          "stories",        // consonant+y
		"Day":            "days",           // vowel+y
		"Branch":         "branches",       // ends in ch
		"Dish":           "dishes",         // ends in sh
	}
	for input, want := range cases {
		got := defaultGormTableName(input)
		assert.Equal(t, want, got, "defaultGormTableName(%q)", input)
	}
}

func TestCamelToSnake(t *testing.T) {
	cases := map[string]string{
		"":             "",
		"User":         "user",
		"OrderLine":    "order_line",
		"HTTPHandler":  "http_handler",
		"APIKey":       "api_key",
		"orderLine":    "order_line",
		"ID":           "id",
		"VAT":          "vat",
	}
	for input, want := range cases {
		got := camelToSnake(input)
		assert.Equal(t, want, got, "camelToSnake(%q)", input)
	}
}
