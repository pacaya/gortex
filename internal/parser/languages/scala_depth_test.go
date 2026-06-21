package languages

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestScalaExtractor_Fields(t *testing.T) {
	const scala = `package com.app

class Account {
  val balance: Int = 0
  var pending: List[Transaction] = Nil
  def deposit(amount: Int): Boolean = {
    true
  }
}
`
	res, err := NewScalaExtractor().Extract("Account.scala", []byte(scala))
	if err != nil {
		t.Fatal(err)
	}

	byName := map[string]*graph.Node{}
	for _, n := range res.Nodes {
		byName[n.Name] = n
	}
	typedAs := map[string]string{} // field id -> referenced type
	for _, e := range res.Edges {
		if e.Kind == graph.EdgeTypedAs {
			typedAs[e.From] = e.To
		}
	}

	// val field keeps its base type in meta, but a primitive annotation
	// (Int) deliberately emits no typed_as usage edge — those would just
	// clutter the graph with unresolved::Int edges that never land.
	balance := byName["balance"]
	if balance == nil || balance.Kind != graph.KindField {
		t.Fatalf("val 'balance' should be a field node, got %+v", balance)
	}
	if balance.Meta["field_type"] != "Int" {
		t.Errorf("balance field_type = %v, want Int", balance.Meta["field_type"])
	}
	if _, ok := typedAs[balance.ID]; ok {
		t.Errorf("primitive-typed balance should NOT have a typed_as edge, got %q", typedAs[balance.ID])
	}

	// A container generic surfaces its element type as a usage edge:
	// `List[Transaction]` references Transaction.
	if p := byName["pending"]; p != nil && typedAs[p.ID] != "unresolved::Transaction" {
		t.Errorf("pending (List[Transaction]) should typed_as unresolved::Transaction, got %q", typedAs[p.ID])
	}

	// var field is flagged mutable; generic type reduced to its base.
	pending := byName["pending"]
	if pending == nil || pending.Kind != graph.KindField {
		t.Fatalf("var 'pending' should be a field node, got %+v", pending)
	}
	if pending.Meta["mutable"] != true {
		t.Errorf("var pending should be mutable: meta=%v", pending.Meta)
	}
	if pending.Meta["field_type"] != "List" {
		t.Errorf("pending field_type = %v, want List (generic stripped)", pending.Meta["field_type"])
	}

	// function gains a return type.
	deposit := byName["deposit"]
	if deposit == nil {
		t.Fatal("method 'deposit' was not extracted")
	}
	if deposit.Meta["return_type"] != "Boolean" {
		t.Errorf("deposit return_type = %v, want Boolean", deposit.Meta["return_type"])
	}
}
