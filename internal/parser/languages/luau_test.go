package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

const luauSample = `local Account = {}

function greet(name: string): string
	helper()
	return "hi " .. name
end

local function helper()
	return 42
end

function Account:deposit(amount: number)
	self.balance = self.balance + amount
end

export type Account = { balance: number }

type Id = number

type Pair<K, V> = { key: K, value: V }
`

func TestLuauExtractor_LanguageAndExtensions(t *testing.T) {
	e := NewLuauExtractor()
	assert.Equal(t, "luau", e.Language())
	assert.Equal(t, []string{".luau"}, e.Extensions())
}

func TestLuauExtractor_FunctionsAndMethods(t *testing.T) {
	e := NewLuauExtractor()
	result, err := e.Extract("account.luau", []byte(luauSample))
	require.NoError(t, err)

	funcs := nodesOfKind(result.Nodes, graph.KindFunction)
	fnNames := make([]string, len(funcs))
	for i, f := range funcs {
		fnNames[i] = f.Name
		assert.Equal(t, "luau", f.Language)
	}
	assert.Contains(t, fnNames, "greet")
	assert.Contains(t, fnNames, "helper")

	methods := nodesOfKind(result.Nodes, graph.KindMethod)
	require.GreaterOrEqual(t, len(methods), 1)
	var deposit *graph.Node
	for _, m := range methods {
		if m.Name == "deposit" {
			deposit = m
		}
	}
	require.NotNil(t, deposit, "expected method node for Account:deposit")
	assert.Equal(t, "Account", deposit.Meta["receiver"])

	// The method should be linked to its receiver type via EdgeMemberOf.
	memberEdges := edgesOfKind(result.Edges, graph.EdgeMemberOf)
	require.GreaterOrEqual(t, len(memberEdges), 1)
}

func TestLuauExtractor_TypeAliases(t *testing.T) {
	e := NewLuauExtractor()
	result, err := e.Extract("account.luau", []byte(luauSample))
	require.NoError(t, err)

	types := nodesOfKind(result.Nodes, graph.KindType)
	byName := map[string]*graph.Node{}
	for _, ty := range types {
		byName[ty.Name] = ty
		assert.Equal(t, "luau", ty.Language)
	}

	require.Contains(t, byName, "Account", "expected KindType for export type Account")
	require.Contains(t, byName, "Id", "expected KindType for type Id")
	require.Contains(t, byName, "Pair", "expected KindType for type Pair<K, V>")

	assert.Equal(t, true, byName["Account"].Meta["exported"],
		"export type Account must be marked exported=true")
	assert.Equal(t, false, byName["Id"].Meta["exported"],
		"plain type Id must be marked exported=false")
}

func TestLuauExtractor_GenericParams(t *testing.T) {
	e := NewLuauExtractor()
	result, err := e.Extract("account.luau", []byte(luauSample))
	require.NoError(t, err)

	gps := nodesOfKind(result.Nodes, graph.KindGenericParam)
	names := make([]string, len(gps))
	for i, g := range gps {
		names[i] = g.Name
	}
	// type Pair<K, V> declares two generic parameters.
	assert.Contains(t, names, "K")
	assert.Contains(t, names, "V")
}

func TestLuauExtractor_GreetCallsHelper(t *testing.T) {
	e := NewLuauExtractor()
	result, err := e.Extract("account.luau", []byte(luauSample))
	require.NoError(t, err)

	greetID := "account.luau::greet"
	helperTarget := "unresolved::helper"

	found := false
	for _, edge := range result.Edges {
		if edge.Kind == graph.EdgeCalls && edge.From == greetID && edge.To == helperTarget {
			found = true
			break
		}
	}
	assert.True(t, found, "expected greet to call helper (EdgeCalls %s -> %s)", greetID, helperTarget)
}

func TestLuauExtractor_TypeAnnotationReferences(t *testing.T) {
	e := NewLuauExtractor()
	src := []byte(`function transfer(from: Account, to: Account): boolean
	return true
end
`)
	result, err := e.Extract("bank.luau", src)
	require.NoError(t, err)

	transferID := "bank.luau::transfer"
	typedAs := 0
	for _, edge := range result.Edges {
		if edge.Kind == graph.EdgeTypedAs && edge.From == transferID && edge.To == "unresolved::Account" {
			typedAs++
		}
	}
	// `boolean` is a builtin and must NOT produce a reference edge.
	for _, edge := range result.Edges {
		if edge.Kind == graph.EdgeReferences && edge.To == "unresolved::boolean" {
			t.Fatalf("builtin return type boolean must not yield a reference edge")
		}
	}
	assert.GreaterOrEqual(t, typedAs, 1, "expected at least one EdgeTypedAs to named type Account")
}
