package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

func runPyExtractFixtureORM(t *testing.T, filePath, src string) *extractedFixture {
	t.Helper()
	ext := NewPythonExtractor()
	result, err := ext.Extract(filePath, []byte(src))
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	return foldFixture(result)
}

func runJavaExtractFixtureORM(t *testing.T, filePath, src string) *extractedFixture {
	t.Helper()
	ext := NewJavaExtractor()
	result, err := ext.Extract(filePath, []byte(src))
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	return foldFixture(result)
}

func runRubyExtractFixtureORM(t *testing.T, filePath, src string) *extractedFixture {
	t.Helper()
	ext := NewRubyExtractor()
	result, err := ext.Extract(filePath, []byte(src))
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	return foldFixture(result)
}

func TestPythonORM_SQLAlchemyExplicitTablename(t *testing.T) {
	src := `from sqlalchemy.orm import DeclarativeBase

class Base(DeclarativeBase):
    pass

class User(Base):
    __tablename__ = "users"
    id: int
`
	fix := runPyExtractFixtureORM(t, "models/user.py", src)
	models := fix.edgesByKind[graph.EdgeModelsTable]
	require.Len(t, models, 1, "User should produce a models_table edge")
	assert.Equal(t, "models/user.py::User", models[0].From)
	assert.Equal(t, "db::orm::users", models[0].To)
	assert.Equal(t, "sqlalchemy", models[0].Meta["orm"])
	assert.Equal(t, "override", models[0].Meta["derivation"])
}

func TestPythonORM_DjangoMetaDbTable(t *testing.T) {
	src := `from django.db import models

class Order(models.Model):
    class Meta:
        db_table = "orders_v2"
`
	fix := runPyExtractFixtureORM(t, "shop/models.py", src)
	models := fix.edgesByKind[graph.EdgeModelsTable]
	require.Len(t, models, 1, "Order should produce a models_table edge from Django Meta.db_table")
	assert.Equal(t, "db::orm::orders_v2", models[0].To)
	assert.Equal(t, "django", models[0].Meta["orm"])
}

func TestPythonORM_NonOrmClassIgnored(t *testing.T) {
	src := `class Plain:
    name = "x"
`
	fix := runPyExtractFixtureORM(t, "x.py", src)
	assert.Empty(t, fix.edgesByKind[graph.EdgeModelsTable])
}

func TestPythonORM_DefaultConventionPlural(t *testing.T) {
	src := `from sqlalchemy.orm import DeclarativeBase

class Base(DeclarativeBase):
    pass

class OrderLine(Base):
    pass
`
	fix := runPyExtractFixtureORM(t, "x.py", src)
	models := fix.edgesByKind[graph.EdgeModelsTable]
	require.Len(t, models, 1)
	assert.Equal(t, "db::orm::order_lines", models[0].To)
	assert.Equal(t, "convention", models[0].Meta["derivation"])
}

func TestJavaORM_EntityWithTableName(t *testing.T) {
	src := `package com.example;

@Entity
@Table(name = "customers")
public class Customer {
    private Long id;
}
`
	fix := runJavaExtractFixtureORM(t, "com/example/Customer.java", src)
	models := fix.edgesByKind[graph.EdgeModelsTable]
	require.Len(t, models, 1)
	assert.Equal(t, "db::orm::customers", models[0].To)
	assert.Equal(t, "jpa", models[0].Meta["orm"])
	assert.Equal(t, "override", models[0].Meta["derivation"])
	assert.Equal(t, "@Table(name)", models[0].Meta["source_attr"])
}

func TestJavaORM_BareEntityFallsBackToConvention(t *testing.T) {
	src := `package com.example;

@Entity
public class Order {
    private Long id;
}
`
	fix := runJavaExtractFixtureORM(t, "com/example/Order.java", src)
	models := fix.edgesByKind[graph.EdgeModelsTable]
	require.Len(t, models, 1)
	assert.Equal(t, "db::orm::orders", models[0].To)
	assert.Equal(t, "convention", models[0].Meta["derivation"])
}

func TestJavaORM_NonEntityIgnored(t *testing.T) {
	src := `package com.example;

public class Service {
    public void doIt() {}
}
`
	fix := runJavaExtractFixtureORM(t, "com/example/Service.java", src)
	assert.Empty(t, fix.edgesByKind[graph.EdgeModelsTable])
}

func TestRubyORM_ApplicationRecordSubclass(t *testing.T) {
	src := `class Customer < ApplicationRecord
end
`
	fix := runRubyExtractFixtureORM(t, "app/models/customer.rb", src)
	models := fix.edgesByKind[graph.EdgeModelsTable]
	require.Len(t, models, 1)
	assert.Equal(t, "db::orm::customers", models[0].To)
	assert.Equal(t, "activerecord", models[0].Meta["orm"])
	assert.Equal(t, "convention", models[0].Meta["derivation"])
}

func TestRubyORM_ActiveRecordBaseWithExplicitTableName(t *testing.T) {
	src := `class LegacyOrder < ActiveRecord::Base
  self.table_name = "tbl_orders_legacy"
end
`
	fix := runRubyExtractFixtureORM(t, "models/legacy.rb", src)
	models := fix.edgesByKind[graph.EdgeModelsTable]
	require.Len(t, models, 1)
	assert.Equal(t, "db::orm::tbl_orders_legacy", models[0].To)
	assert.Equal(t, "override", models[0].Meta["derivation"])
}

func TestRubyORM_NonActiveRecordIgnored(t *testing.T) {
	src := `class PlainStruct
end
`
	fix := runRubyExtractFixtureORM(t, "x.rb", src)
	assert.Empty(t, fix.edgesByKind[graph.EdgeModelsTable])
}

func TestTypeScriptORM_TypeORMEntityPositionalString(t *testing.T) {
	src := `import { Entity } from "typeorm";

@Entity("invoices")
export class Invoice {
  id!: number;
}
`
	fix := runTSExtractFixture(t, "src/Invoice.ts", src)
	models := fix.edgesByKind[graph.EdgeModelsTable]
	require.Len(t, models, 1)
	assert.Equal(t, "db::orm::invoices", models[0].To)
	assert.Equal(t, "typeorm", models[0].Meta["orm"])
	assert.Equal(t, "override", models[0].Meta["derivation"])
}

func TestTypeScriptORM_TypeORMEntityOptionsObject(t *testing.T) {
	src := `import { Entity } from "typeorm";

@Entity({ name: "users_v2" })
export class User {
  id!: number;
}
`
	fix := runTSExtractFixture(t, "src/User.ts", src)
	models := fix.edgesByKind[graph.EdgeModelsTable]
	require.Len(t, models, 1)
	assert.Equal(t, "db::orm::users_v2", models[0].To)
	assert.Equal(t, "@Entity({name})", models[0].Meta["source_attr"])
}

func TestTypeScriptORM_BareEntityFallsBackToConvention(t *testing.T) {
	src := `import { Entity } from "typeorm";

@Entity()
export class Customer {
  id!: number;
}
`
	fix := runTSExtractFixture(t, "src/Customer.ts", src)
	models := fix.edgesByKind[graph.EdgeModelsTable]
	require.Len(t, models, 1)
	assert.Equal(t, "db::orm::customers", models[0].To)
	assert.Equal(t, "convention", models[0].Meta["derivation"])
}

func TestTypeScriptORM_NonEntityIgnored(t *testing.T) {
	src := `export class Service {
  do() {}
}
`
	fix := runTSExtractFixture(t, "src/Service.ts", src)
	assert.Empty(t, fix.edgesByKind[graph.EdgeModelsTable])
}
